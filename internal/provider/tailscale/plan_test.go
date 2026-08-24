package tailscale

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// joinProfile is a connection that records this machine's tailnet membership.
func joinProfile(desired core.DesiredConnectionState) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "conn-join", Name: "my network",
		Kind:     core.ConnectionPrivateNetwork,
		Desired:  desired,
		Revision: 1,
		Driver:   core.DriverSelection{ProviderID: "tailscale"},
		Spec: core.ConnectionSpec{
			PrivateNetwork: &core.PrivateNetworkSpec{Mode: core.PrivateNetworkJoin},
		},
	}
}

// exposeProfile publishes a local address to the tailnet.
//
// The address comes from the service-exposure source, which is where every other kind
// carries "the thing being made reachable". A Tailscale-specific field would mean the
// same concept had two names.
func exposeProfile(desired core.DesiredConnectionState, address string) *core.ConnectionProfile {
	return &core.ConnectionProfile{
		ID: "conn-serve", Name: "api on the tailnet",
		Kind:     core.ConnectionPrivateNetwork,
		Desired:  desired,
		Revision: 2,
		Driver:   core.DriverSelection{ProviderID: "tailscale"},
		Spec: core.ConnectionSpec{
			PrivateNetwork: &core.PrivateNetworkSpec{Mode: core.PrivateNetworkExpose},
			ServiceExposure: &core.ServiceExposureSpec{
				Source: core.SourceSpec{
					Kind:     core.SourceExisting,
					Existing: &core.ExistingServiceSpec{Address: address},
				},
			},
		},
	}
}

// TestCapabilitiesRefuseEverythingPublic pins the declaration that keeps a private
// choice private.
//
// `tailscale funnel` would publish to the internet. It is deliberately not implemented,
// and the capability declaration is what stops the recommendation engine offering
// Tailscale for a public exposure — a user choosing "private network" must not end up
// with a public address.
func TestCapabilitiesRefuseEverythingPublic(t *testing.T) {
	caps, err := New(newFakeRunner()).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(caps.Kinds) != 1 || caps.Kinds[0] != core.ConnectionPrivateNetwork {
		t.Fatalf("kinds = %v, want private_network only", caps.Kinds)
	}
	if caps.TemporaryAddresses.Supported {
		t.Error("a temporary public address is declared supported")
	}
	if caps.CustomHostnames.Supported {
		t.Error("a custom public hostname is declared supported")
	}
	if caps.ManagedDNS.Supported {
		t.Error("managed public DNS is declared supported")
	}
	if !caps.PrivateExposure.Supported {
		t.Error("private exposure is declared unsupported, which is the one thing this does")
	}
	// Every protocol it offers is private.
	for protocol, capability := range caps.Protocols {
		if capability.Supported && !capability.Private {
			t.Errorf("protocol %s is declared supported but not private", protocol)
		}
	}
	// The only protection is tailnet membership, which is not a policy Portico applies.
	for _, protection := range caps.BuiltInProtection {
		if protection.Supported && protection.Kind != core.ProtectionPrivateNet {
			t.Errorf("protection %s is declared supported; Portico cannot enforce it here",
				protection.Kind)
		}
	}
}

// TestJoiningAnAlreadySignedInMachineCreatesNothing pins the ownership decision.
//
// Membership is machine-wide, predates the connection and outlives it. A plan claiming
// to create it would be describing work Portico is not going to do, and would imply
// closing the connection should undo it.
func TestJoiningAnAlreadySignedInMachineCreatesNothing(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	plan, err := New(runner).Plan(context.Background(),
		core.DesiredConnection{Profile: joinProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Intent != core.IntentOpen {
		t.Fatalf("intent = %q", plan.Intent)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("a join plan has %d steps, want only the membership check: %+v",
			len(plan.Steps), plan.Steps)
	}
	if plan.Steps[0].Technical.Type != "verify_membership" {
		t.Fatalf("the only step is %q", plan.Steps[0].Technical.Type)
	}
	// The address the user would actually use.
	if plan.Expected.PrivateAddress != "workstation.tail0abc.ts.net" {
		t.Fatalf("expected private address %q", plan.Expected.PrivateAddress)
	}
	if plan.Expected.PublicAddress != "" {
		t.Fatalf("a private network plan promised a public address %q",
			plan.Expected.PublicAddress)
	}
	if plan.Fingerprint == "" {
		t.Error("the plan has no fingerprint, so preview cannot be bound to apply")
	}
}

// TestPlanningRefusesASignedOutMachineWithInstructions pins that the refusal is useful.
//
// Portico holds no Tailscale credential and cannot sign the machine in. Saying so, with
// the command to run, is the difference between a blocked user and a next step.
func TestPlanningRefusesASignedOutMachineWithInstructions(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusNeedsLogin, nil)
	_, err := New(runner).Plan(context.Background(),
		core.DesiredConnection{Profile: joinProfile(core.DesiredOpen)})

	if err == nil {
		t.Fatal("a signed-out machine was planned against")
	}
	if !strings.Contains(err.Error(), "tailscale up") {
		t.Errorf("the refusal does not say how to sign in: %v", err)
	}
	if !strings.Contains(err.Error(), "does not hold Tailscale credentials") {
		t.Errorf("the refusal does not say why Portico cannot do it: %v", err)
	}
}

// TestPlanningRefusesAnUnapprovedMachine pins the state that is not a failure.
//
// Signed in but not approved is a person's decision in the admin console, not something
// wrong with the machine. It must not be reported as a broken client.
func TestPlanningRefusesAnUnapprovedMachine(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusNeedsMachineAuth, nil)
	_, err := New(runner).Plan(context.Background(),
		core.DesiredConnection{Profile: joinProfile(core.DesiredOpen)})

	if err == nil {
		t.Fatal("an unapproved machine was planned against")
	}
	if !strings.Contains(err.Error(), "admin console") {
		t.Errorf("the refusal does not say where to approve it: %v", err)
	}
}

// TestPlanningRefusesADifferentTailnet pins that Portico does not silently switch
// networks.
//
// Moving a machine between tailnets means signing out of one and into another, which
// affects every connection on the machine. Portico refuses and says which network it is
// actually on.
func TestPlanningRefusesADifferentTailnet(t *testing.T) {
	profile := joinProfile(core.DesiredOpen)
	profile.Spec.PrivateNetwork.NetworkID = "example.com"

	runner := newFakeRunner().on("status --json", statusOtherTailnet, nil)
	_, err := New(runner).Plan(context.Background(), core.DesiredConnection{Profile: profile})

	if err == nil {
		t.Fatal("a machine on a different tailnet was planned against")
	}
	if !strings.Contains(err.Error(), "other.example") {
		t.Errorf("the refusal does not name the tailnet the machine is on: %v", err)
	}
}

// TestServingPlansTheStepsInOrder pins the expose path.
func TestServingPlansTheStepsInOrder(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	plan, err := New(runner).Plan(context.Background(),
		core.DesiredConnection{Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000")})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var types []string
	for _, step := range plan.Steps {
		types = append(types, step.Technical.Type)
	}
	want := []string{"verify_membership", "verify_origin", "serve", "verify_serve"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", types, want)
	}

	// The summary says who can reach it, because that is the question the kind exists
	// to answer.
	var summaries string
	for _, step := range plan.Steps {
		summaries += step.Summary + "\n"
	}
	if !strings.Contains(summaries, "only to the tailnet") {
		t.Errorf("no step says the service stays private:\n%s", summaries)
	}
	if plan.Expected.PublicAddress != "" {
		t.Error("a serve plan promised a public address")
	}
}

// TestServingNeedsAnAddress pins that the plan refuses rather than publishing nothing.
func TestServingNeedsAnAddress(t *testing.T) {
	profile := exposeProfile(core.DesiredOpen, "")
	profile.Spec.ServiceExposure = nil

	runner := newFakeRunner().on("status --json", statusRunning, nil)
	_, err := New(runner).Plan(context.Background(), core.DesiredConnection{Profile: profile})
	if err == nil {
		t.Fatal("a serve with no address to publish was planned")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestClosingAJoinDoesNotSignTheMachineOut pins the most consequential decision here.
//
// `tailscale down` would take the machine off the network entirely, breaking every other
// connection on it — and undoing something Portico never did.
func TestClosingAJoinDoesNotSignTheMachineOut(t *testing.T) {
	runner := newFakeRunner()
	plan, err := New(runner).Plan(context.Background(),
		core.DesiredConnection{Profile: joinProfile(core.DesiredClosed)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Intent != core.IntentClose {
		t.Fatalf("intent = %q", plan.Intent)
	}
	for _, step := range plan.Steps {
		if strings.Contains(strings.ToLower(step.Summary), "sign out") &&
			!strings.Contains(step.Summary, "will not sign it out") {
			t.Errorf("a close step suggests signing the machine out: %q", step.Summary)
		}
	}
	// And the summary tells the user the machine stays on the network.
	if !strings.Contains(plan.Steps[0].Summary, "stays signed in") {
		t.Errorf("the close step does not say the machine stays signed in: %q",
			plan.Steps[0].Summary)
	}
	// Planning a close asks the client nothing: there is nothing to check.
	if runner.called("status") {
		t.Error("closing a join queried the client's status for no reason")
	}
}

// TestAWrongKindIsRefused pins that the adapter does not accept a connection it cannot
// deliver.
func TestAWrongKindIsRefused(t *testing.T) {
	profile := joinProfile(core.DesiredOpen)
	profile.Kind = core.ConnectionServiceExposure

	_, err := New(newFakeRunner()).Plan(context.Background(),
		core.DesiredConnection{Profile: profile})
	if err == nil {
		t.Fatal("a service exposure was planned by the private-network provider")
	}
}
