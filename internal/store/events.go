package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/paoloanzn/portico/internal/core"
)

// events table migration - append-only event journal for SSE replay.
const migrationEventsTable = `
CREATE TABLE IF NOT EXISTS events (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id  TEXT,
    connection_id TEXT,
    occurred_at   TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    stage         TEXT,
    payload_json  BLOB NOT NULL
);
`

// AppendEvent appends an event to the append-only event journal.
// Events are immutable and assigned a monotonically increasing sequence number.
func (s *Store) AppendEvent(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID,
	eventType string, stage string, payload []byte) (int64, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO events (operation_id, connection_id, occurred_at, event_type, stage, payload_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		operationID, connectionID, now, eventType, stage, payload)
	if err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	return result.LastInsertId()
}

// GetEventsSince returns events with sequence greater than afterSeq, ordered by seq.
// Used for SSE replay on reconnect.
func (s *Store) GetEventsSince(ctx context.Context, afterSeq int64, limit int) ([]core.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, operation_id, connection_id, occurred_at, event_type, stage, payload_json
		FROM events
		WHERE seq > ?
		ORDER BY seq
		LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []core.Event
	for rows.Next() {
		var evt core.Event
		var opID, connID, occurredAt, stage string
		var payload []byte
		var seq int64
		if err := rows.Scan(&seq, &opID, &connID, &occurredAt, &evt.Type, &stage, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		evt.Sequence = seq
		at, err := time.Parse(time.RFC3339, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse occurred_at: %w", err)
		}
		evt.Timestamp = at
		// Store stage in the Data field as part of OperationEvent
		if err := json.Unmarshal(payload, &evt.Data); err != nil {
			return nil, fmt.Errorf("unmarshal payload: %w", err)
		}
		events = append(events, evt)
	}
	return events, rows.Err()
}

// GetLastEventSeq returns the highest event sequence number.
// Used for snapshot high-water mark.
func (s *Store) GetLastEventSeq(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(seq) FROM events").Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("query last seq: %w", err)
	}
	return seq.Int64, nil
}
