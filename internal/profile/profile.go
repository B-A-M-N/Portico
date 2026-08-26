// Package profile describes provider-neutral connection intent.
//
// Providers answer how traffic moves. Profiles answer what the traffic means
// and what local gateway policy it needs. Keeping these types independent of
// core.Provider makes an OpenAI-compatible service reusable with any transport
// that can carry HTTP streaming traffic.
package profile

import (
	"github.com/B-A-M-N/portico/internal/core"
)

// ProfileKind identifies the service intent. The persisted vocabulary is
// owned by core (core.ProfileKind constants) — this alias exists so the
// profile package can operate on the same type without a second authority
// whose literals could drift.
type ProfileKind = core.ProfileKind

const (
	ProfileOpenAICompatible = core.ProfileOpenAICompatible
	// ProfileOpenAIMCP is the Secure MCP workload consumed by OpenAI's
	// client-mediated tunnel. It is a profile intent; the client tunnel is
	// only one transport that can carry it.
	ProfileOpenAIMCP  = core.ProfileOpenAIMCP
	ProfileWebService = core.ProfileWebService
)

type ProfileID string
type ProviderID string

// Profile is provider-neutral intent. TransportProviderID is resolved by the
// planner; it is retained here only so a caller can request a preferred
// transport without making the profile implementation transport-specific.
type Profile struct {
	ID                  ProfileID
	Name                string
	Kind                ProfileKind
	Target              TargetSpec
	Gateway             GatewaySpec
	TransportProviderID ProviderID
}

// TargetSpec describes the local service the profile fronts.
type TargetSpec struct {
	Host     string
	Port     int
	Protocol string
	BasePath string
}

// GatewaySpec configures the local Portico gateway.
type GatewaySpec struct {
	Enabled      bool
	AuthRequired bool
	AuthTokens   []string
	AllowSSE     bool
}

// TransportCapabilities are the provider properties a profile can filter on.
type TransportCapabilities struct {
	HTTP            bool
	TCP             bool
	PublicExposure  bool
	PrivateExposure bool
	StableHostname  bool
	Streaming       bool
	Authentication  bool
	RemoteForward   bool
	DynamicForward  bool
	NoAccount       bool
}
