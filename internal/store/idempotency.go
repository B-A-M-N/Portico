package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ErrIdempotencyKeyConflict means a key is already reserved for another plan,
// or its first execution is still in flight.
var ErrIdempotencyKeyConflict = errors.New("idempotency key conflict")

// ReserveIdempotentKey claims a key before an external mutation starts. The
// caller that gets reserved=true owns execution. A completed operation is
// returned for replay; an unbound reservation is an in-progress conflict.
func (s *Store) ReserveIdempotentKey(ctx context.Context, key string, planID core.PlanID) (reserved bool, operationID core.OperationID, err error) {
	if key == "" {
		return false, "", fmt.Errorf("idempotency key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("begin idempotency reservation: %w", err)
	}
	defer tx.Rollback()

	var existingPlan, existingOperation string
	err = tx.QueryRowContext(ctx,
		"SELECT plan_id, COALESCE(operation_id, '') FROM idempotency_keys WHERE key = ?", key,
	).Scan(&existingPlan, &existingOperation)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO idempotency_keys (key, plan_id, operation_id, created_at) VALUES (?, ?, NULL, ?)",
			key, string(planID), time.Now().UTC().Format(time.RFC3339)); err != nil {
			return false, "", fmt.Errorf("reserve idempotency key: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, "", fmt.Errorf("commit idempotency reservation: %w", err)
		}
		return true, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("lookup idempotency reservation: %w", err)
	}
	if existingPlan != string(planID) {
		return false, "", fmt.Errorf("%w: key belongs to plan %q", ErrIdempotencyKeyConflict, existingPlan)
	}
	if existingOperation == "" {
		return false, "", fmt.Errorf("%w: execution is still in progress", ErrIdempotencyKeyConflict)
	}
	return false, core.OperationID(existingOperation), nil
}
