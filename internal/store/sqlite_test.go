package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paoloanzn/portico/internal/core"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testProfile() *core.ConnectionProfile {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &core.ConnectionProfile{
		ID:       "test-conn-1",
		Name:     "test-connection",
		Revision: 1,
		Source: core.SourceSpec{
			Kind: core.SourceExisting,
			Existing: &core.ExistingServiceSpec{
				Network:  "tcp",
				Address:  "localhost",
				Protocol: core.ProtocolHTTP,
			},
		},
		Exposure: core.ExposureSpec{
			Mode: core.ExposureTemporary,
		},
		Protection: core.ProtectionSpec{
			Kind: core.ProtectionNone,
		},
		Provider: core.ProviderSelection{
			ProviderID: core.ProviderID("mock"),
		},
		Lifecycle: core.LifecycleSpec{
			OnDisconnect: core.DisconnectKeepAlive,
		},
		Desired:   core.DesiredOpen,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func testRuntime() *core.ConnectionRuntime {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &core.ConnectionRuntime{
		ConnectionID:   "test-conn-1",
		State:          core.RuntimeOpen,
		LastObservedAt: now,
		LastTransition: now,
		Endpoint: core.EndpointRuntime{
			PublicAddress:  "https://test.example.com",
			PrivateAddress: "http://localhost:8080",
		},
		Provider: core.ProviderRuntime{
			ProviderID: core.ProviderID("mock"),
		},
	}
}

// TestSaveRuntimeColumnCount is a regression test ensuring the INSERT
// column list matches the number of bound arguments in SaveRuntime.
func TestSaveRuntimeColumnCount(t *testing.T) {
	s := newTestStore(t)

	// Save a profile first (FK constraint on runtime)
	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	rt := testRuntime()
	if err := s.SaveRuntime(context.Background(), rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
}

// TestSaveRuntimeActiveOperationRoundTrip ensures active_operation_id
// is correctly persisted and loaded.
func TestSaveRuntimeActiveOperationRoundTrip(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	opID := core.OperationID("op-123")
	rt := testRuntime()
	rt.ActiveOperation = &opID

	if err := s.SaveRuntime(context.Background(), rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	loaded, err := s.LoadRuntime(context.Background(), rt.ConnectionID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}

	if loaded.ActiveOperation == nil {
		t.Fatal("expected ActiveOperation to be non-nil after load")
	}
	if *loaded.ActiveOperation != opID {
		t.Fatalf("ActiveOperation = %q, want %q", *loaded.ActiveOperation, opID)
	}
}

// TestSaveRuntimeRoundTrip ensures all runtime fields survive save/load.
func TestSaveRuntimeRoundTrip(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	rt := testRuntime()
	if err := s.SaveRuntime(context.Background(), rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}

	loaded, err := s.LoadRuntime(context.Background(), rt.ConnectionID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}

	if loaded.ConnectionID != rt.ConnectionID {
		t.Fatalf("ConnectionID = %q, want %q", loaded.ConnectionID, rt.ConnectionID)
	}
	if loaded.State != rt.State {
		t.Fatalf("State = %q, want %q", loaded.State, rt.State)
	}
	if loaded.Endpoint.PublicAddress != rt.Endpoint.PublicAddress {
		t.Fatalf("PublicAddress = %q, want %q", loaded.Endpoint.PublicAddress, rt.Endpoint.PublicAddress)
	}
	if loaded.Endpoint.PrivateAddress != rt.Endpoint.PrivateAddress {
		t.Fatalf("PrivateAddress = %q, want %q", loaded.Endpoint.PrivateAddress, rt.Endpoint.PrivateAddress)
	}
	if loaded.Provider.ProviderID != rt.Provider.ProviderID {
		t.Fatalf("ProviderID = %q, want %q", loaded.Provider.ProviderID, rt.Provider.ProviderID)
	}
}

// TestSaveProfileUpsert verifies that a profile can be updated in place.
func TestSaveProfileUpsert(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	p.Revision = 2
	p.Name = "updated-connection"
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile update: %v", err)
	}

	loaded, err := s.LoadProfile(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}

	if loaded.Revision != 2 {
		t.Fatalf("Revision = %d, want 2", loaded.Revision)
	}
	if loaded.Name != "updated-connection" {
		t.Fatalf("Name = %q, want %q", loaded.Name, "updated-connection")
	}
}

// TestSaveResourceUpsert verifies provider resources upsert correctly.
func TestSaveResourceUpsert(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	res := &core.ProviderResource{
		ConnectionID: p.ID,
		ProviderID:   core.ProviderID("mock"),
		Type:         core.ResourceTunnel,
		ExternalID:   "tun-123",
		Ownership:    core.OwnershipManaged,
		Metadata:     map[string]string{"key": "value"},
	}

	if err := s.SaveResource(context.Background(), res); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}

	// Update ownership using AdoptResource (explicit ownership change).
	if err := s.AdoptResource(context.Background(), res.ProviderID, res.Type, res.ExternalID, p.ID, p.ID); err != nil {
		t.Fatalf("AdoptResource: %v", err)
	}

	loaded, err := s.LoadResource(context.Background(), res.ProviderID, res.Type, res.ExternalID)
	if err != nil {
		t.Fatalf("LoadResource: %v", err)
	}

	if loaded.Ownership != core.OwnershipAdopted {
		t.Fatalf("Ownership = %q, want %q", loaded.Ownership, core.OwnershipAdopted)
	}
}

// TestListUnresolvedFindings verifies finding persistence and query.
func TestSaveFindingAndListUnresolved(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	f := &core.DiagnosticFinding{
		ID:           core.FindingID("finding-1"),
		ConnectionID: p.ID,
		Segment:      core.RouteSegmentID("connector"),
		Severity:     core.SeverityError,
		Summary:      "Connector not running",
		Explanation:  "The connector process has exited",
		ObservedAt:   now,
	}

	if err := s.SaveFinding(context.Background(), f); err != nil {
		t.Fatalf("SaveFinding: %v", err)
	}

	findings, err := s.ListUnresolvedFindings(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("ListUnresolvedFindings: %v", err)
	}

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if findings[0].ID != f.ID {
		t.Fatalf("ID = %q, want %q", findings[0].ID, f.ID)
	}
	if findings[0].Summary != f.Summary {
		t.Fatalf("Summary = %q, want %q", findings[0].Summary, f.Summary)
	}
}

// TestListProfilesNoDeadlock ensures ListProfiles + LoadProfile RLock nesting does not deadlock.
func TestListProfilesNoDeadlock(t *testing.T) {
	s := newTestStore(t)

	// Save two profiles
	for _, id := range []core.ConnectionID{"a", "b"} {
		p := testProfile()
		p.ID = id
		p.Name = "conn-" + string(id)
		if err := s.SaveProfile(context.Background(), p); err != nil {
			t.Fatalf("SaveProfile(%q): %v", id, err)
		}
	}

	// ListProfiles internally calls LoadProfile for each, acquiring nested RLock.
	// This test must not deadlock.
	profiles, err := s.ListProfiles(context.Background())
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
}

// TestHealth verifies the health check works.
func TestHealth(t *testing.T) {
	s := newTestStore(t)
	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

// TestOpenFileMode verifies database file has restricted permissions.
func TestOpenFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	mode := info.Mode().Perm()
	if mode != 0600 {
		t.Fatalf("DB file mode = %04o, want 0600", mode)
	}
}

// TestSavePlanRoundTrip verifies plan persistence round-trips correctly.
func TestSavePlanRoundTrip(t *testing.T) {
	s := newTestStore(t)

	p := testProfile()
	if err := s.SaveProfile(context.Background(), p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	plan := &core.OperationPlan{
		ID:              core.PlanID("plan-1"),
		ConnectionID:    p.ID,
		ProfileRevision: 1,
		Provider:        core.ProviderID("mock"),
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{
			{
				ID:      "step-1",
				Summary: "Create tunnel",
				Technical: core.TechnicalOperation{
					Type: "mock.create_tunnel",
				},
			},
		},
		Fingerprint: "abc123",
		CreatedAt:   now,
	}

	if err := s.SavePlan(context.Background(), plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	loaded, err := s.LoadPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}

	if loaded.ID != plan.ID {
		t.Fatalf("ID = %q, want %q", loaded.ID, plan.ID)
	}
	if len(loaded.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(loaded.Steps))
	}
	if loaded.Steps[0].Technical.Type != "mock.create_tunnel" {
		t.Fatalf("Step[0].Technical.Type = %q, want %q", loaded.Steps[0].Technical.Type, "mock.create_tunnel")
	}
}
