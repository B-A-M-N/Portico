package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// TestPlanOpenRefusesAProfileThatCanNoLongerBeOpened pins where a stored
// profile meets a rule that tightened after it was saved.
//
// Rejecting it at load would make a connection disappear because a rule
// changed. Rejecting it here refuses only the attempt to open it and says why —
// before the provider is resolved and before any origin is prepared, so the
// failure names the reason rather than surfacing as a provider error.
func TestPlanOpenRefusesAProfileThatCanNoLongerBeOpened(t *testing.T) {
	ctrl := New(newTestRegistry(mock.New()), nil)

	// A profile of the shape that was storable before protection required a
	// stable address.
	profile := newFailureTestProfile()
	profile.ID = "conn-legacy"
	profile.Driver = core.DriverSelection{ProviderID: "mock"}
	profile.Spec.ServiceExposure.Exposure = core.ExposureSpec{Mode: core.ExposureTemporary}
	profile.Spec.ServiceExposure.Protection = core.ProtectionSpec{
		Kind:          core.ProtectionEmailOTP,
		AllowedEmails: []string{"person@example.com"},
	}
	// Installed directly, as a restore would, bypassing CreateProfile.
	ctrl.mu.Lock()
	ctrl.profiles[profile.ID] = profile
	ctrl.mu.Unlock()

	_, err := ctrl.PlanOpen(context.Background(), profile.ID)
	if err == nil {
		t.Fatal("a profile that cannot be opened produced a plan")
	}
	if !strings.Contains(err.Error(), "can no longer be opened") {
		t.Fatalf("the refusal does not say what happened: %v", err)
	}
	if !strings.Contains(err.Error(), "permanent address") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}
