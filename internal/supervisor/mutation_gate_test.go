package supervisor

import (
	"testing"
	"time"
)

// TestMutationAdmissionClosesBeforeShutdownProceeds pins the ordering
// invariant: an admitted handler keeps shutdown out until it releases its
// lease, and no new handler can acquire one after shutdown closes the gate.
func TestMutationAdmissionClosesBeforeShutdownProceeds(t *testing.T) {
	s := &Supervisor{mutating: true}
	release, err := s.admitMutation()
	if err != nil {
		t.Fatalf("admitMutation: %v", err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		s.beginShutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown closed the gate while a mutation lease was active")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not proceed after the mutation lease was released")
	}
	if s.AcceptingMutations() {
		t.Fatal("shutdown left mutation admission open")
	}
	if _, err := s.admitMutation(); err == nil {
		t.Fatal("mutation was admitted after shutdown closed the gate")
	}
}
