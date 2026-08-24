package core

import (
	"strings"
	"testing"
)

// Environment values that are references, not secrets.
//
// validateCommandEnvironment refused any credential-shaped variable name and told
// the user to "use a provider or OS secret reference instead" — when no such
// reference existed. The advice named a feature that was not there, and a command
// needing an API key could not be run by Portico at all.
//
// A reference is "env:NAME": the connection stores the name of a variable and the
// value is read from the supervisor's environment when the command starts. What
// must never happen is a literal credential reaching the spec, because a spec is
// stored in the database, shown in previews, and written into support exports.

// TestALiteralCredentialIsRefused pins the rule that already existed.
func TestALiteralCredentialIsRefused(t *testing.T) {
	for _, name := range []string{
		"API_TOKEN", "api_token", "MY_SECRET", "DB_PASSWORD",
		"AWS_CREDENTIAL", "SIGNING_KEY",
	} {
		err := validateCommandEnvironment(map[string]string{name: "literal-value"})
		if err == nil {
			t.Errorf("a literal in %q was accepted into a connection's saved configuration", name)
			continue
		}
		// The refusal has to teach the alternative, or the user has nowhere to go.
		if !strings.Contains(err.Error(), "env:") {
			t.Errorf("the refusal for %q does not name the reference syntax: %v", name, err)
		}
	}
}

// TestAReferenceIsAcceptedWhateverItIsCalled pins the point of the feature.
//
// The name test exists to keep secrets out of the spec. A reference puts no secret
// in the spec, so the name is irrelevant: refusing API_TOKEN=env:SOMETHING would
// refuse exactly the safe construction the error message recommends.
func TestAReferenceIsAcceptedWhateverItIsCalled(t *testing.T) {
	for _, name := range []string{"API_TOKEN", "MY_SECRET", "DB_PASSWORD", "SIGNING_KEY"} {
		if err := validateCommandEnvironment(map[string]string{
			name: NewEnvReference("PORTICO_SOURCE_VAR"),
		}); err != nil {
			t.Errorf("a reference in %q was refused: %v", name, err)
		}
	}
}

// TestAnOrdinaryValueIsAccepted pins that this is not a blanket ban.
func TestAnOrdinaryValueIsAccepted(t *testing.T) {
	if err := validateCommandEnvironment(map[string]string{
		"PORT":      "8080",
		"NODE_ENV":  "production",
		"LOG_LEVEL": "debug",
	}); err != nil {
		t.Fatalf("ordinary configuration was refused: %v", err)
	}
}

// TestAMalformedReferenceIsRefused pins that the syntax has to mean something.
func TestAMalformedReferenceIsRefused(t *testing.T) {
	for _, value := range []string{"env:", "env:   ", "env:HAS SPACE", "env:HAS=EQUALS"} {
		if err := validateCommandEnvironment(map[string]string{"THING": value}); err == nil {
			t.Errorf("the malformed reference %q was accepted", value)
		}
	}
}

// TestAMalformedNameIsRefused pins that a name has to be usable as one.
func TestAMalformedNameIsRefused(t *testing.T) {
	for _, name := range []string{"", "  ", "HAS SPACE", "HAS=EQUALS"} {
		if err := validateCommandEnvironment(map[string]string{name: "value"}); err == nil {
			t.Errorf("the malformed variable name %q was accepted", name)
		}
	}
}

// TestReferenceHelpersRoundTrip pins the shape the origin manager relies on.
func TestReferenceHelpersRoundTrip(t *testing.T) {
	ref := NewEnvReference("SOURCE_VAR")
	if !IsEnvReference(ref) {
		t.Fatalf("%q is not recognised as a reference", ref)
	}
	if got := EnvReferenceName(ref); got != "SOURCE_VAR" {
		t.Fatalf("EnvReferenceName(%q) = %q", ref, got)
	}
	// A literal is not a reference, however it looks.
	for _, literal := range []string{"", "8080", "environment", "envelope", "ENV:UPPER"} {
		if IsEnvReference(literal) {
			t.Errorf("%q was treated as a reference", literal)
		}
	}
}

// TestRedactionDisclosesNothing pins what may be shown about an environment.
//
// A reference is shown in full: the variable's name is not a secret, and seeing it
// is how a user checks they wrote the right one. A literal is shown as present and
// nothing more — it passed validation, so it is not credential-shaped, but a
// connection's environment is still the user's business rather than something to
// print into a diagnostic they may attach to a bug report.
func TestRedactionDisclosesNothing(t *testing.T) {
	redacted := RedactCommandEnvironment(map[string]string{
		"PORT":      "8080",
		"API_TOKEN": NewEnvReference("REAL_TOKEN_VAR"),
	})

	if got := redacted["API_TOKEN"]; got != "env:REAL_TOKEN_VAR" {
		t.Errorf("the reference was not shown as itself: %q", got)
	}
	if got := redacted["PORT"]; got == "8080" {
		t.Errorf("a literal value was disclosed: %q", got)
	}
	if got := redacted["PORT"]; got != "(set)" {
		t.Errorf("a literal is described as %q, want (set)", got)
	}

	// An empty environment redacts to nothing rather than an empty map that a
	// renderer would draw a heading for.
	if RedactCommandEnvironment(nil) != nil {
		t.Error("an empty environment produced a non-nil redaction")
	}
}

// TestValidationIsSharedWithClients pins that there is one rule.
//
// The exported entry exists so a client can refuse a bad value on the question
// that asked for it. It must be the same function: a second implementation would
// be a second answer, and the one that drifted would be the one deciding whether a
// credential reaches the database.
func TestValidationIsSharedWithClients(t *testing.T) {
	bad := map[string]string{"API_TOKEN": "literal"}
	internal := validateCommandEnvironment(bad)
	exported := ValidateCommandEnvironmentForInput(bad)
	if (internal == nil) != (exported == nil) {
		t.Fatal("the exported validator disagrees with the internal one")
	}
	if internal.Error() != exported.Error() {
		t.Fatalf("the two validators give different reasons:\n%v\n%v", internal, exported)
	}
}
