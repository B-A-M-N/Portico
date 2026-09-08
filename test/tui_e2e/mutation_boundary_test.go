package tui_e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTUIAccountRemovalRefusalThenSuccess drives the account mutation boundary
// through the real binary: an account still used by a connection must be named
// by the removal preview and refuse, and after the dependent connection is
// deleted the same removal must succeed. Both halves assert the visible screen
// and the resulting provider state, not IPC replies.
func TestTUIAccountRemovalRefusalThenSuccess(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer fixtureServer.Close()

	// A mock account, created through the provider-declared CLI flow with the
	// secure stdin channel, is the only account a hermetic test can create and
	// destroy.
	runCLIWithStdin(t, f, "mock-secret-value\n",
		"provider", "login", "mock", "--set", "account_id=acct-removal",
		"--credential-stdin")
	out := runCLI(t, f, "provider", "list", "--json")
	if !contains(out, "acct-removal") {
		t.Fatalf("precondition: the mock account is absent from the provider list:\n%s", out)
	}

	// A connection bound to the account makes the removal refusal real rather
	// than vacuous.
	runCLI(t, f, "create", "removal-dep", "--provider", "mock",
		"--source", "http://"+fixtureServer.Listener.Addr().String(),
		"--source-type", "existing_service", "--health-enabled=false",
		"--account-id", "acct-removal")

	s := f.startTUI(120, 40)
	s.waitFor("CONNECTIONS")
	s.waitFor("removal-dep")

	// Providers screen: select the mock provider, then its account row. The
	// selected account row is the one that draws the account marker, so the
	// walk ends on visible, physical evidence rather than a guess.
	s.send("p")
	s.waitFor("PROVIDERS")
	s.waitFor("acct-removal")
	walkToAccount(s, "acct-removal")
	// The refusal: x previews the removal and names the dependency. The
	// preview is asynchronous, so the first frame says it is still checking;
	// waiting for the settled answer — never a sleep — is what makes the
	// assertion about the answer rather than about a frame that happened to
	// render first. The pending wording is deliberately not in the set: a
	// frame can lag the model under redraw coalescing, so only a settled
	// answer proves the preview arrived.
	s.send("x")
	s.waitFor("REMOVE ACCOUNT")
	s.waitForEither("cannot be removed", "Portico will forget the credential")
	if !contains(s.screen(), "removal-dep") {
		t.Fatalf("removal preview does not name the dependent connection:\n%s", s.debug())
	}
	// Confirm is refused: the account survives and the screen says why.
	s.send("enter")
	s.waitFor("cannot be removed")
	s.send("esc")
	s.waitFor("PROVIDERS")
	if !contains(s.screen(), "acct-removal") {
		t.Fatalf("the account vanished despite a refused removal:\n%s", s.debug())
	}

	// Remove the dependency through the real delete flow. Home's empty state
	// ("Nothing is published yet.") is the user-visible proof the deletion
	// took effect; there is no list header left to wait for.
	s.escUntil("CONNECTIONS")
	s.waitFor("removal-dep")
	s.send("d")
	s.waitFor("EXACTLY THESE STEPS")
	s.send("enter")
	s.waitFor("Status: Completed")
	s.send("esc")
	s.waitFor("Nothing is published yet.")

	// The identical removal now succeeds and the row disappears. Enter is
	// pressed only once the preview has settled, and the confirmation is
	// waited for by its own status — the providers frame that first follows
	// can still show the pre-removal snapshot, so asserting absence there
	// raced the refetch.
	s.send("p")
	s.waitFor("PROVIDERS")
	s.waitFor("acct-removal")
	walkToAccount(s, "acct-removal")
	s.send("x")
	s.waitFor("REMOVE ACCOUNT")
	s.waitForEither("cannot be removed", "Portico will forget the credential")
	s.send("enter")
	s.waitFor("Removed acct-removal")
	s.waitForEither("Nothing is published yet.", "PROVIDERS")
	// The absence check targets the row form ("acct-removal — status"), not
	// the bare substring: the success status "Removed acct-removal." itself
	// contains the account id and legitimately stays on screen. The row would
	// render with an em-dash separator the status line never carries.
	s.waitForNot("acct-removal —")
	if contains(s.screen(), "acct-removal —") {
		t.Fatalf("the account survived a confirmed removal:\n%s", s.debug())
	}
	s.send("ctrl-c")
	s.waitExit()

	// The CLI's independent view agrees with the screen.
	out = runCLI(t, f, "provider", "list", "--json")
	if contains(out, "acct-removal") {
		t.Fatalf("removed account is still present in the provider list:\n%s", out)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || stringsIndex(haystack, needle) >= 0
}

// walkToAccount presses Down until the named account row draws the selection
// marker, failing with the screen if the cursor never reaches it.
//
// A dwell after each press is load-bearing: Bubble Tea coalesces redraws, so
// Down keys sent back-to-back can all be consumed between two rendered frames
// and the intermediate cursor positions never appear on screen at all — the
// walk would then stride straight past the account row while the parsed
// screen still showed the start of the list. Spacing the presses lets every
// position render before the walk looks again.
func walkToAccount(s *session, accountID string) {
	t := s.t
	for range 40 {
		if contains(s.screen(), "\u25b8 "+accountID) || contains(s.screen(), "> "+accountID) {
			return
		}
		s.send("down")
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("the cursor never reached account %q:\n%s", accountID, s.debug())
}

func stringsIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestTUICredentialReplacementThroughRealSurface drives credential replacement
// on the Providers screen: the new secret is collected through the masked
// field, validated, and the account keeps its identity afterward.
func TestTUICredentialReplacementThroughRealSurface(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	runCLIWithStdin(t, f, "first-secret-value\n",
		"provider", "login", "mock", "--set", "account_id=acct-replace",
		"--credential-stdin")

	s := f.startTUI(120, 40)
	s.waitFor("Nothing is published yet.")
	s.send("p")
	s.waitFor("PROVIDERS")
	s.waitFor("acct-replace")
	walkToAccount(s, "acct-replace")

	// Replace the credential: c opens the setup form in replacement mode with
	// every non-secret field frozen.
	s.send("c")
	s.waitFor("Replace the credential for acct-replace")
	canary := "TUI-REPLACE-CANARY-DO-NOT-PRINT"
	s.typeSecretText(canary)
	s.waitFor("••••••••")
	if contains(s.screen(), canary) || contains(string(s.rawOutput()), canary) {
		t.Fatal("replacement secret appeared in the visible or raw PTY output")
	}
	s.send("enter")
	// The confirm step restates the consequences; enter commits the rotation.
	s.waitFor("Press enter to confirm")
	s.send("enter")
	// The screen reports success without ever printing the secret.
	s.waitFor("PROVIDERS")
	if contains(s.screen(), canary) || contains(string(s.rawOutput()), canary) {
		t.Fatal("replacement secret survived leaving the form")
	}
	if !contains(s.screen(), "acct-replace") {
		t.Fatalf("the account identity did not survive credential replacement:\n%s", s.debug())
	}
	s.send("ctrl-c")
	s.waitExit()

	// Independent verification: re-verifying the account through the CLI must
	// succeed, which only the new credential can make true. The exact verdict
	// word differs between authoritative and declared-strength verifiers, so
	// the assertion accepts either honest answer but not an error.
	out := runCLI(t, f, "provider", "verify", "mock", "acct-replace")
	if !contains(strings.ToLower(out), "verified") && !contains(strings.ToLower(out), "checked") {
		t.Fatalf("re-verification failed after replacement:\n%s", out)
	}
}
