package core

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

// SetupFlow is a provider's complete declarative setup description.
type SetupFlow struct {
	// Summary explains, in plain language, what configuring this provider
	// enables.
	Summary string
	Fields  []SetupField
	// CapabilityNotes explain what becomes available at each level of
	// configuration, so a user can decide how far to go.
	CapabilityNotes []string
}

// ProviderSetup is an optional capability. A provider implementing it can be
// configured through the generic setup flow; one that does not cannot be
// configured at all, which the UI must state rather than presenting an empty
// form.
type ProviderSetup interface {
	SetupFlow() SetupFlow
}
