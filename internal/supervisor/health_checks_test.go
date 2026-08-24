package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

// One interpretation of machine health.
//
// There were four. Readiness aggregated provider availability and per-connection
// blockers; doctor re-derived provider status from the same fields with its own
// switch, reaching its own verdicts; setup rendered readiness; recovery interpreted
// a failure to connect. The copy that drifted was whichever one nobody was reading.

// newReadinessHandler builds a handler with a real store, a real registry and its
// own data directory, so the checks that read files and the database are exercised
// against real ones rather than against stubs that cannot fail the way real ones do.
func newReadinessHandler(t *testing.T) *supervisorHandler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "portico.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	registry := provider.NewRegistry()
	socket := filepath.Join(dir, "portico.sock")
	// A socket file with the permissions the supervisor creates, so the default
	// case is the healthy one and a test that wants a wide socket makes it wide.
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return &supervisorHandler{sup: &Supervisor{
		store:      st,
		registry:   registry,
		controller: controller.New(registry, st),
		paths: app.Paths{
			DatabasePath: filepath.Join(dir, "portico.db"),
			SocketPath:   socket,
			LogDir:       dir,
		},
	}}
}

// findCheck returns the check with the given ID.
func findCheck(t *testing.T, checks []ipc.HealthCheckDTO, id string) ipc.HealthCheckDTO {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("no check with ID %q; got %v", id, checkIDs(checks))
	return ipc.HealthCheckDTO{}
}

func checkIDs(checks []ipc.HealthCheckDTO) []string {
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		out = append(out, check.ID)
	}
	return out
}

// TestEveryCheckExplainsItself pins the shape a check has to have.
//
// A state on its own is not a health report: a reader needs to know what was
// checked and what it means. And anything not "ok" has to say what to do, or it is
// a complaint rather than a diagnosis.
func TestEveryCheckExplainsItself(t *testing.T) {
	h := newReadinessHandler(t)
	checks := h.healthChecks(context.Background())
	if len(checks) == 0 {
		t.Fatal("no health checks were produced at all")
	}

	for _, check := range checks {
		if check.ID == "" {
			t.Errorf("a check has no ID: %+v", check)
		}
		if check.Title == "" {
			t.Errorf("check %q has no title, so nothing can say what was checked", check.ID)
		}
		if check.Summary == "" {
			t.Errorf("check %q has no summary, so its state cannot be interpreted", check.ID)
		}
		switch check.State {
		case "ok", "attention", "problem", "unknown":
		default:
			t.Errorf("check %q has the unrecognised state %q", check.ID, check.State)
		}
	}
}

// TestAnUnrunnableCheckIsNotAPass pins the rule that matters most.
//
// A check that could not be run must report "unknown". Reporting "ok" is how a
// broken machine looks healthy — the failure mode the audit warns about, where a
// green result is arranged rather than observed.
func TestAnUnrunnableCheckIsNotAPass(t *testing.T) {
	h := newReadinessHandler(t)
	// No store: every check that reads the database becomes unanswerable.
	h.sup.store = nil

	checks := h.healthChecks(context.Background())
	database := findCheck(t, checks, "database")
	if database.State == "ok" {
		t.Fatalf("a check that could not run reported ok: %+v", database)
	}
	if database.State != "unknown" {
		t.Errorf("state = %q, want unknown", database.State)
	}
	if !strings.Contains(database.Summary, "not open") {
		t.Errorf("the summary does not say why it could not be checked: %q", database.Summary)
	}
}

// TestTheSocketPermissionCheckRefusesAWideSocket pins a real security check.
func TestTheSocketPermissionCheckRefusesAWideSocket(t *testing.T) {
	h := newReadinessHandler(t)

	// A socket other users can open.
	if err := os.WriteFile(h.sup.paths.SocketPath, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.sup.paths.SocketPath, 0o666); err != nil {
		t.Fatal(err)
	}

	check := h.socketCheck()
	if check.State != "problem" {
		t.Fatalf("a world-accessible socket is reported as %q", check.State)
	}
	if !strings.Contains(check.Summary, "other users") {
		t.Errorf("the summary does not say who can reach it: %q", check.Summary)
	}
	if check.NextAction == "" {
		t.Error("the check gives no way to fix it")
	}

	// Tightened, it passes.
	if err := os.Chmod(h.sup.paths.SocketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if check := h.socketCheck(); check.State != "ok" {
		t.Fatalf("a private socket is reported as %q: %s", check.State, check.Summary)
	}
}

// TestAMissingSocketIsAProblemWhileRunning pins that the supervisor answering this
// question at all means the socket should exist.
func TestAMissingSocketIsAProblemWhileRunning(t *testing.T) {
	h := newReadinessHandler(t)
	h.sup.paths.SocketPath = filepath.Join(t.TempDir(), "definitely-absent.sock")

	check := h.socketCheck()
	if check.State != "problem" {
		t.Fatalf("a missing socket is reported as %q", check.State)
	}
	if check.NextAction == "" {
		t.Error("the check does not say what to do about it")
	}
}

// TestTheKeyCheckDistinguishesFreshFromLost pins the difference that matters.
//
// No key and nothing to decrypt is a new installation. No key with accounts stored
// is unrecoverable data, and the symptom — every credential failing at once — looks
// like every credential being wrong rather than the key being gone.
func TestTheKeyCheckDistinguishesFreshFromLost(t *testing.T) {
	h := newReadinessHandler(t)

	// Fresh: no key, no accounts.
	check := h.keyDatabaseCheck()
	if check.State != "ok" {
		t.Fatalf("a fresh installation reports %q: %s", check.State, check.Summary)
	}

	// A key present is fine.
	keyPath := filepath.Join(filepath.Dir(h.sup.paths.DatabasePath), "portico-key.bin")
	if err := os.WriteFile(keyPath, []byte("not-a-real-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := h.keyDatabaseCheck(); check.State != "ok" {
		t.Fatalf("a present key reports %q: %s", check.State, check.Summary)
	}

	// Several keys is worth a note, not a problem: rotation leaves the old ones.
	second := filepath.Join(filepath.Dir(h.sup.paths.DatabasePath), "portico-key-2.bin")
	if err := os.WriteFile(second, []byte("older-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	check = h.keyDatabaseCheck()
	if check.State != "attention" {
		t.Fatalf("two keys report %q, want attention: %s", check.State, check.Summary)
	}
	if check.NextAction == "" {
		t.Error("the multiple-key note gives no guidance")
	}
}

// TestCleanupObligationsAreCounted pins that an invisible cost becomes visible.
func TestCleanupObligationsAreCounted(t *testing.T) {
	h := newReadinessHandler(t)
	checks := h.healthChecks(context.Background())

	cleanup := findCheck(t, checks, "cleanup")
	// A fresh store has none.
	if cleanup.State != "ok" {
		t.Fatalf("a clean store reports %q: %s", cleanup.State, cleanup.Summary)
	}
	if !strings.Contains(cleanup.Summary, "No provider resources") {
		t.Errorf("the summary does not say the store is clean: %q", cleanup.Summary)
	}
}

// TestReadinessCarriesTheChecks pins the convergence: one computation, read by
// every surface.
//
// Doctor prints these and the setup screen renders them. If readiness did not carry
// them, each would have to ask separately and could get different answers.
func TestReadinessCarriesTheChecks(t *testing.T) {
	h := newReadinessHandler(t)

	readiness, err := h.HandleReadiness()
	if err != nil {
		t.Fatalf("HandleReadiness: %v", err)
	}
	if len(readiness.Checks) == 0 {
		t.Fatal("readiness carries no health checks, so doctor and setup must derive their own")
	}

	// The checks a reader needs are all present.
	for _, id := range []string{"encryption_key", "cleanup", "events", "socket", "providers"} {
		findCheck(t, readiness.Checks, id)
	}
}

// TestProviderChecksDoNotReinterpretAvailability pins that there is one reading of
// the provider's own declaration.
//
// Doctor used to switch over availability strings itself, reaching verdicts the
// readiness screen reached differently from the same field. The provider check now
// reads summariseProviderReadiness, which is what the readiness screen reads.
func TestProviderChecksDoNotReinterpretAvailability(t *testing.T) {
	h := newReadinessHandler(t)
	ctx := context.Background()

	checks := h.providerChecks(ctx)
	if len(checks) == 0 {
		t.Fatal("no provider checks were produced")
	}
	// The first is the overall verdict, so a reader gets the answer before the list.
	if checks[0].ID != "providers" {
		t.Fatalf("the first provider check is %q, want the overall verdict", checks[0].ID)
	}

	readiness, err := h.HandleReadiness()
	if err != nil {
		t.Fatal(err)
	}
	// Every provider in the readiness list has a check, and the summaries agree —
	// because they come from the same function rather than two switches.
	for _, entry := range readiness.Providers {
		check := findCheck(t, readiness.Checks, "provider:"+entry.ID)
		if check.Summary != entry.Summary {
			t.Errorf("provider %s is summarised differently by the check and the readiness "+
				"entry:\n  check: %q\n  entry: %q", entry.ID, check.Summary, entry.Summary)
		}
	}
}
