package supervisor

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/provider/builtin"
	"github.com/B-A-M-N/portico/internal/provider/mock"
	"github.com/B-A-M-N/portico/internal/provider/portforward"
)

// SetupKind on the provider DTOs is derived from the provider's own
// declaration, not from provider names or availability.
//
// `configurable := provider.ID != ""` in the TUI enabled "Add account" for
// every catalogued provider because the DTOs carried nothing better: the
// supervisor knew which providers declare a setup flow and never said so. These
// tests pin that the served answer comes from the Definition — "account" for a
// provider that stores a credential, "guidance" for one that only instructs,
// and empty for one with no flow at all — including for a provider whose
// adapter cannot currently be built, which is exactly the one a user needs to
// configure.
func TestSnapshotCarriesSetupKindFromTheDeclaration(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st,
		mock.NewDefinition(),
		portforward.NewDefinition(),
	)
	sup.activateAll(context.Background())

	snapshot, err := (&supervisorHandler{sup: sup}).HandleSnapshot()
	if err != nil {
		t.Fatalf("HandleSnapshot: %v", err)
	}

	got := map[string]string{}
	for _, p := range snapshot.Providers {
		got[p.ID] = p.SetupKind
	}
	// The mock declares an account-storing flow.
	if got["mock"] != "account" {
		t.Errorf("mock SetupKind = %q, want %q", got["mock"], "account")
	}
	// The local port forward has no setup flow at all: nothing to configure,
	// so a client must not offer to configure it.
	if got["portforward"] != "" {
		t.Errorf("portforward SetupKind = %q, want empty", got["portforward"])
	}
}

func TestReadinessCarriesSetupKindFromTheDeclaration(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st,
		mock.NewDefinition(),
		portforward.NewDefinition(),
	)
	sup.activateAll(context.Background())

	readiness, err := (&supervisorHandler{sup: sup}).HandleReadiness()
	if err != nil {
		t.Fatalf("HandleReadiness: %v", err)
	}

	got := map[string]string{}
	for _, p := range readiness.Providers {
		got[p.ID] = p.SetupKind
	}
	if got["mock"] != "account" {
		t.Errorf("mock SetupKind = %q, want %q", got["mock"], "account")
	}
	if got["portforward"] != "" {
		t.Errorf("portforward SetupKind = %q, want empty", got["portforward"])
	}
}

// TestSetupKindSurvivesAFailedActivation pins that setup capability is static:
// a provider whose adapter could not be constructed still reports its declared
// kind, because a provider that is switched off is the one a user most needs to
// set up.
func TestSetupKindSurvivesAFailedActivation(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, mock.NewDefinition())
	// Deliberately no successful activation: the mock needs its runtime, and
	// without it the registry keeps only what a failed activation declared.
	_ = sup.activateDefinition(context.Background(), mock.NewDefinition())

	snapshot, err := (&supervisorHandler{sup: sup}).HandleSnapshot()
	if err != nil {
		t.Fatalf("HandleSnapshot: %v", err)
	}
	for _, p := range snapshot.Providers {
		if p.ID == "mock" && p.SetupKind != "account" {
			t.Fatalf("mock SetupKind = %q with a dead adapter, want account", p.SetupKind)
		}
	}
}

// TestBuiltinDefinitionSetupKindMatrix pins the served SetupKind for every
// provider compiled into the binary, derived the way the supervisor derives
// it: from the definition's own declaration, statically.
//
// The hand-built-DTO tests above pin the TUI's reading of the field; this
// matrix pins that the field's value is right for the real composition root.
// A provider gaining or losing a setup flow without this table would change
// what the providers screen offers and only the TUI fixtures would know.
func TestBuiltinDefinitionSetupKindMatrix(t *testing.T) {
	st := newRecoveryTestStore(t)
	sup, _ := activationTestSupervisor(t, st, builtin.Definitions(builtin.Config{})...)

	cases := []struct {
		id   string
		want string
	}{
		{"cloudflare", "account"},
		{"ngrok", ""},
		{"client_tunnel", "account"},
		{"portforward", ""},
		{"tailscale", "guidance"},
		{"zrok", ""},
	}
	for _, tc := range cases {
		if got := sup.setupKindFor(tc.id); got != tc.want {
			t.Errorf("%s SetupKind = %q, want %q", tc.id, got, tc.want)
		}
	}
}
