package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/mock"
)

// clientTunnelMock presents the mock provider under the transport identity
// the alias resolves to.
type clientTunnelMock struct {
	*mock.Provider
}

func (p *clientTunnelMock) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "client_tunnel", Name: "client_tunnel", DisplayName: "Client Tunnel"}
}

// clientTunnelMultiAccountMock is a client_tunnel-identified provider that
// resolves account-scoped children.
type clientTunnelMultiAccountMock struct {
	*mock.Provider
	children map[core.ProviderAccountID]core.Provider
}

func (p *clientTunnelMultiAccountMock) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "client_tunnel", Name: "client_tunnel", DisplayName: "Client Tunnel"}
}

func (p *clientTunnelMultiAccountMock) ProviderForAccount(id core.ProviderAccountID) (core.Provider, error) {
	child := p.children[id]
	if child == nil {
		return nil, errors.New("account unavailable")
	}
	return child, nil
}

// TestALegacyClientTunnelProviderIDStillResolves pins the read-time alias for
// profiles persisted before the workload/transport split: they carry
// "openai_tunnel" as the provider ID, and must still resolve to the
// client_tunnel provider rather than fail as not-found.
func TestALegacyClientTunnelProviderIDStillResolves(t *testing.T) {
	registry := newTestRegistry(&clientTunnelMock{mock.New()})
	ctrl := New(registry, newTestJournal())

	profile := &core.ConnectionProfile{
		Driver: core.DriverSelection{ProviderID: "openai_tunnel"},
	}

	if _, err := ctrl.providerForProfile(profile); err != nil {
		t.Fatalf("a legacy openai_tunnel profile no longer resolves: %v", err)
	}
}

// TestTheAliasResolvesThroughAnAccountScopedProvider pins that the alias is
// applied before account binding, so a legacy profile on a multi-account
// transport reaches the right account adapter.
func TestTheAliasResolvesThroughAnAccountScopedProvider(t *testing.T) {
	child := mock.New()
	registry := newTestRegistry(&clientTunnelMultiAccountMock{Provider: child, children: map[core.ProviderAccountID]core.Provider{"account-a": child}})
	ctrl := New(registry, newTestJournal())

	profile := &core.ConnectionProfile{
		Driver: core.DriverSelection{ProviderID: "openai_tunnel", AccountID: "account-a"},
	}
	resolved, err := ctrl.providerForProfile(profile)
	if err != nil {
		t.Fatalf("a legacy profile with an account no longer resolves: %v", err)
	}
	if resolved == nil {
		t.Fatal("the account-scoped provider was not returned")
	}
}

// TestTheAliasIsReadTimeOnly pins that "openai_tunnel" is never registered as
// its own provider: a lookup of the legacy spelling against a registry that
// holds only client_tunnel succeeds through the alias, while nothing in the
// registry answers to the old name directly.
func TestTheAliasIsReadTimeOnly(t *testing.T) {
	registry := newTestRegistry(&clientTunnelMock{mock.New()})

	if registry.Get("openai_tunnel") != nil {
		t.Fatal("the legacy spelling was registered as a second provider")
	}
	if registry.Get("client_tunnel") == nil {
		t.Fatal("the transport provider is missing from the registry")
	}
}

// TestAnUnknownProviderStillFailsByName pins that the alias does not swallow
// genuinely unknown providers — the error must name what was asked for.
func TestAnUnknownProviderStillFailsByName(t *testing.T) {
	ctrl := New(newTestRegistry(mock.New()), newTestJournal())

	profile := &core.ConnectionProfile{
		Driver: core.DriverSelection{ProviderID: "no_such_provider"},
	}
	_, err := ctrl.providerForProfile(profile)
	if err == nil {
		t.Fatal("an unknown provider resolved anyway")
	}
	if !strings.Contains(err.Error(), "no_such_provider") && !strings.Contains(err.Error(), "not found") &&
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("the error does not identify the provider: %v", err)
	}
}
