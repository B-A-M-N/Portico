package core

import "time"

// CapabilitySupport describes whether a capability is supported.
type CapabilitySupport struct {
	Supported bool
	Stability Stability
	Requires  []Requirement
	Notes     []string
}

// Stability describes how stable a capability is.
type Stability string

const (
	StabilityStable       Stability = "stable"
	StabilityBeta         Stability = "beta"
	StabilityExperimental Stability = "experimental"
)

// Requirement describes a requirement for a capability.
type Requirement struct {
	Resource string
	Reason   string
}

// Capabilities represents provider capabilities.
type Capabilities struct {
	TemporaryAddresses CapabilitySupport
	CustomHostnames    CapabilitySupport
	PrivateExposure    CapabilitySupport
	ManagedDNS         CapabilitySupport
	BuiltInProtection  []ProtectionCapability
	Protocols          map[Protocol]ProtocolCapability
	Telemetry          TelemetryCapability
	Redundancy         RedundancyCapability
	RemoteConfig       CapabilitySupport
	Expiration         ExpirationCapability
	Constraints        []CapabilityConstraint
}

// ProtectionCapability describes a protection capability.
type ProtectionCapability struct {
	Kind      ProtectionKind
	Supported bool
	Stability Stability
	Notes     []string
}

// ProtocolCapability describes protocol support.
type ProtocolCapability struct {
	Supported   bool
	Public      bool
	Private     bool
	Constraints []CapabilityConstraint
}

// CapabilityConstraint describes a constraint on a capability with structured matching fields.
type CapabilityConstraint struct {
	Code          string
	SourceKinds   []SourceKind
	Protocols     []Protocol
	MCPTransports []MCPTransport
	ExposureModes []ExposureMode
	Protection    []ProtectionKind
	Supported     bool
	Requirement   *Requirement
	Message       string
}

// Matches evaluates whether this constraint applies to the given profile.
func (c *CapabilityConstraint) Matches(profile *ConnectionProfile) bool {
	if c.Code == "" {
		return false
	}

	// Check source kinds
	if len(c.SourceKinds) > 0 {
		matchesSource := false
		for _, sk := range c.SourceKinds {
			if profile.Source.Kind == sk {
				matchesSource = true
				break
			}
		}
		if !matchesSource {
			return false
		}
	}

	// Check protocols
	if len(c.Protocols) > 0 {
		matchesProtocol := false
		for _, p := range c.Protocols {
			if profile.Exposure.Protocol == p {
				matchesProtocol = true
				break
			}
		}
		if !matchesProtocol {
			return false
		}
	}

	// Check MCP transports
	if len(c.MCPTransports) > 0 && profile.Source.MCP != nil {
		matchesMCP := false
		for _, mt := range c.MCPTransports {
			if profile.Source.MCP.Transport == mt {
				matchesMCP = true
				break
			}
		}
		if !matchesMCP {
			return false
		}
	}

	// Check exposure modes
	if len(c.ExposureModes) > 0 {
		matchesExposure := false
		for _, em := range c.ExposureModes {
			if profile.Exposure.Mode == em {
				matchesExposure = true
				break
			}
		}
		if !matchesExposure {
			return false
		}
	}

	// Check protection kinds
	if len(c.Protection) > 0 {
		matchesProtection := false
		for _, pk := range c.Protection {
			if profile.Protection.Kind == pk {
				matchesProtection = true
				break
			}
		}
		if !matchesProtection {
			return false
		}
	}

	return true
}

// ConstraintViolation describes a constraint that prevents a capability from being used.
type ConstraintViolation struct {
	Code        string
	Message     string
	Explanation string
}

// CheckConstraints evaluates all constraints against a profile and returns violations.
func CheckConstraints(constraints []CapabilityConstraint, profile *ConnectionProfile) []ConstraintViolation {
	var violations []ConstraintViolation
	for _, c := range constraints {
		if c.Matches(profile) {
			if c.Supported {
				// No violation
				continue
			}
			msg := c.Message
			if msg == "" {
				msg = "capability not available for this configuration"
			}
			violations = append(violations, ConstraintViolation{
				Code:        c.Code,
				Message:     msg,
				Explanation: msg,
			})
		}
	}
	return violations
}

// TelemetryCapability describes telemetry support.
type TelemetryCapability struct {
	Supported         bool
	RequestCounts     bool
	LatencyHistograms bool
	ErrorCounts       bool
	Stability         Stability
}

// RedundancyCapability describes redundancy support.
type RedundancyCapability struct {
	Supported     bool
	MaxConnectors int
	Stability     Stability
}

// ExpirationCapability describes expiration support.
type ExpirationCapability struct {
	Supported   bool
	MaxDuration time.Duration
	Stability   Stability
}

// ProviderIdentity identifies a provider.
type ProviderIdentity struct {
	ID          ProviderID
	Name        string
	DisplayName string
	LogoURL     string
}
