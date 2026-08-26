package controller

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	profilepkg "github.com/B-A-M-N/portico/internal/profile"
)

// validateProfileTransport is the boundary between provider-neutral service
// intent and the selected transport. The provider registry answers whether a
// transport can execute a connection kind; this registry answers whether it
// can carry the selected workload's protocol semantics.
func (c *Controller) validateProfileTransport(p *core.ConnectionProfile, caps core.Capabilities) error {
	if c.profileRegistry == nil {
		return nil
	}
	kind := profilepkg.ProfileKind(p.ProfileKind)
	if kind == "" {
		kind = derivedProfileKind(p)
	}
	definition, ok := c.profileRegistry.Lookup(kind)
	if !ok {
		return core.ErrValidation(fmt.Sprintf("profile kind %q is not registered", kind))
	}

	intent := profilepkg.Profile{
		ID:                  profilepkg.ProfileID(p.ID),
		Name:                p.Name,
		Kind:                kind,
		Target:              targetForProfile(p),
		TransportProviderID: profilepkg.ProviderID(p.Driver.ProviderID),
		Gateway: profilepkg.GatewaySpec{
			Enabled:      kind == profilepkg.ProfileOpenAICompatible || kind == profilepkg.ProfileOpenAIMCP,
			AuthRequired: kind == profilepkg.ProfileOpenAICompatible,
			AllowSSE:     kind == profilepkg.ProfileOpenAICompatible || kind == profilepkg.ProfileOpenAIMCP,
		},
	}
	if err := definition.Validate(intent); err != nil {
		return core.ErrValidation(fmt.Sprintf("profile %q is invalid: %v", kind, err))
	}
	// A client-mediated connection's transport is the OpenAI client itself;
	// the provider capability contract for this legacy/private kind predates
	// profile streaming metadata. Its protocol validation happens in the
	// client transport and MCP health path. Service exposures, however, must
	// prove that the selected public/private transport preserves streaming.
	if p.EffectiveKind() != core.ConnectionClientTunnel && !definition.Compatible(transportCapabilities(caps)) {
		return core.ErrValidation(fmt.Sprintf("transport %q cannot carry profile %q", p.Driver.ProviderID, kind))
	}
	return nil
}

func derivedProfileKind(p *core.ConnectionProfile) profilepkg.ProfileKind {
	if p == nil {
		return profilepkg.ProfileWebService
	}
	switch p.EffectiveKind() {
	case core.ConnectionClientTunnel:
		return profilepkg.ProfileOpenAIMCP
	case core.ConnectionServiceExposure:
		if p.Spec.ServiceExposure != nil && p.Spec.ServiceExposure.Source.MCP != nil {
			return profilepkg.ProfileOpenAIMCP
		}
	}
	return profilepkg.ProfileWebService
}

func targetForProfile(p *core.ConnectionProfile) profilepkg.TargetSpec {
	if p == nil {
		return profilepkg.TargetSpec{}
	}
	var protocol, address string
	if p.Spec.ClientTunnel != nil {
		protocol = string(core.ProtocolHTTP)
		address = p.Spec.ClientTunnel.MCP.Endpoint
	} else {
		source := p.GetSource()
		switch source.Kind {
		case core.SourceExisting:
			if source.Existing != nil {
				protocol, address = string(source.Existing.Protocol), source.Existing.Address
			}
		case core.SourceDirectory:
			protocol, address = string(core.ProtocolHTTP), net.JoinHostPort("127.0.0.1", strconv.Itoa(source.Directory.ListenPort))
		case core.SourceCommand:
			if source.Command != nil {
				protocol, address = string(source.Command.Protocol), net.JoinHostPort("127.0.0.1", strconv.Itoa(source.Command.Port))
			}
		case core.SourceMCP:
			if source.MCP != nil {
				protocol, address = string(core.ProtocolHTTP), source.MCP.Endpoint
			}
		}
	}
	if protocol == "" {
		protocol = string(core.ProtocolHTTP)
	}
	host, port, basePath := splitTarget(address)
	return profilepkg.TargetSpec{Host: host, Port: port, Protocol: protocol, BasePath: basePath}
}

func splitTarget(address string) (host string, port int, basePath string) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, ""
	}
	parseAddress := address
	if !strings.Contains(parseAddress, "://") {
		parseAddress = "http://" + parseAddress
	}
	if parsed, err := url.Parse(parseAddress); err == nil {
		host = parsed.Hostname()
		port, _ = strconv.Atoi(parsed.Port())
		basePath = parsed.Path
		return host, port, basePath
	}
	host, portText, err := net.SplitHostPort(address)
	if err == nil {
		port, _ = strconv.Atoi(portText)
	}
	return host, port, ""
}

func transportCapabilities(caps core.Capabilities) profilepkg.TransportCapabilities {
	result := profilepkg.TransportCapabilities{
		Streaming: caps.Streaming.Supported,
	}
	for protocol, capability := range caps.Protocols {
		if !capability.Supported {
			continue
		}
		switch protocol {
		case core.ProtocolHTTP, core.ProtocolHTTPS:
			result.HTTP = true
		case core.ProtocolTCP:
			result.TCP = true
		}
		result.PublicExposure = result.PublicExposure || capability.Public
		result.PrivateExposure = result.PrivateExposure || capability.Private
	}
	result.StableHostname = caps.CustomHostnames.Supported
	result.Authentication = len(caps.BuiltInProtection) > 1
	return result
}
