package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestOptionsAreUnchangedForAConfiguredCloudflare is the guard on this
// refactor: with the provider people actually have, every menu must offer
// exactly what it offered before options were derived from capability.
func TestOptionsAreUnchangedForAConfiguredCloudflare(t *testing.T) {
	caps := providerCapabilities{providers: fullCloudflareSnapshot()}

	if got := availableValues(caps.exposureChoices("existing_service", "")); !equalStrings(got,
		[]string{"temporary_public", "permanent_public"}) {
		t.Fatalf("exposures = %v", got)
	}
	if got := availableValues(caps.protectionChoices("permanent_public")); !equalStrings(got,
		[]string{"none", "email_otp"}) {
		t.Fatalf("protections on a permanent address = %v", got)
	}
	if got := availableValues(caps.protectionChoices("temporary_public")); !equalStrings(got,
		[]string{"none"}) {
		t.Fatalf("protections on a temporary address = %v", got)
	}
}

// TestOptionsAreUnchangedForQuickTunnelsOnly covers the other case people have:
// Cloudflare with no account.
func TestOptionsAreUnchangedForQuickTunnelsOnly(t *testing.T) {
	caps := providerCapabilities{providers: quickTunnelOnlySnapshot()}

	if got := availableValues(caps.exposureChoices("existing_service", "")); !equalStrings(got,
		[]string{"temporary_public"}) {
		t.Fatalf("exposures = %v", got)
	}
	if got := availableValues(caps.protectionChoices("temporary_public")); !equalStrings(got,
		[]string{"none"}) {
		t.Fatalf("protections = %v", got)
	}
}

// TestUnavailableOptionsAreShownWithWhatTheyNeed pins the behaviour this
// change exists for.
//
// Hiding an option left a one-item menu that still advertised navigation, and
// gave the user no way to learn that a permanent address exists or what it
// would take. An option nobody can see is an option nobody can ask for.
func TestUnavailableOptionsAreShownWithWhatTheyNeed(t *testing.T) {
	caps := providerCapabilities{providers: quickTunnelOnlySnapshot()}
	choices := caps.exposureChoices("existing_service", "")

	if len(choices) != 2 {
		t.Fatalf("got %d choices, want both listed", len(choices))
	}
	permanent := choices[1]
	if permanent.Available {
		t.Fatal("a permanent address is not available without an account")
	}
	if permanent.Reason == "" {
		t.Fatal("an unavailable option does not say why")
	}
	if len(permanent.Detail) == 0 {
		t.Fatal("an unavailable option does not say what it would do or need")
	}

	// The rendered menu must show it, and must not claim navigation it lacks.
	view := (&WizardModel{}).renderChoices("How should it be reachable?", choices, 0)
	if !strings.Contains(view, "Permanently") {
		t.Fatalf("the unavailable option is hidden:\n%s", view)
	}
	if !strings.Contains(view, permanent.Reason) {
		t.Fatalf("the reason is not shown:\n%s", view)
	}

	single := []wizardChoice{{Label: "Only one", Available: true}}
	if strings.Contains((&WizardModel{}).renderChoices("t", single, 0), "↑↓") {
		t.Fatal("a one-item menu advertises navigation with nowhere to go")
	}
	if !strings.Contains(view, "↑↓") {
		t.Fatal("a multi-item menu does not offer navigation")
	}
}

// TestASecondProviderChangesWhatIsOffered is the point of deriving from
// capability: a provider Portico has never heard of before extends the menu
// without any wizard change.
func TestASecondProviderChangesWhatIsOffered(t *testing.T) {
	caps := providerCapabilities{providers: append(quickTunnelOnlySnapshot(), ipc.ProviderDTO{
		ID: "acme", DisplayName: "Acme", Availability: "ready", Readiness: "ready", Selectable: true,
		Capabilities: &ipc.CapabilitySetDTO{
			CustomHostnames: true,
			ProtectionModes: []string{"none", "email_otp"},
		},
	})}

	choices := caps.exposureChoices("existing_service", "")
	if !choices[1].Available {
		t.Fatal("a second provider offering hostnames did not make the option available")
	}
	if !containsString(choices[1].Providers, "Acme") {
		t.Fatalf("the option does not say who provides it: %v", choices[1].Providers)
	}
	// Attribution matters: the user should know which provider carries it.
	if containsString(choices[1].Providers, "Cloudflare") {
		t.Fatal("a provider that cannot own a hostname was credited with the capability")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestTheWizardDoesNotReinterpretAvailability pins that selectability is read,
// not derived.
//
// A provider marked ready but not selectable must not be offered, and one
// marked selectable must be — whatever the availability string says. That is
// what keeps the wizard and the recommendation engine from drifting into two
// answers again.
func TestTheWizardDoesNotReinterpretAvailability(t *testing.T) {
	notSelectable := []ipc.ProviderDTO{{
		ID: "acme", DisplayName: "Acme", Availability: "ready", Readiness: "ready",
		Selectable: false,
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true, CustomHostnames: true,
			ProtectionModes: []string{"none", "email_otp"},
		},
	}}
	caps := providerCapabilities{providers: notSelectable}
	if values := availableValues(caps.exposureChoices("existing_service", "")); len(values) != 0 {
		t.Fatalf("a provider the supervisor called unselectable supplied options: %v", values)
	}

	selectable := notSelectable
	selectable[0].Selectable = true
	caps = providerCapabilities{providers: selectable}
	if values := availableValues(caps.exposureChoices("existing_service", "")); len(values) != 2 {
		t.Fatalf("a selectable provider supplied no options: %v", values)
	}
}

// accountlessCloudflareWithPotential is the truthful accountless Cloudflare:
// current capability is Quick-Tunnel-only, potential capability is the full
// account-scoped contract the definition knows about.
func accountlessCloudflareWithPotential() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "unconfigured", Readiness: "needs_config", Selectable: false,
		SetupActions: []string{"Add a Cloudflare account"},
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses:       true,
			ProtectionModes:          []string{"none"},
			Protocols:                []string{"http", "https"},
			PotentialCustomHostnames: true,
			PotentialProtectionModes: []string{"none", "email_otp"},
		},
	}}
}

// TestPermanentHostnameAdviceNamesOnlyPotentialProviders pins the P1-4 fix:
// setup advice for a permanent hostname comes from providers whose definition
// declares custom hostnames after setup, never from every unready provider.
// The old predicate — not usable OR not supporting hostnames — matched every
// unready provider at all, so the advice listed installing a tunnel client
// and enabling ngrok for a capability neither can ever deliver.
func TestPermanentHostnameAdviceNamesOnlyPotentialProviders(t *testing.T) {
	providers := append(accountlessCloudflareWithPotential(),
		ipc.ProviderDTO{
			ID: "client_tunnel", DisplayName: "Client-mediated MCP transport",
			Availability: "client_missing", Selectable: false,
			SetupActions: []string{"Install tunnel-client from the OpenAI platform's tunnel settings"},
			Capabilities: &ipc.CapabilitySetDTO{},
		},
		ipc.ProviderDTO{
			ID: "ngrok", DisplayName: "ngrok",
			Availability: "experimental", Selectable: false,
			SetupActions: []string{"Set PORTICO_ENABLE_EXPERIMENTAL_NGROK=1 to enable it"},
			Capabilities: &ipc.CapabilitySetDTO{},
		},
	)
	caps := providerCapabilities{providers: providers}

	choices := caps.exposureChoices("existing_service", "")
	permanent, ok := choiceAt(choices, 1)
	if !ok || permanent.Value != "permanent_public" {
		t.Fatalf("the exposure menu lost its permanent choice: %+v", choices)
	}
	if permanent.Available {
		t.Fatal("an accountless landscape offered a permanent hostname now")
	}
	for _, forbidden := range []string{
		"Install tunnel-client",
		"Set PORTICO_ENABLE_EXPERIMENTAL_NGROK",
	} {
		if strings.Contains(strings.Join(permanent.Detail, "\n"), forbidden) {
			t.Errorf("permanent-hostname advice recommends %q, which cannot deliver one:\n%s",
				forbidden, strings.Join(permanent.Detail, "\n"))
		}
	}
	if !strings.Contains(strings.Join(permanent.Detail, "\n"), "Add a Cloudflare account") {
		t.Errorf("permanent-hostname advice does not name the one provider that can deliver it:\n%s",
			strings.Join(permanent.Detail, "\n"))
	}
	if permanent.Reason != "needs a provider account" {
		t.Errorf("reason = %q, want the after-setup wording", permanent.Reason)
	}
}

// TestPermanentHostnameUnsupportedWhenNoProviderCanDeliver pins the third
// state: with no potential anywhere, the choice says so and offers no setup
// advice at all.
func TestPermanentHostnameUnsupportedWhenNoProviderCanDeliver(t *testing.T) {
	providers := []ipc.ProviderDTO{{
		ID: "portforward", DisplayName: "Local port forward",
		Availability: "ready", Readiness: "ready", Selectable: true,
		Capabilities: &ipc.CapabilitySetDTO{},
	}}
	caps := providerCapabilities{providers: providers}

	permanent := caps.exposureChoices("existing_service", "")[1]
	if permanent.Available {
		t.Fatal("a port-forward-only landscape offered a permanent hostname")
	}
	if permanent.Reason != "no installed provider offers permanent hostnames" {
		t.Errorf("reason = %q, want the unsupported wording", permanent.Reason)
	}
	if got := strings.Join(permanent.Detail, "\n"); strings.Contains(got, "setting up") {
		t.Errorf("the unsupported state offers setup advice:\n%s", got)
	}
}

// TestOTPGuidanceNamesOnlyPotentialProviders pins the same filter for the
// protection choice: email-OTP advice names only providers whose definition
// declares email OTP after setup.
func TestOTPGuidanceNamesOnlyPotentialProviders(t *testing.T) {
	providers := append(accountlessCloudflareWithPotential(),
		ipc.ProviderDTO{
			ID: "portforward", DisplayName: "Local port forward",
			Availability: "ready", Selectable: true,
			Capabilities: &ipc.CapabilitySetDTO{},
		},
	)
	caps := providerCapabilities{providers: providers}

	choices := caps.protectionChoices("permanent_public")
	otp, ok := choiceAt(choices, 1)
	if !ok || otp.Value != "email_otp" {
		t.Fatalf("the protection menu lost its OTP choice: %+v", choices)
	}
	if otp.Available {
		t.Fatal("OTP was offered with no provider able to apply it now")
	}
	got := strings.Join(otp.Detail, "\n")
	if !strings.Contains(got, "Add a Cloudflare account") {
		t.Errorf("OTP advice does not name the potential-capable provider:\n%s", got)
	}
	if otp.Reason != "needs a provider account" {
		t.Errorf("reason = %q, want the after-setup wording", otp.Reason)
	}
}

// TestAvailableNowNeedsNoAdvice pins that a provider that can deliver the
// capability right now is offered it without setup advice, even though its
// potential contract is also present.
func TestAvailableNowNeedsNoAdvice(t *testing.T) {
	caps := providerCapabilities{providers: fullCloudflareSnapshot()}

	permanent := caps.exposureChoices("existing_service", "")[1]
	if !permanent.Available {
		t.Fatalf("a configured Cloudflare was not offered a permanent hostname: %+v", permanent)
	}
	if got := strings.Join(permanent.Detail, "\n"); strings.Contains(got, "setting up") {
		t.Errorf("an available choice carries setup advice:\n%s", got)
	}

	otp := caps.protectionChoices("permanent_public")[1]
	if !otp.Available {
		t.Fatalf("a configured Cloudflare was not offered OTP: %+v", otp)
	}
}
