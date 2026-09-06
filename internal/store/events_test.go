package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func TestEventRetentionUsesInjectedClock(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)}
	st, err := OpenWithClock(filepath.Join(t.TempDir(), "events.db"), clock)
	if err != nil {
		t.Fatalf("OpenWithClock: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, "op-old", "conn-1", "old", "", clock.Now().Add(-29*24*time.Hour), []byte(`{}`)); err != nil {
		t.Fatalf("append old event: %v", err)
	}

	clock.now = clock.now.Add(2 * 24 * time.Hour)
	if _, err := st.AppendEvent(ctx, "op-new", "conn-1", "new", "", clock.Now(), []byte(`{}`)); err != nil {
		t.Fatalf("append new event: %v", err)
	}

	events, err := st.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDurableEventsSince: %v", err)
	}
	if len(events) != 1 || events[0].Event.Type != "new" {
		t.Fatalf("retained events = %#v, want only the new event", events)
	}
}
