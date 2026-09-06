package screens

import (
	"strings"
	"testing"
)

// SPEC §29.4: show a warning before public exposure with upload or delete
// enabled. The wizard forces protection for such exposures; the review screen
// must also SAY what the user is about to publish — a remotely mutable view
// of a local directory — rather than describing it in neutral field words.

func publicDirectoryWizard(allowUpload, allowDelete bool) *WizardModel {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
	m.state = WizardState{
		Step:           WizardStepReview,
		ConnectionKind: "service_exposure",
		SourceType:     "directory",
		ExposureMode:   "permanent_public",
		Hostname:       "files.example.com",
		AllowUpload:    allowUpload,
		AllowDelete:    allowDelete,
	}
	return m
}

func TestPublicMutableDirectoryReviewCarriesWarning(t *testing.T) {
	for _, tc := range []struct{ name string }{
		{"upload only"}, {"delete only"}, {"upload and delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upload, del bool
			if strings.Contains(tc.name, "upload") {
				upload = true
			}
			if strings.Contains(tc.name, "delete") {
				del = true
			}
			m := publicDirectoryWizard(upload, del)
			view := m.serviceExposureReview()
			found := false
			for _, section := range view {
				if !strings.Contains(section.title, "warning") {
					continue
				}
				found = true
				text := strings.Join(section.lines, " ")
				if upload && !strings.Contains(text, "UPLOAD") {
					t.Error("upload warning missing")
				}
				if del && !strings.Contains(text, "DELETE") {
					t.Error("delete warning missing")
				}
				if del && !strings.Contains(text, "NOT restored") {
					t.Error("the no-restore caveat is missing")
				}
			}
			if !found {
				t.Fatalf("no security warning section in review:\n%v", view)
			}
		})
	}
}

func TestReadOnlyOrPrivateDirectoryHasNoWarning(t *testing.T) {
	// Read-only public browser: nothing mutable to warn about.
	m := publicDirectoryWizard(false, false)
	for _, section := range m.serviceExposureReview() {
		if strings.Contains(section.title, "warning") {
			t.Fatal("a read-only public browser produced a mutability warning")
		}
	}
}
