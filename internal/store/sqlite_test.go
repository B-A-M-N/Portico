package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
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

func TestConnectionCreateAndUpdateCommitDurableEventsWithState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	profile := testProfile()
	runtime := testRuntime()
	if err := s.CreateConnection(ctx, profile, runtime); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	events, err := s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("get creation event: %v", err)
	}
	if len(events) != 1 || events[0].Event.Type != core.EventConnectionCreated || events[0].ConnectionID != profile.ID {
		t.Fatalf("creation event = %#v", events)
	}

	profile.Name = "renamed"
	updated, err := s.UpdateProfile(ctx, profile, profile.Revision)
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if updated.Revision != profile.Revision+1 || updated.Name != "renamed" {
		t.Fatalf("updated profile = %#v", updated)
	}
	events, err = s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("get update event: %v", err)
	}
	if len(events) != 2 || events[1].Event.Type != core.EventConnectionUpdated || events[1].ConnectionID != profile.ID {
		t.Fatalf("update event = %#v", events)
	}
}

func TestCommitConnectorRuntimeEventPersistsStateAndEventTogether(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveProfile(ctx, testProfile()); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	rt := testRuntime()
	rt.Connector = core.ConnectorRuntime{PID: 4242, Status: core.ConnectorStatusRunning, Restarts: 2}
	when := time.Now().UTC().Truncate(time.Second)
	rt.LastTransition = when
	seq, err := s.CommitConnectorRuntimeEvent(ctx, rt, "connector.started", "running", when, map[string]string{"connection_id": string(rt.ConnectionID)})
	if err != nil {
		t.Fatalf("CommitConnectorRuntimeEvent: %v", err)
	}
	if seq < 1 {
		t.Fatalf("event sequence = %d, want positive", seq)
	}
	stored, err := s.LoadRuntime(ctx, rt.ConnectionID)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if stored.Connector.PID != 4242 || stored.Connector.Status != core.ConnectorStatusRunning || stored.Connector.Restarts != 2 {
		t.Fatalf("stored connector = %#v", stored.Connector)
	}
	events, err := s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDurableEventsSince: %v", err)
	}
	if len(events) != 1 || events[0].Event.Sequence != seq || events[0].Event.Type != "connector.started" || events[0].ConnectionID != rt.ConnectionID {
		t.Fatalf("durable connector event = %#v", events)
	}
}

func TestMarkResourceExternallyRemovedRequiresExactResource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	resource := &core.ProviderResource{
		ConnectionID: p.ID,
		ProviderID:   "mock",
		Type:         core.ResourceDNSRecord,
		ExternalID:   "dns-123",
		Ownership:    core.OwnershipManaged,
	}
	if err := s.SaveResource(ctx, resource); err != nil {
		t.Fatalf("SaveResource: %v", err)
	}
	if err := s.MarkResourceExternallyRemoved(ctx, p.ID, "mock", core.ResourceDNSRecord, "dns-123"); err != nil {
		t.Fatalf("MarkResourceExternallyRemoved: %v", err)
	}
	stored, err := s.LoadResource(ctx, "mock", core.ResourceDNSRecord, "dns-123")
	if err != nil {
		t.Fatalf("LoadResource: %v", err)
	}
	if stored.Lifecycle != core.LifecycleExternallyRemoved {
		t.Fatalf("lifecycle = %q, want %q", stored.Lifecycle, core.LifecycleExternallyRemoved)
	}
	if err := s.MarkResourceExternallyRemoved(ctx, p.ID, "mock", core.ResourceDNSRecord, "dns-missing"); err == nil {
		t.Fatal("MarkResourceExternallyRemoved accepted an unknown resource")
	}
}

func TestTunnelCredentialsAreExactProviderTunnelRecords(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-credential-rotation")
	providerID := core.ProviderID("cloudflare")
	if err := s.SaveTunnelCredential(ctx, connID, providerID, "tunnel-old", []byte("old-token")); err != nil {
		t.Fatalf("save old credential: %v", err)
	}
	if err := s.SaveTunnelCredential(ctx, connID, providerID, "tunnel-new", []byte("new-token")); err != nil {
		t.Fatalf("save replacement credential: %v", err)
	}
	oldToken, err := s.LoadTunnelCredentialExact(ctx, connID, providerID, "tunnel-old")
	if err != nil || oldToken != "old-token" {
		t.Fatalf("old exact credential = %q, %v", oldToken, err)
	}
	newToken, err := s.LoadTunnelCredentialExact(ctx, connID, providerID, "tunnel-new")
	if err != nil || newToken != "new-token" {
		t.Fatalf("new exact credential = %q, %v", newToken, err)
	}
	if _, _, err := s.LoadTunnelCredential(ctx, connID); err == nil {
		t.Fatal("ambiguous connection credential lookup unexpectedly succeeded")
	}
	if err := s.DeleteTunnelCredentialExact(ctx, connID, providerID, "tunnel-old"); err != nil {
		t.Fatalf("delete old credential: %v", err)
	}
	newToken, err = s.LoadTunnelCredentialExact(ctx, connID, providerID, "tunnel-new")
	if err != nil || newToken != "new-token" {
		t.Fatalf("replacement credential was changed by old cleanup: %q, %v", newToken, err)
	}
}

func TestLoadTunnelCredentialExactMigratesProviderUnboundCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-legacy-credential")
	tunnelID := "tunnel-legacy"
	legacyToken := []byte("legacy-token")
	encrypted, err := encryptCredential(s.secretStore, legacyToken, legacyCredentialContext(connID, tunnelID))
	if err != nil {
		t.Fatalf("encrypt legacy credential: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB().Exec(`INSERT INTO tunnel_credentials
		(connection_id, provider_id, tunnel_id, token_encrypted, created_at, updated_at)
		VALUES (?, '', ?, ?, ?, ?)`, connID, tunnelID, encrypted, now, now); err != nil {
		t.Fatalf("seed legacy credential: %v", err)
	}
	token, err := s.LoadTunnelCredentialExact(ctx, connID, "cloudflare", tunnelID)
	if err != nil || token != string(legacyToken) {
		t.Fatalf("load migrated credential = %q, %v", token, err)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM tunnel_credentials WHERE connection_id = ? AND provider_id = 'cloudflare' AND tunnel_id = ?`, connID, tunnelID).Scan(&rows); err != nil {
		t.Fatalf("count modern credential: %v", err)
	}
	if rows != 1 {
		t.Fatalf("modern credential rows = %d, want 1", rows)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM tunnel_credentials WHERE connection_id = ? AND provider_id = '' AND tunnel_id = ?`, connID, tunnelID).Scan(&rows); err != nil {
		t.Fatalf("count legacy credential: %v", err)
	}
	if rows != 0 {
		t.Fatalf("legacy credential rows = %d, want 0", rows)
	}
}

func TestRotateSecretKeyReencryptsCredentialsBeforeRetiringOldKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	connID := core.ConnectionID("conn-key-rotation")
	if err := s.SaveTunnelCredential(ctx, connID, "cloudflare", "tunnel-1", []byte("rotation-token")); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	var blobBytes []byte
	if err := s.DB().QueryRow(`SELECT token_encrypted FROM tunnel_credentials WHERE connection_id = ?`, connID).Scan(&blobBytes); err != nil {
		t.Fatalf("read original blob: %v", err)
	}
	var original EncryptedBlob
	if err := json.Unmarshal(blobBytes, &original); err != nil || original.KeyVersion != 1 {
		t.Fatalf("original blob = %#v, unmarshal error = %v", original, err)
	}
	version, err := s.RotateSecretKey(ctx)
	if err != nil || version != 2 {
		t.Fatalf("RotateSecretKey = version %d, err %v; want 2, nil", version, err)
	}
	if err := s.DB().QueryRow(`SELECT token_encrypted FROM tunnel_credentials WHERE connection_id = ?`, connID).Scan(&blobBytes); err != nil {
		t.Fatalf("read rotated blob: %v", err)
	}
	var rotated EncryptedBlob
	if err := json.Unmarshal(blobBytes, &rotated); err != nil || rotated.KeyVersion != version {
		t.Fatalf("rotated blob = %#v, unmarshal error = %v", rotated, err)
	}
	token, err := s.LoadTunnelCredentialExact(ctx, connID, "cloudflare", "tunnel-1")
	if err != nil || token != "rotation-token" {
		t.Fatalf("rotated credential = %q, %v", token, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.path), keyFileName)); !os.IsNotExist(err) {
		t.Fatalf("retired v1 key remains or stat failed: %v", err)
	}
	if _, err := os.Stat(versionedKeyFilePath(filepath.Dir(s.path), version)); err != nil {
		t.Fatalf("current rotated key missing: %v", err)
	}
}

func TestProviderCredentialIsEncryptedBoundAndRotated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveProviderCredential(ctx, "cloudflare", "account-a-token", []byte("api-token")); err != nil {
		t.Fatalf("SaveProviderCredential: %v", err)
	}
	if err := s.UpsertProviderAccount(ctx, core.ProviderAccount{
		ID: "account-a", Provider: "cloudflare", Label: "Account A", CredentialRef: "account-a-token",
		Metadata: map[string]string{"zone_id": "zone-a"}, Status: core.AccountAuthenticated,
	}); err != nil {
		t.Fatalf("UpsertProviderAccount: %v", err)
	}
	accounts, err := s.ListProviderAccounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].CredentialRef != "account-a-token" || accounts[0].Metadata["zone_id"] != "zone-a" {
		t.Fatalf("ListProviderAccounts = %#v, %v", accounts, err)
	}
	token, err := s.LoadProviderCredential(ctx, "cloudflare", "account-a-token")
	if err != nil || token != "api-token" {
		t.Fatalf("LoadProviderCredential = %q, %v", token, err)
	}
	if _, err := s.LoadProviderCredential(ctx, "mock", "account-a-token"); err == nil {
		t.Fatal("credential loaded for the wrong provider")
	}
	if _, err := s.RotateSecretKey(ctx); err != nil {
		t.Fatalf("RotateSecretKey: %v", err)
	}
	token, err = s.LoadProviderCredential(ctx, "cloudflare", "account-a-token")
	if err != nil || token != "api-token" {
		t.Fatalf("LoadProviderCredential after rotation = %q, %v", token, err)
	}
}

func TestReadSnapshotIncludesStateAndEventHighWater(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := testProfile()
	rt := testRuntime()
	if err := s.SaveProfile(ctx, p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if err := s.SaveRuntime(ctx, rt); err != nil {
		t.Fatalf("SaveRuntime: %v", err)
	}
	seq, err := s.AppendEvent(ctx, "op-1", p.ID, "connection.state_changed", "succeeded", time.Now().UTC(), []byte(`{"state":"open"}`))
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	snapshot, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snapshot.LastSeq != seq {
		t.Fatalf("snapshot LastSeq=%d, want %d", snapshot.LastSeq, seq)
	}
	if len(snapshot.Profiles) != 1 || snapshot.Profiles[0].ID != p.ID {
		t.Fatalf("snapshot profiles=%+v", snapshot.Profiles)
	}
	if len(snapshot.Runtimes) != 1 || snapshot.Runtimes[0].ConnectionID != p.ID || snapshot.Runtimes[0].State != core.RuntimeOpen {
		t.Fatalf("snapshot runtimes=%+v", snapshot.Runtimes)
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

func TestSyncFindingsAtomicallyAppendsOnlyNewAndResolvedDurableEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	profile := testProfile()
	if err := s.SaveProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	finding := core.DiagnosticFinding{
		ID: "finding-durable-events", ConnectionID: profile.ID, Segment: core.SegmentProtection,
		Severity: core.SeverityError, Summary: "Protection drift", Explanation: "exact resource missing", ObservedAt: time.Now().UTC(),
	}
	if err := s.SyncFindings(ctx, profile.ID, []core.DiagnosticFinding{finding}); err != nil {
		t.Fatalf("first SyncFindings: %v", err)
	}
	events, err := s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event.Type != core.EventDiagnosticFinding || events[0].ConnectionID != profile.ID {
		t.Fatalf("unexpected first diagnostic event: %#v", events)
	}
	if err := s.SyncFindings(ctx, profile.ID, []core.DiagnosticFinding{finding}); err != nil {
		t.Fatalf("repeat SyncFindings: %v", err)
	}
	events, err = s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("unchanged finding must not re-emit, got %#v", events)
	}
	if err := s.SyncFindings(ctx, profile.ID, nil); err != nil {
		t.Fatalf("resolution SyncFindings: %v", err)
	}
	events, err = s.GetDurableEventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Event.Type != core.EventDiagnosticResolved || events[1].ConnectionID != profile.ID {
		t.Fatalf("unexpected resolution event: %#v", events)
	}
	// The runtime projection is committed alongside the findings. After the
	// resolution run it must be empty rather than retain stale diagnostics.
	runtime := &core.ConnectionRuntime{ConnectionID: profile.ID, State: core.RuntimeOpen, LastObservedAt: time.Now().UTC(), LastTransition: time.Now().UTC()}
	if err := s.SaveRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFindings(ctx, profile.ID, []core.DiagnosticFinding{finding}); err != nil {
		t.Fatal(err)
	}
	storedRuntime, err := s.LoadRuntime(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedRuntime.Diagnostics) != 1 || storedRuntime.Diagnostics[0].ID != finding.ID {
		t.Fatalf("runtime diagnostics not synchronized: %#v", storedRuntime.Diagnostics)
	}
	if err := s.SyncFindings(ctx, profile.ID, nil); err != nil {
		t.Fatal(err)
	}
	storedRuntime, err = s.LoadRuntime(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedRuntime.Diagnostics) != 0 {
		t.Fatalf("resolved diagnostics remained in runtime: %#v", storedRuntime.Diagnostics)
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

// TestOpenCreatesMissingParent verifies first-run XDG database locations work
// even when the Portico data directory has not been created yet.
func TestOpenCreatesMissingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "nested", "portico.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open missing parent: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created: %v", err)
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
