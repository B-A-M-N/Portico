package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

type stubProvider struct {
	id       core.ProviderID
	caps     core.Capabilities
	capsErr  error
	capsHold chan struct{}
}

func (s *stubProvider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: s.id, Name: string(s.id), DisplayName: string(s.id)}
}

func (s *stubProvider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	if s.capsHold != nil {
		select {
		case <-s.capsHold:
		case <-ctx.Done():
			return core.Capabilities{}, ctx.Err()
		}
	}
	return s.caps, s.capsErr
}

func (s *stubProvider) Authenticate(context.Context, core.AuthRequest) error { return nil }

func (s *stubProvider) Plan(context.Context, core.DesiredConnection) (*core.OperationPlan, error) {
	return nil, nil
}
func (s *stubProvider) Observe(context.Context, core.ConnectionID) (*core.ObservedConnection, error) {
	return &core.ObservedConnection{}, nil
}
func (s *stubProvider) ExecuteStep(context.Context, core.ConnectionID, core.PlanStep) (core.StepResult, error) {
	return core.StepResult{}, nil
}

// TestCatalogEntriesRemainVisibleWithoutAnAdapter pins audit item 11. A
// provider whose client is missing was previously skipped entirely, so the UI
// could not distinguish "not installed" from "does not exist".
func TestCatalogEntriesRemainVisibleWithoutAnAdapter(t *testing.T) {
	r := NewRegistry()
	r.AddCatalogEntry(CatalogEntry{
		ID:           "cloudflare",
		DisplayName:  "Cloudflare",
		Availability: AvailabilityClientMissing,
		Reason:       "cloudflared was not found on PATH",
		SetupActions: []string{"Install cloudflared"},
	})
	r.AddCatalogEntry(CatalogEntry{
		ID:           "tailscale",
		Availability: AvailabilityNotImplemented,
		Reason:       "no adapter yet",
	})

	snaps := r.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("got %d providers, want 2 catalogued entries", len(snaps))
	}

	byID := map[core.ProviderID]ProviderSnapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}

	cf := byID["cloudflare"]
	if cf.Availability != AvailabilityClientMissing {
		t.Fatalf("cloudflare availability = %q, want %q", cf.Availability, AvailabilityClientMissing)
	}
	if cf.Reason == "" {
		t.Fatal("client-missing provider carries no reason")
	}
	if len(cf.SetupActions) == 0 {
		t.Fatal("client-missing provider offers no setup action")
	}
	if cf.DisplayName != "Cloudflare" {
		t.Fatalf("display name = %q", cf.DisplayName)
	}
	// A not-implemented provider must be distinguishable from a missing client.
	if byID["tailscale"].Availability != AvailabilityNotImplemented {
		t.Fatalf("tailscale availability = %q", byID["tailscale"].Availability)
	}
}

// TestRegisteredAdapterSupersedesCatalogEntry ensures a provider that does load
// is not listed twice or reported as unavailable.
func TestRegisteredAdapterSupersedesCatalogEntry(t *testing.T) {
	r := NewRegistry()
	r.AddCatalogEntry(CatalogEntry{ID: "cloudflare", Availability: AvailabilityClientMissing})
	if err := r.Add(&stubProvider{id: "cloudflare"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	r.SetAccounts("cloudflare", []core.ProviderAccountID{"acct-1"})

	snaps := r.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("got %d entries, want 1 (adapter supersedes catalog)", len(snaps))
	}
	if snaps[0].Availability != AvailabilityReady {
		t.Fatalf("availability = %q, want ready", snaps[0].Availability)
	}
}

// TestAPendingAccountDoesNotMakeAProviderReady pins the restart half of the
// authenticated-only-after-validation invariant.
//
// Setup correctly stores an unverified credential as pending and says so. The
// registry then derived "authenticated" from how many accounts existed, so
// after a restart that same pending account made the provider look ready and
// selectable — the invariant held in the database and broke on reconstruction.
func TestAPendingAccountDoesNotMakeAProviderReady(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(&stubProvider{id: "acme"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	r.SetAccountInfo("acme", []AccountInfo{
		{ID: "acct-pending", Label: "Unverified", Status: string(core.AccountPending)},
	})

	snaps := r.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("got %d entries, want 1", len(snaps))
	}
	if snaps[0].Authenticated {
		t.Fatal("a pending account made the provider report as authenticated")
	}
	if snaps[0].Availability == AvailabilityReady {
		t.Fatal("a pending account made the provider report as ready")
	}
	// The account must remain visible so it can be repaired rather than
	// silently vanishing from the provider screen.
	if len(snaps[0].PendingAccounts) != 1 {
		t.Fatalf("pending account not surfaced for management: %#v", snaps[0].PendingAccounts)
	}
	// It must not be offered for selection or operations.
	if len(snaps[0].Accounts) != 0 {
		t.Fatalf("a pending account was offered as usable: %#v", snaps[0].Accounts)
	}
}

// TestAnAuthenticatedAccountAlongsideAPendingOneKeepsTheProviderUsable ensures
// the pending status is a per-account fact, not a provider-wide downgrade.
func TestAnAuthenticatedAccountAlongsideAPendingOneKeepsTheProviderUsable(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(&stubProvider{id: "acme"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	r.SetAccountInfo("acme", []AccountInfo{
		{ID: "acct-good", Label: "Verified", Status: string(core.AccountAuthenticated)},
		{ID: "acct-pending", Label: "Unverified", Status: string(core.AccountPending)},
	})

	snap := r.Snapshot()[0]
	if !snap.Authenticated || snap.Availability != AvailabilityReady {
		t.Fatal("a verified account did not keep the provider usable")
	}
	if len(snap.Accounts) != 1 || snap.Accounts[0].ID != "acct-good" {
		t.Fatalf("usable accounts = %#v, want only the verified one", snap.Accounts)
	}
	if len(snap.PendingAccounts) != 1 {
		t.Fatalf("pending account not surfaced: %#v", snap.PendingAccounts)
	}
}

// TestRevokedAndExpiredAccountsAreNotUsable covers the other statuses that mean
// a credential cannot be relied on.
func TestRevokedAndExpiredAccountsAreNotUsable(t *testing.T) {
	for _, status := range []core.ProviderAccountStatus{core.AccountRevoked, core.AccountExpired} {
		t.Run(string(status), func(t *testing.T) {
			r := NewRegistry()
			if err := r.Add(&stubProvider{id: "acme"}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			r.SetAccountInfo("acme", []AccountInfo{{ID: "acct", Status: string(status)}})

			snap := r.Snapshot()[0]
			if snap.Authenticated || snap.Availability == AvailabilityReady {
				t.Fatalf("a %s account left the provider usable", status)
			}
		})
	}
}

// TestCapabilityErrorIsReportedNotDiscarded ensures a failed capability query
// is not presented as an authoritative empty capability set.
func TestCapabilityErrorIsReportedNotDiscarded(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(&stubProvider{id: "broken", capsErr: errors.New("api unreachable")}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snaps := r.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("got %d entries", len(snaps))
	}
	if snaps[0].Availability != AvailabilityDegraded {
		t.Fatalf("availability = %q, want degraded", snaps[0].Availability)
	}
	if snaps[0].CapabilityError == "" {
		t.Fatal("capability error was discarded")
	}
}

// TestSnapshotDoesNotHoldTheLockDuringCapabilityQueries pins the concurrency
// fix. Capabilities used to be queried while the registry read lock was held
// and with a background context, so one slow adapter blocked every other
// registry operation indefinitely.
func TestSnapshotDoesNotHoldTheLockDuringCapabilityQueries(t *testing.T) {
	r := NewRegistry()
	release := make(chan struct{})
	if err := r.Add(&stubProvider{id: "slow", capsHold: release}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Snapshot()
	}()

	// While the snapshot is blocked inside Capabilities, an unrelated writer
	// must still be able to take the lock.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		r.SetAccounts("other", []core.ProviderAccountID{"acct"})
	}()

	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("registry writer blocked while a capability query was in flight")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot did not complete")
	}
}

// TestSnapshotIsConcurrencySafe exercises the copy-then-query structure under
// parallel access.
func TestSnapshotIsConcurrencySafe(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(&stubProvider{id: "a"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	r.AddCatalogEntry(CatalogEntry{ID: "b", Availability: AvailabilityNotImplemented})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				r.Snapshot()
				r.SetAccounts("a", []core.ProviderAccountID{"acct"})
				r.GetAccounts("a")
			}
		}()
	}
	wg.Wait()
}
