package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// Idempotency reservation semantics.
//
// The old lookup-then-record pattern left the entire external mutation
// between the two steps: two concurrent callers with one key could both see
// "free" and both execute, and a record failure after success made a retry
// re-execute. ReserveIdempotentKey closes that window: the key is claimed
// transactionally before any mutation, bound to its plan, and reused for a
// different plan is a conflict rather than a mislabelled replay.

func openReservationStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/idem.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestReserveIdempotentKeyClaimsOnceForSamePlan(t *testing.T) {
	st := openReservationStore(t)
	ctx := context.Background()
	const key = "replay-key-1"
	plan := core.PlanID("plan-A")

	reserved, opID, err := st.ReserveIdempotentKey(ctx, key, plan)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if !reserved || opID != "" {
		t.Fatalf("first reserve = (reserved=%v, op=%q), want (true, \"\")", reserved, opID)
	}

	// Persist a real operation (the FK requires it), then bind the way
	// HandleApplyPlan does after execution completes.
	opIDReal := core.OperationID("op-real-1")
	// operations.plan_id references operation_plans: persist the plan first.
	if err := st.SavePlan(ctx, &core.OperationPlan{
		ID: plan, ConnectionID: "conn-1", ProfileRevision: 1,
		Provider: "mock", Intent: core.IntentOpen,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := st.SaveOperation(ctx, opIDReal, plan, "conn-1", "completed",
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	if err := st.RecordIdempotentKey(ctx, key, opIDReal); err != nil {
		t.Fatalf("RecordIdempotentKey: %v", err)
	}

	// Replay of the same key + same plan returns the earlier operation.
	reserved, opID, err = st.ReserveIdempotentKey(ctx, key, plan)
	if err != nil {
		t.Fatalf("replay reserve: %v", err)
	}
	if reserved {
		t.Fatal("a bound key was handed out again")
	}
	if opID != opIDReal {
		t.Fatalf("replay returned op %q, want %q", opID, opIDReal)
	}
}

func TestReserveIdempotentKeyConflictOnDifferentPlan(t *testing.T) {
	st := openReservationStore(t)
	ctx := context.Background()
	const key = "replay-key-2"

	if _, _, err := st.ReserveIdempotentKey(ctx, key, "plan-A"); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	_, _, err := st.ReserveIdempotentKey(ctx, key, "plan-B")
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("key reuse for another plan returned %v, want ErrIdempotencyKeyConflict", err)
	}
}

func TestReserveIdempotentKeyUnboundReservationIsInProgressConflict(t *testing.T) {
	st := openReservationStore(t)
	ctx := context.Background()
	const key = "replay-key-3"
	const plan = core.PlanID("plan-C")

	// Reserved but never bound: the first caller's execution is in flight.
	if _, _, err := st.ReserveIdempotentKey(ctx, key, plan); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A concurrent same-plan replay must NOT be handed the reservation — two
	// racing executions is exactly what idempotency exists to prevent. It is
	// refused as in-progress; retrying after completion returns the bound op.
	reserved, _, err := st.ReserveIdempotentKey(ctx, key, plan)
	if reserved {
		t.Fatal("an in-flight reservation was handed to a second caller")
	}
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("in-flight replay returned %v, want ErrIdempotencyKeyConflict", err)
	}
}

// TestReserveIdempotentKeyConcurrentExactlyOneWinner is the race the old
// lookup/record pattern lost: N goroutines racing one key must yield exactly
// one reservation; everyone else either replays or conflicts.
func TestReserveIdempotentKeyConcurrentExactlyOneWinner(t *testing.T) {
	st := openReservationStore(t)
	ctx := context.Background()
	const key = "race-key"
	const plan = core.PlanID("plan-race")

	const callers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserved, _, err := st.ReserveIdempotentKey(ctx, key, plan)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && reserved {
				winners++
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d concurrent callers produced %d winners, want exactly 1", callers, winners)
	}
}
