package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/origin"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// TestPublicMutableDirectoryPlanCarriesWarning pins that the plan itself —
// not only the TUI's review screen — warns about a publicly reachable,
// write-enabled directory.
//
// A warning that exists only in the interface is invisible to every other
// consumer of the same immutable plan: a CLI user approving by fingerprint and
// an API caller scripting opens both deserve the same disclosure the TUI
// shows. The warning names the distinct consequences, because "can change the
// directory" understates remote deletion of files Portico will never restore.
func TestPublicMutableDirectoryPlanCarriesWarning(t *testing.T) {
	for _, tc := range []struct {
		name            string
		allowUpload     bool
		allowDelete     bool
		wantSubstrings  []string
		notWantContains string
	}{
		{
			name:           "upload only",
			allowUpload:    true,
			wantSubstrings: []string{"add or replace files"},
		},
		{
			name:           "delete only",
			allowDelete:    true,
			wantSubstrings: []string{"remove files permanently", "NOT restored"},
		},
		{
			name:           "upload and delete",
			allowUpload:    true,
			allowDelete:    true,
			wantSubstrings: []string{"add or replace files", "remove files permanently", "NOT restored"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := New(newTestRegistry(mock.New()), nil)
			ctrl.SetOriginManager(origin.NewManager())
			profile := newFailureTestProfile()
			profile.ID = "conn-mutable"
			profile.Driver = core.DriverSelection{ProviderID: "mock"}
			profile.Spec.ServiceExposure.Exposure = core.ExposureSpec{
				Mode:             core.ExposurePermanent,
				RequestedAddress: "files.example.com",
			}
			profile.Spec.ServiceExposure.Protection = core.ProtectionSpec{
				Kind:          core.ProtectionEmailOTP,
				AllowedEmails: []string{"owner@example.com"},
			}
			profile.Spec.ServiceExposure.Source = core.SourceSpec{
				Kind: core.SourceDirectory,
				Directory: &core.DirectorySpec{
					Path:        t.TempDir(),
					Mode:        core.DirectoryModeWrites,
					AllowUpload: tc.allowUpload,
					AllowDelete: tc.allowDelete,
				},
			}
			ctrl.mu.Lock()
			ctrl.profiles[profile.ID] = profile
			ctrl.mu.Unlock()

			plan, err := ctrl.PlanOpen(context.Background(), profile.ID)
			if err != nil {
				t.Fatalf("PlanOpen: %v", err)
			}
			found := false
			for _, warning := range plan.Warnings {
				if warning.Code != "PTO-OPEN-PUBLIC-MUTABLE-DIRECTORY" {
					continue
				}
				found = true
				for _, want := range tc.wantSubstrings {
					if !strings.Contains(warning.Message, want) {
						t.Errorf("warning %q does not contain %q", warning.Message, want)
					}
				}
			}
			if !found {
				t.Fatalf("plan carries no mutable-directory warning: %+v", plan.Warnings)
			}
		})
	}
}

// TestReadOnlyDirectoryPlanHasNoMutableWarning keeps the warning scoped to
// what is actually dangerous: a read-only public browser states nothing false.
func TestReadOnlyDirectoryPlanHasNoMutableWarning(t *testing.T) {
	ctrl := New(newTestRegistry(mock.New()), nil)
	ctrl.SetOriginManager(origin.NewManager())
	profile := newFailureTestProfile()
	profile.ID = "conn-readonly"
	profile.Driver = core.DriverSelection{ProviderID: "mock"}
	profile.Spec.ServiceExposure.Exposure = core.ExposureSpec{
		Mode:             core.ExposurePermanent,
		RequestedAddress: "files.example.com",
	}
	profile.Spec.ServiceExposure.Protection = core.ProtectionSpec{
		Kind:          core.ProtectionEmailOTP,
		AllowedEmails: []string{"owner@example.com"},
	}
	profile.Spec.ServiceExposure.Source = core.SourceSpec{
		Kind: core.SourceDirectory,
		Directory: &core.DirectorySpec{
			Path: t.TempDir(),
			Mode: core.DirectoryModeRead,
		},
	}
	ctrl.mu.Lock()
	ctrl.profiles[profile.ID] = profile
	ctrl.mu.Unlock()

	plan, err := ctrl.PlanOpen(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("PlanOpen: %v", err)
	}
	for _, warning := range plan.Warnings {
		if warning.Code == "PTO-OPEN-PUBLIC-MUTABLE-DIRECTORY" {
			t.Fatalf("a read-only directory produced a mutability warning: %+v", plan.Warnings)
		}
	}
}
