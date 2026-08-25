package supervisor

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

func existingHealthProfile(spec core.HealthCheckSpec) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		Kind: core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{ServiceExposure: &core.ServiceExposureSpec{
			Source: core.SourceSpec{
				Kind:     core.SourceExisting,
				Existing: &core.ExistingServiceSpec{Health: spec},
			},
		}},
	}
}

func TestExistingServiceHealthPreservesLegacyDefault(t *testing.T) {
	if !serviceCheckApplicable(existingHealthProfile(core.HealthCheckSpec{})) {
		t.Fatal("legacy existing-service profile lost its default service probe")
	}
	if serviceCheckApplicable(existingHealthProfile(core.HealthCheckSpec{Configured: true})) {
		t.Fatal("explicitly disabled health probe is still applicable")
	}
	if !serviceCheckApplicable(existingHealthProfile(core.HealthCheckSpec{Configured: true, Enabled: true})) {
		t.Fatal("explicitly enabled health probe is not applicable")
	}
}

func TestHealthSpecFromDTOMarksExplicitConfiguration(t *testing.T) {
	spec, err := healthSpecFromDTO(&ipc.HealthCheckSpecDTO{
		Enabled: true, Path: "/healthz", Timeout: "2s", Interval: "30s",
	})
	if err != nil {
		t.Fatalf("healthSpecFromDTO: %v", err)
	}
	if !spec.Configured || !spec.Enabled || spec.Path != "/healthz" || spec.Timeout.String() != "2s" || spec.Interval.String() != "30s" {
		t.Fatalf("health spec = %+v", spec)
	}
}
