package core

import (
	"strings"
	"testing"
)

func TestConnectionProfileRejectsCredentialLikeCommandEnvironment(t *testing.T) {
	profile := &ConnectionProfile{
		ID:   "command-env",
		Name: "command-env",
		Source: SourceSpec{Kind: SourceCommand, Command: &CommandSpec{
			Executable: "server", Port: 8080, Env: map[string]string{"API_TOKEN": "not-persisted"},
		}},
		Exposure:   ExposureSpec{Mode: ExposureTemporary},
		Protection: ProtectionSpec{Kind: ProtectionNone},
		Provider:   ProviderSelection{ProviderID: "mock"},
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
		Source: SourceSpec{Kind: SourceCommand, Command: &CommandSpec{
			Executable: "server", Port: 8080, Env: map[string]string{"LOG_LEVEL": "debug"},
		}},
		Exposure:   ExposureSpec{Mode: ExposureTemporary},
		Protection: ProtectionSpec{Kind: ProtectionNone},
		Provider:   ProviderSelection{ProviderID: "mock"},
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
