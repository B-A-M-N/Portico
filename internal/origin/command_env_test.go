//go:build linux

package origin

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Resolving an environment reference at start.
//
// This is the one place a reference becomes a value. The connection stores
// "env:NAME"; the child receives the contents of NAME. If that resolution happened
// anywhere earlier the plaintext would be in the spec, and therefore in the
// database, in previews and in support exports.

// envValue reads one variable out of the block allowlistedEnv produced.
func envValue(block []string, name string) (string, bool) {
	for _, entry := range block {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value, true
		}
	}
	return "", false
}

// TestAReferenceIsResolvedIntoTheChildEnvironment pins the whole point.
func TestAReferenceIsResolvedIntoTheChildEnvironment(t *testing.T) {
	t.Setenv("PORTICO_TEST_SOURCE_TOKEN", "the-real-secret")

	block := allowlistedEnv(map[string]string{
		"API_TOKEN": core.NewEnvReference("PORTICO_TEST_SOURCE_TOKEN"),
	})

	got, ok := envValue(block, "API_TOKEN")
	if !ok {
		t.Fatalf("the child did not receive API_TOKEN at all: %v", block)
	}
	if got != "the-real-secret" {
		t.Fatalf("API_TOKEN = %q, want the referenced value", got)
	}
	// The reference itself is not passed through: a child reading "env:NAME" as a
	// credential would fail obscurely.
	if strings.Contains(got, "env:") {
		t.Errorf("the reference was passed through unresolved: %q", got)
	}
}

// TestADanglingReferenceIsOmitted pins the failure mode.
//
// A reference to a variable the supervisor does not have cannot be honoured. It is
// omitted rather than passed through as the literal string "env:NAME", which the
// child would read as a nonsense credential and fail on in its own way — a
// confusing error a long way from the cause.
func TestADanglingReferenceIsOmitted(t *testing.T) {
	block := allowlistedEnv(map[string]string{
		"API_TOKEN": core.NewEnvReference("PORTICO_TEST_DEFINITELY_NOT_SET"),
	})

	if value, ok := envValue(block, "API_TOKEN"); ok {
		t.Fatalf("a dangling reference produced API_TOKEN=%q", value)
	}
	for _, entry := range block {
		if strings.Contains(entry, "env:") {
			t.Errorf("an unresolved reference reached the child: %q", entry)
		}
	}
}

// TestALiteralIsPassedThrough pins that ordinary configuration still works.
func TestALiteralIsPassedThrough(t *testing.T) {
	block := allowlistedEnv(map[string]string{"NODE_ENV": "production"})
	if got, _ := envValue(block, "NODE_ENV"); got != "production" {
		t.Fatalf("NODE_ENV = %q", got)
	}
}

// TestTheSupervisorEnvironmentIsStillNotInherited pins that adding reference
// resolution did not widen what a child sees.
//
// The allowlist exists because the supervisor's environment holds provider
// credentials. A reference reads one named variable deliberately; it must not
// become a way for everything else to leak in.
func TestTheSupervisorEnvironmentIsStillNotInherited(t *testing.T) {
	t.Setenv("PORTICO_TEST_UNRELATED_SECRET", "must-not-leak")
	t.Setenv("PORTICO_TEST_SOURCE_TOKEN", "referenced")

	block := allowlistedEnv(map[string]string{
		"API_TOKEN": core.NewEnvReference("PORTICO_TEST_SOURCE_TOKEN"),
	})

	for _, entry := range block {
		if strings.Contains(entry, "must-not-leak") {
			t.Fatalf("an unrelated variable leaked into the child: %q", entry)
		}
		if strings.HasPrefix(entry, "PORTICO_TEST_UNRELATED_SECRET=") {
			t.Fatalf("an unrelated variable was inherited: %q", entry)
		}
	}
	// The referenced one arrives under the name the command asked for, not under
	// the source variable's name.
	if _, ok := envValue(block, "PORTICO_TEST_SOURCE_TOKEN"); ok {
		t.Error("the source variable itself was passed to the child")
	}
	if got, _ := envValue(block, "API_TOKEN"); got != "referenced" {
		t.Errorf("API_TOKEN = %q", got)
	}
}
