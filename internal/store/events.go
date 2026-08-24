package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
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

const (
	eventRetentionAge   = 30 * 24 * time.Hour
	eventRetentionCount = 100000
)

// DurableEvent is one authoritative event-journal row. Its indexing metadata
// is kept separate from arbitrary payload data so replay does not have to
// infer connection or operation identity from a map's concrete Go type.
type DurableEvent struct {
	Event        core.Event
	OperationID  core.OperationID
	ConnectionID core.ConnectionID
	Stage        string
}

// appendEventTx appends an already-normalized event to an existing state
// transaction. It is intentionally private: callers must use it only when
// the event and its state transition share the same SQLite transaction.
func appendEventTx(ctx context.Context, tx *sql.Tx, operationID core.OperationID, connectionID core.ConnectionID,
	eventType string, stage string, occurredAt time.Time, data any) (int64, error) {
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshal durable event payload: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO events (operation_id, connection_id, occurred_at, event_type, stage, payload_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		operationID, connectionID, occurredAt.UTC().Format(time.RFC3339), eventType, stage, payload)
	if err != nil {
		return 0, fmt.Errorf("append durable event: %w", err)
	}
	return result.LastInsertId()
}

// AppendEvent appends an event to the append-only event journal.
// Events are immutable and assigned a monotonically increasing sequence number.
func (s *Store) AppendEvent(ctx context.Context, operationID core.OperationID, connectionID core.ConnectionID,
	eventType string, stage string, occurredAt time.Time, payload []byte) (int64, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO events (operation_id, connection_id, occurred_at, event_type, stage, payload_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		operationID, connectionID, occurredAt.UTC().Format(time.RFC3339), eventType, stage, payload)
	if err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	if err := s.pruneEventsLocked(ctx); err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	return result.LastInsertId()
}

// pruneEventsLocked enforces the bounded event journal required for SSE
// replay. The caller must hold s.mu.
func (s *Store) pruneEventsLocked(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-eventRetentionAge).Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE occurred_at < ?", cutoff); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE seq <= COALESCE((
			SELECT seq FROM events ORDER BY seq DESC LIMIT 1 OFFSET ?
		), 0)`, eventRetentionCount)
	return err
}

// OldestEventSeq returns the earliest retained sequence, or zero when the
// journal is empty. SSE uses it to detect a replay cursor that has expired.
func (s *Store) OldestEventSeq(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(seq) FROM events").Scan(&seq); err != nil {
		return 0, fmt.Errorf("query oldest event seq: %w", err)
	}
	return seq.Int64, nil
}

// EventJournalHealth reports the size and span of the retained journal.
//
// Health checks need to distinguish an empty journal from one that has been
// trimmed: a client reconnecting with a cursor older than the oldest retained
// sequence cannot be replayed to, and a journal that has stopped being trimmed
// grows without bound. Both are answerable here and nowhere else, because the
// supervisor is the only component that opens the database.
func (s *Store) EventJournalHealth(ctx context.Context) (count int64, oldest int64, newest int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var oldestVal, newestVal sql.NullInt64
	row := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), MIN(seq), MAX(seq) FROM events")
	if err := row.Scan(&count, &oldestVal, &newestVal); err != nil {
		return 0, 0, 0, fmt.Errorf("query event journal health: %w", err)
	}
	return count, oldestVal.Int64, newestVal.Int64, nil
}

// EventRetentionLimit is the number of events the journal keeps, so a health
// check can say how close to the limit the journal is running rather than
// reporting a raw count nobody can interpret.
func EventRetentionLimit() int { return eventRetentionCount }

// GetEventsSince returns events with sequence greater than afterSeq, ordered by seq.
// Used for SSE replay on reconnect.
func (s *Store) GetEventsSince(ctx context.Context, afterSeq int64, limit int) ([]core.Event, error) {
	records, err := s.GetDurableEventsSince(ctx, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	events := make([]core.Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	return events, nil
}

// GetDurableEventsSince returns replay rows with their indexed metadata.
func (s *Store) GetDurableEventsSince(ctx context.Context, afterSeq int64, limit int) ([]DurableEvent, error) {
	return s.GetDurableEventsBetween(ctx, afterSeq, 0, limit)
}

// GetDurableEventsForOperation returns the authoritative, globally sequenced
// event history for one operation. Operation detail endpoints use this rather
// than the older operation_events projection so their cursors match SSE replay
// across supervisor restarts.
func (s *Store) GetDurableEventsForOperation(ctx context.Context, operationID core.OperationID) ([]DurableEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, operation_id, connection_id, occurred_at, event_type, stage, payload_json
		FROM events WHERE operation_id = ? ORDER BY seq`, operationID)
	if err != nil {
		return nil, fmt.Errorf("query operation events: %w", err)
	}
	defer rows.Close()
	var events []DurableEvent
	for rows.Next() {
		var record DurableEvent
		var opID, connID, occurredAt, stage string
		var payload []byte
		var seq int64
		if err := rows.Scan(&seq, &opID, &connID, &occurredAt, &record.Event.Type, &stage, &payload); err != nil {
			return nil, fmt.Errorf("scan operation event: %w", err)
		}
		record.Event.Sequence = seq
		record.OperationID = core.OperationID(opID)
		record.ConnectionID = core.ConnectionID(connID)
		record.Stage = stage
		at, err := time.Parse(time.RFC3339, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse operation event time: %w", err)
		}
		record.Event.Timestamp = at
		if err := json.Unmarshal(payload, &record.Event.Data); err != nil {
			return nil, fmt.Errorf("unmarshal operation event payload: %w", err)
		}
		events = append(events, record)
	}
	return events, rows.Err()
}

// GetDurableEventsBetween returns events in (afterSeq, throughSeq]. A zero
// throughSeq leaves the upper bound open. Replay captures a high-water mark
// then pages within that fixed range so concurrent publications are delivered
// through the live subscription rather than extending replay indefinitely.
func (s *Store) GetDurableEventsBetween(ctx context.Context, afterSeq, throughSeq int64, limit int) ([]DurableEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, operation_id, connection_id, occurred_at, event_type, stage, payload_json
		FROM events
		WHERE seq > ? AND (? = 0 OR seq <= ?)
		ORDER BY seq
		LIMIT ?`, afterSeq, throughSeq, throughSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []DurableEvent
	for rows.Next() {
		var record DurableEvent
		var opID, connID, occurredAt, stage string
		var payload []byte
		var seq int64
		if err := rows.Scan(&seq, &opID, &connID, &occurredAt, &record.Event.Type, &stage, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		record.Event.Sequence = seq
		record.OperationID = core.OperationID(opID)
		record.ConnectionID = core.ConnectionID(connID)
		record.Stage = stage
		at, err := time.Parse(time.RFC3339, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse occurred_at: %w", err)
		}
		record.Event.Timestamp = at
		if err := json.Unmarshal(payload, &record.Event.Data); err != nil {
			return nil, fmt.Errorf("unmarshal payload: %w", err)
		}
		events = append(events, record)
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
