package core

import (
	"fmt"
	"strings"
)

// UsabilitySource records where a value came from in a normal-user flow.
//
// The distinction is deliberately explicit. A discovered address and a typed
// address may both be valid strings, but they ask very different amounts of a
// user: one is a choice, the other is a fact the user had to know.
type UsabilitySource string

const (
	UsabilityInferred       UsabilitySource = "INFERRED"
	UsabilityDiscovered     UsabilitySource = "DISCOVERED"
	UsabilitySelected       UsabilitySource = "SELECTED"
	UsabilitySuggested      UsabilitySource = "SUGGESTED"
	UsabilityManualRequired UsabilitySource = "MANUAL_REQUIRED"
)

// UsabilityFieldKind identifies the kind of value being presented. It keeps
// the invariant focused on the things a normal user should not have to look up
// or type when Portico can discover or choose them.
type UsabilityFieldKind string

const (
	UsabilityID               UsabilityFieldKind = "id"
	UsabilityProviderResource UsabilityFieldKind = "provider_resource"
	UsabilityPort             UsabilityFieldKind = "port"
	UsabilityLocalEndpoint    UsabilityFieldKind = "local_endpoint"
	UsabilityExecutable       UsabilityFieldKind = "executable"
	UsabilityFilesystemPath   UsabilityFieldKind = "filesystem_path"
	UsabilityEnum             UsabilityFieldKind = "enum"
	UsabilityIntent           UsabilityFieldKind = "intent"
)

// UsabilityField is the audit record for one user-facing value.
//
// A manual value is allowed when the fact belongs to an external system or is
// outside Portico's discovery scope. It must carry the reason shown to the
// user; silently falling back to a raw field is the invariant violation.
type UsabilityField struct {
	ID           string
	Kind         UsabilityFieldKind
	Source       UsabilitySource
	ManualReason string
}

// Validate checks the invariant for one field.
func (f UsabilityField) Validate() error {
	if strings.TrimSpace(f.ID) == "" {
		return fmt.Errorf("usability field has no ID")
	}
	if !validUsabilitySource(f.Source) {
		return fmt.Errorf("usability field %q has unknown source %q", f.ID, f.Source)
	}
	if strings.TrimSpace(string(f.Kind)) == "" {
		return fmt.Errorf("usability field %q has no kind", f.ID)
	}
	if f.Source == UsabilityManualRequired && strings.TrimSpace(f.ManualReason) == "" {
		return fmt.Errorf("usability field %q is manual without a reason", f.ID)
	}
	return nil
}

// ValidateUsabilityFields checks a complete classification produced by a
// client flow. Keeping this small and dependency-free lets TUI and CLI tests
// use the same contract without importing either presentation layer.
func ValidateUsabilityFields(fields []UsabilityField) error {
	for _, field := range fields {
		if err := field.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validUsabilitySource(source UsabilitySource) bool {
	switch source {
	case UsabilityInferred, UsabilityDiscovered, UsabilitySelected,
		UsabilitySuggested, UsabilityManualRequired:
		return true
	default:
		return false
	}
}
