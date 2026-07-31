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
	view := renderChoices("How should it be reachable?", choices, 0)
	if !strings.Contains(view, "Permanently") {
		t.Fatalf("the unavailable option is hidden:\n%s", view)
	}
	if !strings.Contains(view, permanent.Reason) {
		t.Fatalf("the reason is not shown:\n%s", view)
	}

	single := []wizardChoice{{Label: "Only one", Available: true}}
	if strings.Contains(renderChoices("t", single, 0), "↑↓") {
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
