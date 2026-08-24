package core

import (
	"fmt"
	"strings"
)

// Environment values that are references, not secrets.
//
// A command origin can carry environment variables, and validateCommandEnvironment
// refuses any name that looks like a credential — telling the user to "use a
// provider or OS secret reference instead" when no such reference existed. So the
// advice named a feature that was not there, and a command needing an API key
// could not be run by Portico at all.
//
// A reference is the shape "env:NAME": the value is read from the supervisor's own
// environment when the command starts, and what the connection spec stores is the
// name of the variable to read. The secret is therefore never in the spec, never in
// a plan, never in an event, and never in a support export — only the fact that
// something is read from somewhere.
//
// This is deliberately narrow. A file reference would put a path in the spec and
// invite reading arbitrary files as the supervisor's user; a keyring reference
// needs a keyring the supervisor can reach unattended. Both are additions to this
// scheme rather than reasons to widen it now.

// envReferencePrefix marks a value as a reference to an environment variable
// rather than the value itself.
const envReferencePrefix = "env:"

// IsEnvReference reports whether a command environment value is a reference.
func IsEnvReference(value string) bool {
	return strings.HasPrefix(value, envReferencePrefix)
}

// EnvReferenceName is the variable a reference points at.
func EnvReferenceName(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(value, envReferencePrefix))
}

// NewEnvReference builds a reference to an environment variable.
func NewEnvReference(name string) string {
	return envReferencePrefix + strings.TrimSpace(name)
}

// validateCommandEnvironment refuses a literal credential and accepts a reference
// to one.
//
// The name test is unchanged: a variable called API_TOKEN carrying a literal is
// refused, because a spec is stored in the database, shown in previews and written
// into support exports, and a secret in it is a secret in all of those. The same
// name carrying "env:SOMETHING" is accepted, because what is stored is the name of
// a variable rather than its contents.
func validateCommandEnvironment(environment map[string]string) error {
	for name, value := range environment {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("command environment has a variable with no name")
		}
		if strings.ContainsAny(name, "= ") {
			return fmt.Errorf(
				"command environment variable %q is not a valid name: names cannot contain "+
					"spaces or equals signs", name)
		}

		if IsEnvReference(value) {
			referenced := EnvReferenceName(value)
			if referenced == "" {
				return fmt.Errorf(
					"command environment variable %q references nothing: write env:NAME to read "+
						"NAME from the supervisor's environment", name)
			}
			if strings.ContainsAny(referenced, "= ") {
				return fmt.Errorf(
					"command environment variable %q references %q, which is not a valid "+
						"variable name", name, referenced)
			}
			// A reference is safe whatever it is called: the spec stores the name
			// of a variable, not its contents.
			continue
		}

		upper := strings.ToUpper(name)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") ||
			strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "CREDENTIAL") ||
			strings.HasSuffix(upper, "_KEY") {
			return fmt.Errorf(
				"command environment variable %q looks like a credential, and a connection's "+
					"saved configuration is not a place to keep one: it is stored in the database, "+
					"shown in previews and written into support reports. Write env:%s instead, and "+
					"Portico will read %s from its own environment when the command starts",
				name, name, name)
		}
	}
	return nil
}

// ValidateCommandEnvironmentForInput is validateCommandEnvironment, exported so a
// client can refuse a bad value on the question that asked for it rather than at
// create time.
//
// It is the same function, not a copy: a second implementation of this rule would
// be a second answer, and the one that drifted would be the one deciding whether a
// credential reaches the database.
func ValidateCommandEnvironmentForInput(environment map[string]string) error {
	return validateCommandEnvironment(environment)
}

// RedactCommandEnvironment describes a command's environment without disclosing
// any of it.
//
// A reference is shown as the reference it is, because the variable name is not a
// secret and seeing it is how a user checks they wrote the right one. A literal is
// shown as present and nothing more: it passed validation, so it is not
// credential-shaped, but a connection's environment is still the user's business
// rather than something to print into a diagnostic.
func RedactCommandEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return nil
	}
	out := make(map[string]string, len(environment))
	for name, value := range environment {
		if IsEnvReference(value) {
			out[name] = value
			continue
		}
		out[name] = "(set)"
	}
	return out
}
