package core

import "context"

// SetupField describes one input a provider needs in order to configure an
// account. Providers declare their requirements so the UI can render a setup
// form without knowing what any particular provider's fields mean.
//
// Before this existed the setup screen hardcoded Cloudflare's account and zone
// semantics, and the backend refused every other provider outright, so adding a
// provider meant editing the TUI.
type SetupField struct {
	ID          string
	Label       string
	Description string
	// Secret marks a value that must be masked on screen, kept out of logs and
	// errors, and cleared from memory when the flow ends.
	Secret bool
	// Required marks a field the provider cannot be configured without. An
	// optional field must state what is lost by skipping it.
	Required bool
	// Placeholder is an example value, never a default to submit.
	Placeholder string
}

// SetupKind states what completing a flow actually does.
//
// Without this distinction every flow looks like a form that saves something,
// and a provider whose credential Portico cannot store would report a
// successful setup that changed nothing.
type SetupKind string

const (
	// SetupAccount stores an account that Portico will use on the user's
	// behalf.
	SetupAccount SetupKind = "account"
	// SetupGuidance describes what the user must configure outside Portico.
	// Portico cannot hold the credential for such a provider — typically
	// because the client reads it from the supervisor's own environment — so
	// the flow is rendered as instructions and never submitted.
	SetupGuidance SetupKind = "guidance"
)

// SetupFlow is a provider's complete declarative setup description.
type SetupFlow struct {
	// Kind states whether completing this flow stores an account or only tells
	// the user what to do elsewhere. The zero value is SetupAccount, which
	// keeps a provider that predates this field behaving as it did.
	Kind SetupKind
	// Summary explains, in plain language, what configuring this provider
	// enables.
	Summary string
	Fields  []SetupField
	// GuidanceReason states why Portico cannot store this provider's
	// credential. Only meaningful when Kind is SetupGuidance.
	//
	// The provider declares it because only the provider knows how its client
	// obtains a credential; a reason asserted centrally would be a guess that
	// happened to be right for the first such provider.
	GuidanceReason string
	// IdentityField names the field whose value identifies the account. Empty
	// means the provider has a single implicit account.
	IdentityField string
	// SecretField names the field carrying the credential.
	SecretField string
	// CapabilityNotes explain what becomes available at each level of
	// configuration, so a user can decide how far to go.
	CapabilityNotes []string
}

// StoresAccount reports whether submitting this flow persists anything.
func (f SetupFlow) StoresAccount() bool {
	return f.Kind == "" || f.Kind == SetupAccount
}

// SetupValidation is the result of checking a provider credential.
type SetupValidation struct {
	// MissingPermissions names capabilities the credential lacks, so a token
	// that works but cannot do the job says which part is missing.
	MissingPermissions []string
	// Notes carry provider-specific detail worth showing the user.
	Notes []string
}

// SetupValidator is an optional capability. A provider implementing it can
// confirm a credential actually works before Portico records the account as
// authenticated.
//
// A provider that does not implement it is not thereby unusable: its account is
// stored as AccountPending and reported as unverified. What must never happen
// is an unchecked credential being presented as a working one.
type SetupValidator interface {
	ValidateSetup(ctx context.Context, values map[string]string) (SetupValidation, error)
}

// ProviderSetup is an optional capability. A provider implementing it can be
// configured through the generic setup flow; one that does not cannot be
// configured at all, which the UI must state rather than presenting an empty
// form.
type ProviderSetup interface {
	SetupFlow() SetupFlow
}
