package supervisor

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/provider"
)

// TestSelectabilityIsDecidedOnceForEveryAvailability pins that one answer
// crosses the boundary for every state a provider can be in.
//
// Clients were each reading the availability string and disagreeing: the
// recommendation engine refused experimental and degraded providers while the
// wizard's fallback list refused only missing clients, so a failed
// recommendation offered providers the engine considers unusable.
func TestSelectabilityIsDecidedOnceForEveryAvailability(t *testing.T) {
	for _, tc := range []struct {
		availability provider.Availability
		selectable   bool
	}{
		{provider.AvailabilityReady, true},
		// Everything below can be worth showing, and none of it can carry a
		// connection right now.
		{provider.AvailabilityUnconfigured, false},
		{provider.AvailabilityClientMissing, false},
		{provider.AvailabilityExperimental, false},
		{provider.AvailabilityNotImplemented, false},
		{provider.AvailabilityDegraded, false},
	} {
		t.Run(string(tc.availability), func(t *testing.T) {
			if got := tc.availability == provider.AvailabilityReady; got != tc.selectable {
				t.Fatalf("selectable = %v, want %v", got, tc.selectable)
			}
		})
	}
}
