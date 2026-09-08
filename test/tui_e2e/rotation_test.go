//go:build linux

package tui_e2e

import (
	"strings"
	"testing"
)

// TestTUIInstallationKeyRotationKeepsOldSecretsUsable is the encryption-key
// rotation row of the mutation matrix: rotating the installation key re-encrypts
// every stored provider credential, and a supervisor restart must still be able
// to decrypt and use them. The check is the observable one — the account's own
// credential is re-verified through the CLI, which only succeeds if the stored
// secret was re-encrypted under the new key and read back intact.
//
// The pre-rotation verification is the control: the credential is usable before
// the rotation, so the post-rotation failure mode being discriminated is not
// "the credential never worked" but "rotation made a previously-good secret
// unreadable."
func TestTUIInstallationKeyRotationKeepsOldSecretsUsable(t *testing.T) {
	requireE2E(t)
	f := newFixture(t)

	account := "acct-key-rotate"
	secret := "INSTALL-ROTATION-SECRET-VAL22"
	runCLIWithStdin(t, f, secret+"\n",
		"provider", "login", "mock", "--set", "account_id="+account,
		"--credential-stdin")

	// Control: the credential is usable before the rotation. If this fails the
	// rest of the test has nothing to isolate.
	out := runCLI(t, f, "provider", "verify", "mock", account)
	if !contains(strings.ToLower(out), "verified") && !contains(strings.ToLower(out), "checked") {
		t.Fatalf("precondition: the account's credential was not usable before rotation:\n%s", out)
	}

	// Rotate the installation encryption key. This stamps a new key version and
	// re-encrypts every stored secret under it.
	out = runCLI(t, f, "provider", "rotate-key", "--yes")
	if !contains(strings.ToLower(out), "rotated") {
		t.Fatalf("installation key rotation did not report success:\n%s", out)
	}

	// Restart. The restarted supervisor must decrypt the re-encrypted credential
	// from durable storage before it can present the account as usable.
	runCLI(t, f, "supervisor", "stop")
	runCLI(t, f, "supervisor", "start")

	// The durable, now re-encrypted credential must still verify — the whole
	// point of the rotation being lossless.
	out = runCLI(t, f, "provider", "verify", "mock", account)
	if !contains(strings.ToLower(out), "verified") && !contains(strings.ToLower(out), "checked") {
		t.Fatalf("the old credential became unusable after key rotation and restart:\n%s", out)
	}
}