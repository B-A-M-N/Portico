package screens

import (
	"testing"
)

// TestChangingTheAddressDiscardsProtectionThatNeedsIt pins that an answer does
// not survive the invalidation of the answer it depended on.
//
// Protection requires an address that does not move. Switching to a generated
// address while a protection choice was already made left a combination core
// validation rejects, and the rejection arrived at create — a long way from the
// question that caused it.
func TestChangingTheAddressDiscardsProtectionThatNeedsIt(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "permanent_public"
	m.state.Protection = "email_otp"
	m.state.AllowedEmails = []string{"person@example.com"}
	m.state.Step = WizardStepExposure
	// Highlight the temporary option and choose it.
	m.selected = choiceIndex(m.exposureChoices(), "temporary_public")
	m.HandleKey("enter")

	if m.state.Protection != "" {
		t.Fatalf("protection %q survived a change that made it impossible", m.state.Protection)
	}
	if len(m.state.AllowedEmails) != 0 {
		t.Fatal("the people allowed through survived the protection being discarded")
	}
}

// TestChangingTheAddressKeepsProtectionThatStillWorks ensures the rule discards
// only what became impossible.
func TestChangingTheAddressKeepsProtectionThatStillWorks(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public"
	m.state.Protection = "none"
	m.state.Step = WizardStepExposure
	m.selected = choiceIndex(m.exposureChoices(), "permanent_public")
	m.HandleKey("enter")

	if m.state.Protection != "none" {
		t.Fatalf("protection %q was discarded though it remained possible", m.state.Protection)
	}
}

// TestAQuestionLeavingTheSequenceDoesNotStrandTheUser pins the escape hatch.
//
// A question whose predicate stops holding while the user is on it — an account
// question after the provider it belonged to went away — would otherwise have
// nowhere to go back to, because the step is no longer in the sequence.
func TestAQuestionLeavingTheSequenceDoesNotStrandTheUser(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.SourceType = "existing_service"
	m.state.ExposureMode = "temporary_public"
	m.state.Protection = "none"
	// On the account question, but no provider is selected, so it no longer
	// applies.
	m.state.Step = WizardStepAccount
	m.state.Provider = ""

	previous, ok := m.previousStep()
	if !ok {
		t.Fatal("a question that left the sequence offered no way back")
	}
	if !precedes(previous, WizardStepAccount) {
		t.Fatalf("back from the account question landed on %d, which does not precede it", previous)
	}

	m.goBack()
	if m.Step() == WizardStepAccount {
		t.Fatal("the user was left on a question that no longer applies")
	}
}

// TestTheEvaluationAndTheConnectionDescribeTheSameThing pins that a provider is
// scored against the connection that will actually be created.
//
// The two used to be normalised separately, and had diverged: only an existing
// service's protocol was defaulted on the recommendation side, while creation
// also defaults a command's, a directory's, an MCP command's and the MCP
// transport. A command or MCP connection was therefore scored with no protocol
// requirement and created with a concrete one.
func TestTheEvaluationAndTheConnectionDescribeTheSameThing(t *testing.T) {
	base := func() WizardState {
		return WizardState{
			Name: "demo", ExposureMode: "permanent_public", Hostname: "demo.example.com",
			Protection: "none", Provider: "cloudflare", AccountID: "acct-a",
		}
	}

	for _, tc := range []struct {
		name          string
		mutate        func(*WizardState)
		wantProtocol  string
		wantTransport string
	}{
		{
			name: "existing service with no protocol stated",
			mutate: func(s *WizardState) {
				s.SourceType = "existing_service"
				s.SourceAddress = "127.0.0.1"
				s.Port = "8080"
			},
			wantProtocol: "http",
		},
		{
			name: "existing service over https",
			mutate: func(s *WizardState) {
				s.SourceType = "existing_service"
				s.SourceAddress = "127.0.0.1"
				s.Port = "8443"
				s.SourceProtocol = "https"
			},
			wantProtocol: "https",
		},
		{
			name: "directory",
			mutate: func(s *WizardState) {
				s.SourceType = "directory"
				s.SourceAddress = "/srv/site"
				s.DirectoryMode = "read"
			},
			wantProtocol: "http",
		},
		{
			name: "command",
			mutate: func(s *WizardState) {
				s.SourceType = "command"
				s.SourceAddress = "server"
				s.Port = "3000"
			},
			wantProtocol: "http",
		},
		{
			name: "mcp server run as a command",
			mutate: func(s *WizardState) {
				s.SourceType = "mcp_server"
				s.MCPCommand = true
				s.SourceAddress = "mcp-server"
				s.Port = "4000"
			},
			wantProtocol:  "http",
			wantTransport: "http",
		},
		{
			name: "mcp server at an https endpoint",
			mutate: func(s *WizardState) {
				s.SourceType = "mcp_server"
				s.SourceAddress = "https://mcp.example.com/sse"
				s.MCPTransport = "sse"
			},
			wantProtocol:  "https",
			wantTransport: "sse",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWizard(nil, fullCloudflareSnapshot())
			state := base()
			tc.mutate(&state)
			m.state = state

			built := m.buildRequest()
			scored := m.recommendationRequest()

			if scored.ConnectionKind != built.Kind {
				t.Fatalf("kind: scored %q, created %q", scored.ConnectionKind, built.Kind)
			}
			if scored.SourceKind != built.Source.Kind {
				t.Fatalf("source kind: scored %q, created %q", scored.SourceKind, built.Source.Kind)
			}
			if scored.ExposureMode != built.Exposure.Mode {
				t.Fatalf("exposure: scored %q, created %q", scored.ExposureMode, built.Exposure.Mode)
			}
			if scored.RequestedAddress != built.Exposure.RequestedAddress {
				t.Fatalf("address: scored %q, created %q", scored.RequestedAddress, built.Exposure.RequestedAddress)
			}
			if scored.ProtectionKind != built.Protection.Kind {
				t.Fatalf("protection: scored %q, created %q", scored.ProtectionKind, built.Protection.Kind)
			}
			if scored.PreferredProvider != built.Provider.ProviderID {
				t.Fatalf("provider: scored %q, created %q", scored.PreferredProvider, built.Provider.ProviderID)
			}
			if scored.PreferredAccount != built.Provider.AccountID {
				t.Fatalf("account: scored %q, created %q", scored.PreferredAccount, built.Provider.AccountID)
			}
			if scored.Protocol != tc.wantProtocol {
				t.Fatalf("protocol: scored %q, want %q", scored.Protocol, tc.wantProtocol)
			}
			if scored.MCPTransport != tc.wantTransport {
				t.Fatalf("transport: scored %q, want %q", scored.MCPTransport, tc.wantTransport)
			}
			// The scored protocol must be the one the connection carries.
			if built.Source.Existing != nil && scored.Protocol != built.Source.Existing.Protocol {
				t.Fatalf("protocol: scored %q, created %q", scored.Protocol, built.Source.Existing.Protocol)
			}
			if built.Source.Command != nil && scored.Protocol != built.Source.Command.Protocol {
				t.Fatalf("protocol: scored %q, created %q", scored.Protocol, built.Source.Command.Protocol)
			}
			if built.Source.MCP != nil && scored.MCPTransport != built.Source.MCP.Transport {
				t.Fatalf("transport: scored %q, created %q", scored.MCPTransport, built.Source.MCP.Transport)
			}
		})
	}
}
