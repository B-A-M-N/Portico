package tui

import "github.com/B-A-M-N/portico/internal/ipc"

// What the provider can actually do, for the edit screen.
//
// Protection was a fixed cycle through three modes, which offered a policy to a
// provider that cannot enforce one and would fail at apply. Exposure mode was not
// offered at all. Accounts were cycled as opaque IDs.
//
// All three answers already exist: the provider declares its capabilities and its
// accounts in the snapshot. The edit state holds what the root model resolved for
// it, for the same reason the Inspect screen does — the snapshot belongs to the
// root model, and resolving it during render would write to a discarded copy.

// setProviderContext installs the capabilities and accounts of the provider this
// connection uses.
func (s *editState) setProviderContext(caps *ipc.CapabilitySetDTO, accounts []ipc.ProviderAccountDTO) {
	s.caps = caps
	s.usable = accounts
}

// exposureModeChoices are the address behaviours this provider can deliver.
//
// A mode it cannot deliver is not offered. The current mode is always included,
// because a connection already using a mode must be able to keep it even if the
// provider's declaration has since narrowed — otherwise opening the edit screen
// would silently propose changing something the user did not ask about.
func (s *editState) exposureModeChoices() []string {
	current := ""
	if exposed := s.detail.DesiredSpec.ServiceExposure; exposed != nil {
		current = exposed.Exposure.Mode
	}
	if s.caps == nil {
		if current == "" {
			return nil
		}
		return []string{current}
	}

	var choices []string
	if s.caps.TemporaryAddresses {
		choices = append(choices, "temporary_public")
	}
	if s.caps.CustomHostnames {
		choices = append(choices, "permanent_public")
	}
	if s.caps.PrivateExposure {
		choices = append(choices, "private")
	}
	return withCurrent(choices, current)
}

// protectionChoices are the access policies this provider can enforce.
//
// "none" is always available: it is the absence of a policy, which needs no
// provider capability. Everything else is what the provider says it can apply.
func (s *editState) protectionChoices() []string {
	current := ""
	if exposed := s.detail.DesiredSpec.ServiceExposure; exposed != nil {
		current = exposed.Protection.Kind
	}
	choices := []string{"none"}
	if s.caps != nil {
		choices = append(choices, s.caps.ProtectionModes...)
	}
	return withCurrent(dedupeStrings(choices), current)
}

// effectiveExposureMode is the mode this edit would end with.
func (s *editState) effectiveExposureMode() string {
	if s.exposureMode != nil {
		return *s.exposureMode
	}
	if exposed := s.detail.DesiredSpec.ServiceExposure; exposed != nil {
		return exposed.Exposure.Mode
	}
	return ""
}

// accountLabelFor names an account the way the provider labelled it, with its
// status when the status is something the user should know.
//
// The screen cycled opaque account IDs, which is the identity to send and the
// wrong thing to read: a user choosing between "acct-9f2b1c" and "acct-2e7d40"
// is choosing at random.
func (s *editState) accountLabelFor(id string) string {
	if id == "" {
		return ""
	}
	for _, account := range s.usable {
		if account.ID != id {
			continue
		}
		label := account.Label
		if label == "" {
			label = id
		}
		if note := accountStatusNote(account); note != "" {
			label += " — " + note
		}
		return label
	}
	// An account referenced by the connection and absent from the provider's list
	// has been removed. Saying so explains why the connection cannot open.
	return id + " (no longer configured)"
}

// withCurrent guarantees the current value is among the choices, so opening the
// edit screen never proposes a change the user did not make.
func withCurrent(choices []string, current string) []string {
	if current == "" {
		return choices
	}
	for _, choice := range choices {
		if choice == current {
			return choices
		}
	}
	return append([]string{current}, choices...)
}

// dedupeStrings removes repeats while preserving order.
func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
