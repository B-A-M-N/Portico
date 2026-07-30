package credentials

import (
	"os"
	"path/filepath"
	"strings"
)

// SourceKind names where a credential was found.
type SourceKind string

const (
	// SourceEnvironment is an environment variable in the supervisor's own
	// environment.
	SourceEnvironment SourceKind = "environment"
	// SourceClientConfig is a provider client's own configuration file, such as
	// the one written by `ngrok config add-authtoken`.
	SourceClientConfig SourceKind = "client_config"
	// SourcePortico is Portico's own encrypted account store.
	SourcePortico SourceKind = "portico"
)

// Detected describes one place a provider credential can come from, and whether
// something is actually there.
//
// The value is never read. Detection reports presence and location only, so the
// result is safe to render, log and include in a support export.
type Detected struct {
	Provider string
	Kind     SourceKind
	// Location is where this was looked for, in terms a user can act on: an
	// environment variable name or a file path.
	Location string
	// Present reports whether a credential was found there.
	Present bool
	// Description explains what this source gives you, in plain language.
	Description string
	// Action is what to do if it is missing.
	Action string
}

// candidate describes one place to look.
type candidate struct {
	provider    string
	kind        SourceKind
	location    string
	description string
	action      string
	// configKey is the YAML-ish key to look for when kind is SourceClientConfig.
	configKey string
}

// candidates are the places Portico knows to look.
//
// The point of looking at all is that most people have already configured these
// clients directly. Asking them to re-enter a token Portico could simply find is
// the kind of avoidable setup friction this is meant to remove.
func candidates() []candidate {
	home, _ := os.UserHomeDir()

	return []candidate{
		{
			provider:    "cloudflare",
			kind:        SourceEnvironment,
			location:    "CLOUDFLARE_API_TOKEN",
			description: "Lets Portico create managed tunnels, DNS records and Access policies.",
			action:      "Create an API token in the Cloudflare dashboard and export CLOUDFLARE_API_TOKEN.",
		},
		{
			provider:    "ngrok",
			kind:        SourceEnvironment,
			location:    "NGROK_AUTHTOKEN",
			description: "Lets the ngrok agent authenticate.",
			action:      "Export NGROK_AUTHTOKEN, or run: ngrok config add-authtoken <token>",
		},
		{
			provider:    "ngrok",
			kind:        SourceClientConfig,
			location:    filepath.Join(home, ".config", "ngrok", "ngrok.yml"),
			configKey:   "authtoken",
			description: "The ngrok agent's own saved token. Portico uses it as-is.",
			action:      "Run: ngrok config add-authtoken <token>",
		},
		{
			provider:    "openai_tunnel",
			kind:        SourceEnvironment,
			location:    "CONTROL_PLANE_API_KEY",
			description: "Lets the Secure MCP Tunnel client reach the OpenAI control plane.",
			action:      "Create a key at platform.openai.com and export CONTROL_PLANE_API_KEY.",
		},
	}
}

// Detect reports what credentials are already available on this machine.
func Detect() []Detected {
	var found []Detected
	for _, c := range candidates() {
		detected := Detected{
			Provider:    c.provider,
			Kind:        c.kind,
			Location:    c.location,
			Description: c.description,
			Action:      c.action,
		}
		switch c.kind {
		case SourceEnvironment:
			detected.Present = strings.TrimSpace(os.Getenv(c.location)) != ""
		case SourceClientConfig:
			detected.Present = configFileHasKey(c.location, c.configKey)
		}
		found = append(found, detected)
	}
	return found
}

// DetectForProvider reports the sources for one provider.
func DetectForProvider(provider string) []Detected {
	var found []Detected
	for _, d := range Detect() {
		if d.Provider == provider {
			found = append(found, d)
		}
	}
	return found
}

// HasAny reports whether any source for a provider carries a credential.
func HasAny(provider string) bool {
	for _, d := range DetectForProvider(provider) {
		if d.Present {
			return true
		}
	}
	return false
}

// configFileHasKey reports whether a client configuration file defines a
// non-empty value for a key. The value itself is never returned.
func configFileHasKey(path, key string) bool {
	if path == "" || key == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, key+":"); ok {
			if strings.TrimSpace(after) != "" {
				return true
			}
		}
	}
	return false
}
