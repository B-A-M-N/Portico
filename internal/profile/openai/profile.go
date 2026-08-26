package openai

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/profile"
)

// Definition describes the OpenAI-compatible workload independently from the
// transport that carries it. Cloudflare, ngrok, Tailscale, or another HTTP
// streaming transport can be selected after this profile validates.
type Definition struct{}

func (Definition) Kind() profile.ProfileKind { return profile.ProfileOpenAICompatible }

func (Definition) Name() string { return "OpenAI-compatible API" }

func (Definition) Validate(p profile.Profile) error {
	if p.Kind != profile.ProfileOpenAICompatible {
		return fmt.Errorf("profile kind %q is not OpenAI-compatible", p.Kind)
	}
	if p.Target.Host == "" || p.Target.Port < 1 || p.Target.Port > 65535 {
		return fmt.Errorf("OpenAI profile requires a target host and port")
	}
	if strings.TrimSpace(p.Target.Host) == "" {
		return fmt.Errorf("OpenAI profile target host is empty")
	}
	if p.Target.Protocol != "http" && p.Target.Protocol != "https" {
		return fmt.Errorf("OpenAI profile requires HTTP or HTTPS")
	}
	return nil
}

func (Definition) Compatible(caps profile.TransportCapabilities) bool {
	return caps.HTTP && caps.Streaming
}

// MCPDefinition describes the MCP workload used by the Secure MCP client.
// Keeping it beside the OpenAI-compatible API definition is intentional: both
// are OpenAI platform protocols, but neither is a transport provider.
type MCPDefinition struct{}

func (MCPDefinition) Kind() profile.ProfileKind { return profile.ProfileOpenAIMCP }

func (MCPDefinition) Name() string { return "OpenAI MCP" }

func (MCPDefinition) Validate(p profile.Profile) error {
	if p.Kind != profile.ProfileOpenAIMCP {
		return fmt.Errorf("profile kind %q is not OpenAI MCP", p.Kind)
	}
	// Core validation owns the MCP tagged union (endpoint versus command).
	// The profile layer still requires the workload to opt into streaming,
	// because MCP requests may be long-lived even when the endpoint is HTTP.
	if !p.Gateway.AllowSSE {
		return fmt.Errorf("OpenAI MCP profile requires streaming-capable gateway policy")
	}
	return nil
}

func (MCPDefinition) Compatible(caps profile.TransportCapabilities) bool {
	return caps.HTTP && caps.Streaming
}
