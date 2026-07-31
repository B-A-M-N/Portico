package core

import (
	"strings"
	"testing"
)

func TestConnectionProfileRejectsCredentialLikeCommandEnvironment(t *testing.T) {
	profile := &ConnectionProfile{
		ID:   "command-env",
		Name: "command-env",
		Kind: ConnectionServiceExposure,
		Spec: ConnectionSpec{
			ServiceExposure: &ServiceExposureSpec{
				Source: SourceSpec{Kind: SourceCommand, Command: &CommandSpec{
					Executable: "server", Port: 8080, Env: map[string]string{"API_TOKEN": "not-persisted"},
				}},
				Exposure:   ExposureSpec{Mode: ExposureTemporary},
				Protection: ProtectionSpec{Kind: ProtectionNone},
			},
		},
		Driver: DriverSelection{ProviderID: "mock"},
	}
	err := profile.Validate()
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("Validate error = %v, want credential rejection", err)
	}
}

func TestConnectionProfileAllowsNonSensitiveCommandEnvironment(t *testing.T) {
	profile := &ConnectionProfile{
		ID:   "command-env-safe",
		Name: "command-env-safe",
		Kind: ConnectionServiceExposure,
		Spec: ConnectionSpec{
			ServiceExposure: &ServiceExposureSpec{
				Source: SourceSpec{Kind: SourceCommand, Command: &CommandSpec{
					Executable: "server", Port: 8080, Env: map[string]string{"LOG_LEVEL": "debug"},
				}},
				Exposure:   ExposureSpec{Mode: ExposureTemporary},
				Protection: ProtectionSpec{Kind: ProtectionNone},
			},
		},
		Driver: DriverSelection{ProviderID: "mock"},
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestConnectionProfileRejectsWriteDirectoryWithoutProtection(t *testing.T) {
	profile := &ConnectionProfile{
		ID:   "write-dir-no-protection",
		Name: "write-dir-no-protection",
		Kind: ConnectionServiceExposure,
		Spec: ConnectionSpec{
			ServiceExposure: &ServiceExposureSpec{
				Source: SourceSpec{Kind: SourceDirectory, Directory: &DirectorySpec{
					Path:        "/tmp/upload",
					Mode:        DirectoryModeWrites,
					AllowUpload: true,
				}},
				Exposure:   ExposureSpec{Mode: ExposureTemporary},
				Protection: ProtectionSpec{Kind: ProtectionNone},
			},
		},
		Driver: DriverSelection{ProviderID: "mock"},
	}
	err := profile.Validate()
	if err == nil || !strings.Contains(err.Error(), "protection") {
		t.Fatalf("Validate error = %v, want rejection of write-enabled directory without protection", err)
	}
}

func TestConnectionProfileAllowsWriteDirectoryWithProtection(t *testing.T) {
	profile := &ConnectionProfile{
		ID:   "write-dir-protected",
		Name: "write-dir-protected",
		Kind: ConnectionServiceExposure,
		Spec: ConnectionSpec{
			ServiceExposure: &ServiceExposureSpec{
				Source: SourceSpec{Kind: SourceDirectory, Directory: &DirectorySpec{
					Path:        "/tmp/upload",
					Mode:        DirectoryModeWrites,
					AllowUpload: true,
				}},
				Exposure:   ExposureSpec{Mode: ExposurePermanent, RequestedAddress: "files.example.com"},
				Protection: ProtectionSpec{Kind: ProtectionEmailOTP},
			},
		},
		Driver: DriverSelection{ProviderID: "mock"},
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestObservedConnectionFingerprintIsCanonicalAndComplete(t *testing.T) {
	base := &ObservedConnection{ConnectionID: "c", ProviderID: "p", Tunnel: &ObservedTunnel{ID: "t1", State: "healthy"},
		DNSRecords:       []ObservedDNSRecord{{ID: "2", Name: "b", Target: "old"}, {ID: "1", Name: "a", Target: "target"}},
		AccessApps:       []ObservedAccessApp{{ID: "b", Domain: "b.example"}, {ID: "a", Domain: "a.example"}},
		ResourceStatuses: []ObservedResourceStatus{{Type: ResourceDNSRecord, ExternalID: "2", Status: ObservationPresent}, {Type: ResourceTunnel, ExternalID: "t1", Status: ObservationPresent}}}
	first, err := base.ComputeFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	reordered := *base
	reordered.DNSRecords = []ObservedDNSRecord{base.DNSRecords[1], base.DNSRecords[0]}
	reordered.AccessApps = []ObservedAccessApp{base.AccessApps[1], base.AccessApps[0]}
	reordered.ResourceStatuses = []ObservedResourceStatus{base.ResourceStatuses[1], base.ResourceStatuses[0]}
	second, err := reordered.ComputeFingerprint()
	if err != nil || first != second {
		t.Fatalf("canonical fingerprint mismatch: %q / %q, err=%v", first, second, err)
	}
	changed := *base
	changed.DNSRecords = append([]ObservedDNSRecord(nil), base.DNSRecords...)
	changed.DNSRecords[0].Target = "new"
	third, err := changed.ComputeFingerprint()
	if err != nil || first == third {
		t.Fatalf("fingerprint did not capture DNS target change: %q / %q, err=%v", first, third, err)
	}
	withPolicy := *base
	withPolicy.AccessPolicies = []ObservedAccessPolicy{{ID: "policy-1", AppID: "app-1", Decision: "allow", AllowedEmails: []string{"person@example.com"}}}
	policyFingerprint, err := withPolicy.ComputeFingerprint()
	if err != nil || first == policyFingerprint {
		t.Fatalf("fingerprint did not capture Access policy state: %q / %q, err=%v", first, policyFingerprint, err)
	}
	// Fingerprinting must canonicalize a copy, not reorder the provider
	// observation that callers may continue to inspect.
	policyOrder := &ObservedConnection{AccessPolicies: []ObservedAccessPolicy{{
		ID: "policy-2", AllowedEmails: []string{"z@example.com", "a@example.com"},
	}}}
	if _, err := policyOrder.ComputeFingerprint(); err != nil {
		t.Fatal(err)
	}
	if policyOrder.AccessPolicies[0].AllowedEmails[0] != "z@example.com" {
		t.Fatalf("ComputeFingerprint mutated observed policy: %#v", policyOrder.AccessPolicies)
	}
}

// TestProtectionRequiresAnAddressThatDoesNotMove pins a rule that previously
// lived only as a condition in the setup wizard.
//
// A temporary address changes when the connector restarts, so a policy bound to
// it stops applying with nothing reporting that it stopped. Because the rule was
// enforced only in one screen, the recommendation engine — which checks whether
// a provider supports a protection kind, never whether the exposure can carry
// it — accepted the combination, and it failed when the plan was applied, after
// the connection had been saved.
func TestProtectionRequiresAnAddressThatDoesNotMove(t *testing.T) {
	newProfile := func(mode ExposureMode, protection ProtectionKind) *ConnectionProfile {
		// A requested hostname belongs only to a permanent address; setting one
		// on a temporary address is separately invalid.
		exposure := ExposureSpec{Mode: mode}
		if mode == ExposurePermanent {
			exposure.RequestedAddress = "app.example.com"
		}
		return &ConnectionProfile{
			ID: "c1", Name: "test", Kind: ConnectionServiceExposure,
			Spec: ConnectionSpec{ServiceExposure: &ServiceExposureSpec{
				Source: SourceSpec{Kind: SourceExisting, Existing: &ExistingServiceSpec{
					Network: "tcp", Address: "127.0.0.1:3000", Protocol: ProtocolHTTP,
				}},
				Exposure:   exposure,
				Protection: ProtectionSpec{Kind: protection, AllowedEmails: []string{"p@example.com"}},
			}},
			Driver: DriverSelection{ProviderID: "mock"},
		}
	}

	if err := newProfile(ExposureTemporary, ProtectionEmailOTP).Validate(); err == nil {
		t.Fatal("protection was accepted on an address that changes")
	}
	if err := newProfile(ExposurePermanent, ProtectionEmailOTP).Validate(); err != nil {
		t.Fatalf("protection on a permanent address was refused: %v", err)
	}
	if err := newProfile(ExposureTemporary, ProtectionNone).Validate(); err != nil {
		t.Fatalf("an unprotected temporary address was refused: %v", err)
	}
	// Private-network protection is not bound to a public hostname, so a
	// changing address does not detach it.
	if err := newProfile(ExposureTemporary, ProtectionPrivateNet).Validate(); err != nil {
		t.Fatalf("private-network protection was refused on a temporary address: %v", err)
	}
}
