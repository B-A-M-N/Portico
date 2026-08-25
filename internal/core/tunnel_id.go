package core

import (
	"regexp"
	"strings"
)

// ProviderIDOpenAITunnel is the workload name profiles persisted before the
// workload/transport split carried as their provider ID. It is accepted only
// when reading old durable state; it must never be registered or written.
const ProviderIDOpenAITunnel ProviderID = "openai_tunnel"

// ProviderIDClientTunnel is the canonical transport identity for a
// client-mediated MCP tunnel. Every new plan, snapshot, create request and
// persisted row uses this spelling; the legacy one resolves to it on read.
const ProviderIDClientTunnel ProviderID = "client_tunnel"

// NormalizeProviderID resolves the legacy workload spelling to the canonical
// transport identity. Callers use it at every boundary that reads a provider
// ID from durable state, user input, or IPC. New writes never carry the
// legacy value because nothing here maps the canonical ID back onto it.
func NormalizeProviderID(id ProviderID) ProviderID {
	if id == ProviderIDOpenAITunnel {
		return ProviderIDClientTunnel
	}
	return id
}

// tunnelIDPattern is the identifier format the OpenAI tunnel control plane
// requires: the literal prefix "tunnel_" followed by exactly 32 lowercase
// hexadecimal characters. This is the single authority for that format; the
// adapter's plan validation and the wizard's input validation both call
// ValidateTunnelID rather than carrying their own pattern.
var tunnelIDPattern = regexp.MustCompile(`^tunnel_[0-9a-f]{32}$`)

// ValidTunnelID reports whether id is a well-formed OpenAI tunnel identifier.
// It does not trim; use ValidateTunnelID at raw-input boundaries.
func ValidTunnelID(id string) bool {
	return tunnelIDPattern.MatchString(id)
}

// ValidateTunnelID returns nil when id is a well-formed OpenAI tunnel
// identifier, and a validation error naming the defect otherwise. Surrounding
// whitespace is trimmed first: callers pass raw user text at UI/API
// boundaries, and interior whitespace fails the pattern like any other
// malformed character.
func ValidateTunnelID(raw string) error {
	id := strings.TrimSpace(raw)
	switch {
	case id == "":
		return ErrValidation("tunnel ID is required")
	case !strings.HasPrefix(id, "tunnel_"):
		return ErrValidation(`tunnel ID must start with "tunnel_"`)
	case !tunnelIDPattern.MatchString(id):
		return ErrValidation("tunnel ID must be \"tunnel_\" followed by 32 lowercase hexadecimal characters")
	}
	return nil
}
