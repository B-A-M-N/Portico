package profile

import (
	"fmt"
	"sort"
)

// Definition is the provider-neutral contract for a connection profile.
// Providers remain responsible for carrying traffic; definitions decide what
// the traffic means and which transport capabilities it requires.
type Definition interface {
	Kind() ProfileKind
	Name() string
	Validate(Profile) error
	Compatible(TransportCapabilities) bool
}

// Descriptor is a small definition implementation for profiles whose
// validation and transport filter are pure functions.
type Descriptor struct {
	ProfileKind  ProfileKind
	DisplayName  string
	Capabilities TransportCapabilities
	ValidateFunc func(Profile) error
}

func (d Descriptor) Kind() ProfileKind { return d.ProfileKind }

func (d Descriptor) Name() string {
	if d.DisplayName != "" {
		return d.DisplayName
	}
	return string(d.ProfileKind)
}

func (d Descriptor) Validate(p Profile) error {
	if p.Kind != d.ProfileKind {
		return fmt.Errorf("profile kind %q does not match %q", p.Kind, d.ProfileKind)
	}
	if d.ValidateFunc != nil {
		return d.ValidateFunc(p)
	}
	return nil
}

func (d Descriptor) Compatible(caps TransportCapabilities) bool {
	required := d.Capabilities
	return (!required.HTTP || caps.HTTP) &&
		(!required.TCP || caps.TCP) &&
		(!required.PublicExposure || caps.PublicExposure) &&
		(!required.PrivateExposure || caps.PrivateExposure) &&
		(!required.StableHostname || caps.StableHostname) &&
		(!required.Streaming || caps.Streaming) &&
		(!required.Authentication || caps.Authentication) &&
		(!required.RemoteForward || caps.RemoteForward) &&
		(!required.DynamicForward || caps.DynamicForward) &&
		(!required.NoAccount || caps.NoAccount)
}

// Registry stores profile definitions independently of transport providers.
// It is intentionally small: the supervisor can use it to filter transports
// before planning, while execution remains owned by the provider registry.
type Registry struct {
	definitions map[ProfileKind]Definition
}

func NewRegistry(definitions ...Definition) *Registry {
	r := &Registry{definitions: make(map[ProfileKind]Definition, len(definitions))}
	for _, definition := range definitions {
		if definition != nil {
			r.definitions[definition.Kind()] = definition
		}
	}
	return r
}

func (r *Registry) Register(definition Definition) error {
	if definition == nil || definition.Kind() == "" {
		return fmt.Errorf("profile definition is required")
	}
	if _, exists := r.definitions[definition.Kind()]; exists {
		return fmt.Errorf("profile kind %q is already registered", definition.Kind())
	}
	r.definitions[definition.Kind()] = definition
	return nil
}

func (r *Registry) Lookup(kind ProfileKind) (Definition, bool) {
	definition, ok := r.definitions[kind]
	return definition, ok
}

// Kinds returns registered profile kinds in stable order for UI choices.
func (r *Registry) Kinds() []ProfileKind {
	kinds := make([]ProfileKind, 0, len(r.definitions))
	for kind := range r.definitions {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}
