package supervisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestTheSpecCrossesTheWireAsTheKindItIs pins that the detail DTO carries the
// arm the profile actually has.
//
// The builder read GetSource/GetExposure/GetProtection unconditionally, and
// those return zero values for every kind but service exposure. A port forward
// therefore arrived at the client as a service exposure with a blank source, a
// blank exposure mode and protection "" — indistinguishable on the wire from a
// public tunnel that had lost its configuration.
func TestTheSpecCrossesTheWireAsTheKindItIs(t *testing.T) {
	spec := describeSpec(portForwardProfile())

	if spec.Kind != string(core.ConnectionPortForward) {
		t.Fatalf("Kind = %q, want port_forward", spec.Kind)
	}
	if spec.ServiceExposure != nil {
		t.Fatalf("a port forward carries a service exposure arm: %#v", spec.ServiceExposure)
	}
	if spec.PortForward == nil {
		t.Fatal("the port forward arm is missing")
	}
	if spec.PortForward.LocalPort != 5432 || spec.PortForward.RemoteHost != "db.internal" {
		t.Fatalf("the port forward arm lost its detail: %#v", spec.PortForward)
	}
}

// TestEachKindPopulatesExactlyOneArm pins the union property the wire format
// now claims.
func TestEachKindPopulatesExactlyOneArm(t *testing.T) {
	profiles := []*core.ConnectionProfile{
		portForwardProfile(), clientTunnelProfile(), privateNetworkProfile(),
		previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone}),
	}
	for _, p := range profiles {
		spec := describeSpec(p)
		var populated int
		for _, set := range []bool{
			spec.ServiceExposure != nil, spec.PortForward != nil,
			spec.PrivateNetwork != nil, spec.ClientTunnel != nil,
		} {
			if set {
				populated++
			}
		}
		if populated != 1 {
			t.Errorf("%s populated %d arms, want exactly 1", p.EffectiveKind(), populated)
		}
		if spec.Kind != string(p.EffectiveKind()) {
			t.Errorf("%s reported kind %q", p.EffectiveKind(), spec.Kind)
		}
	}
}

// TestACommandsEnvironmentIsNotShippedInTheDetail pins that a rendered, logged
// and exportable view does not carry operator-supplied environment variables,
// which routinely hold tokens.
func TestACommandsEnvironmentIsNotShippedInTheDetail(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	profile.Spec.ServiceExposure.Source = core.SourceSpec{
		Kind: core.SourceCommand,
		Command: &core.CommandSpec{
			Executable: "node", Args: []string{"server.js"}, Port: 3000,
			Env: map[string]string{"STRIPE_SECRET_KEY": "sk_live_deadbeef"},
		},
	}

	encoded, err := json.Marshal(describeSpec(profile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sk_live_deadbeef") {
		t.Fatalf("a secret from the command environment is in the detail DTO: %s", encoded)
	}
	if !strings.Contains(string(encoded), "server.js") {
		t.Fatalf("the command line itself was dropped: %s", encoded)
	}
}

// TestThePreviewSaysWhoCanReachEachKind pins that the screen a user approves an
// open from answers "who can reach this?" for every kind, not just the one that
// happens to be published.
func TestThePreviewSaysWhoCanReachEachKind(t *testing.T) {
	cases := []struct {
		profile *core.ConnectionProfile
		want    string
		reject  string
	}{
		{portForwardProfile(), "Only this machine", "Anyone"},
		{clientTunnelProfile(), "No public address is created", "Anyone"},
		{privateNetworkProfile(), "joins net-1", "Anyone"},
	}
	for _, tc := range cases {
		plan := &core.OperationPlan{Intent: core.IntentOpen}
		got := describeAccess(plan, tc.profile)
		if got == "" {
			t.Errorf("%s: the preview says nothing about who can reach it", tc.profile.EffectiveKind())
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: access = %q, want it to mention %q", tc.profile.EffectiveKind(), got, tc.want)
		}
		if strings.Contains(got, tc.reject) {
			t.Errorf("%s: access = %q, which claims public reachability", tc.profile.EffectiveKind(), got)
		}
	}
}

// TestAnExposedServiceStillWarnsThatAnyoneCanReachIt guards the opposite error:
// making the other kinds honest must not soften the warning on the kind that
// genuinely is open to the internet.
func TestAnExposedServiceStillWarnsThatAnyoneCanReachIt(t *testing.T) {
	profile := previewProfile(core.ExposureTemporary, core.ProtectionSpec{Kind: core.ProtectionNone})
	got := describeAccess(&core.OperationPlan{Intent: core.IntentOpen}, profile)
	if !strings.Contains(got, "Anyone") {
		t.Fatalf("an unprotected public service no longer warns: %q", got)
	}
}

// TestTheListAndTheDetailAgreeOnWhatAConnectionIs pins that the summary a
// client sees in the list is the same summary it sees on the detail screen.
//
// These were built by two hand-maintained copies. Adding the kind to one left
// the other reporting every connection as kindless, which every reader resolves
// to "published service" — so a port forward was a forward in the list and a
// published service on its own detail screen.
func TestTheListAndTheDetailAgreeOnWhatAConnectionIs(t *testing.T) {
	profile := portForwardProfile()
	rt := &core.ConnectionRuntime{
		ConnectionID: profile.ID, State: core.RuntimeOpen,
		Connector: core.ConnectorRuntime{Status: core.ConnectorStatusRunning, PID: 7},
	}
	rt.Endpoint.PrivateAddress = "127.0.0.1:5432"

	summary := connectionSummaryDTO(profile, rt)
	if summary.Kind != string(core.ConnectionPortForward) {
		t.Fatalf("summary Kind = %q, want port_forward", summary.Kind)
	}
	if summary.PrivateAddress != "127.0.0.1:5432" {
		t.Fatalf("summary lost the listener: %#v", summary)
	}
	if summary.PublicAddress != "" {
		t.Fatalf("a port forward reports a public address: %q", summary.PublicAddress)
	}
}
