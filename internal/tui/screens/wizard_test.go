package screens

import (
	"reflect"
	"testing"
)

func TestExistingServiceAddressPreservesPortsAndFormatsIPv6(t *testing.T) {
	tests := []struct {
		name    string
		address string
		port    string
		want    string
	}{
		{name: "empty defaults to loopback", port: "8080", want: "127.0.0.1:8080"},
		{name: "hostname", address: "localhost", port: "8080", want: "localhost:8080"},
		{name: "ipv4", address: "127.0.0.1", port: "8080", want: "127.0.0.1:8080"},
		{name: "raw ipv6", address: "::1", port: "8080", want: "[::1]:8080"},
		{name: "bracketed ipv6", address: "[::1]", port: "8080", want: "[::1]:8080"},
		{name: "existing port", address: "[::1]:3000", port: "8080", want: "[::1]:3000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := existingServiceAddress(test.address, test.port)
			if err != nil {
				t.Fatalf("existingServiceAddress(%q, %q): %v", test.address, test.port, err)
			}
			if got != test.want {
				t.Fatalf("existingServiceAddress(%q, %q) = %q, want %q", test.address, test.port, got, test.want)
			}
		})
	}
}

func TestExistingServiceAddressRejectsAmbiguousColonAddress(t *testing.T) {
	if _, err := existingServiceAddress("example.com:3000:extra", "8080"); err == nil {
		t.Fatal("expected invalid host:port address to be rejected")
	}
}

func TestEmailOTPProtectionRulesAreValidatedAndIncludedInRequest(t *testing.T) {
	emails, domains, err := parseProtectionRules("Person@example.com, @Example.com, person@example.com")
	if err != nil {
		t.Fatalf("parseProtectionRules: %v", err)
	}
	if want := []string{"person@example.com"}; !reflect.DeepEqual(emails, want) {
		t.Fatalf("emails = %#v, want %#v", emails, want)
	}
	if want := []string{"example.com"}; !reflect.DeepEqual(domains, want) {
		t.Fatalf("domains = %#v, want %#v", domains, want)
	}

	m := NewWizard(nil, true)
	m.state = WizardState{
		Name:           "Protected service",
		SourceType:     "existing_service",
		SourceAddress:  "127.0.0.1",
		SourceProtocol: "http",
		Port:           "8080",
		ExposureMode:   "permanent_public",
		Hostname:       "demo.example.com",
		Protection:     "email_otp",
		AllowedEmails:  emails,
		AllowedDomains: domains,
		Provider:       "cloudflare",
	}
	req := m.buildRequest()
	if req.Protection.Kind != "email_otp" || !reflect.DeepEqual(req.Protection.AllowedEmails, emails) || !reflect.DeepEqual(req.Protection.AllowedDomains, domains) {
		t.Fatalf("protection request = %#v", req.Protection)
	}
}

func TestEmailOTPProtectionRulesRejectEmptyAndMalformedValues(t *testing.T) {
	for _, input := range []string{"", "not an address", "@bad_domain"} {
		if _, _, err := parseProtectionRules(input); err == nil {
			t.Fatalf("parseProtectionRules(%q) unexpectedly succeeded", input)
		}
	}
}

func TestProtectionChoicesFollowConfiguredCapabilities(t *testing.T) {
	if got := NewWizard(nil, false).protections(); !reflect.DeepEqual(got, []string{"none"}) {
		t.Fatalf("limited choices = %#v", got)
	}
	full := NewWizard(nil, true)
	full.state.ExposureMode = "permanent_public"
	if got := full.protections(); !reflect.DeepEqual(got, []string{"none", "email_otp"}) {
		t.Fatalf("full choices = %#v", got)
	}
}
