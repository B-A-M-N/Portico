package credentials

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
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
	// environment variable name or a file path. When several places are
	// searched it is the one that matched, or the first searched if none did.
	Location string
	// Searched lists every place this source was looked for.
	//
	// A source can live in more than one place, and the consuming adapter
	// often accepts any of them. Reporting only "not set" against a single
	// path implies the credential does not exist, when it may only mean
	// Portico did not look where the user put it.
	Searched []string
	// Present reports whether a credential was found there.
	Present bool
	// Description explains what this source gives you, in plain language.
	Description string
	// Action is what to do if it is missing.
	Action string
}

// candidate describes one source and every place it can live.
type candidate struct {
	provider string
	kind     SourceKind
	// locations are searched in the order the consuming code searches them, so
	// what Portico reports and what the adapter will actually use agree.
	locations   []string
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
			provider: "cloudflare",
			kind:     SourceEnvironment,
			// Both names are honoured by the code that consumes the token:
			// internal/cli/handler.go prefers the Portico-specific one and
			// falls back to the standard one, and internal/config binds both.
			locations:   []string{"PORTICO_CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKEN"},
			description: "Lets Portico create managed tunnels, DNS records and Access policies.",
			action:      "Create an API token in the Cloudflare dashboard and export CLOUDFLARE_API_TOKEN.",
		},
		{
			provider:    "ngrok",
			kind:        SourceEnvironment,
			locations:   []string{"NGROK_AUTHTOKEN"},
			description: "Lets the ngrok agent authenticate.",
			action:      "Export NGROK_AUTHTOKEN, or run: ngrok config add-authtoken <token>",
		},
		{
			provider: "ngrok",
			kind:     SourceClientConfig,
			// Both paths are the ones the ngrok adapter itself searches. A
			// token in the second was previously reported as missing even
			// though the adapter would have used it.
			locations: []string{
				filepath.Join(home, ".config", "ngrok", "ngrok.yml"),
				filepath.Join(home, ".ngrok2", "ngrok.yml"),
			},
			configKey:   "authtoken",
			description: "The ngrok agent's own saved token. Portico uses it as-is.",
			action:      "Run: ngrok config add-authtoken <token>",
		},
		{
			provider:    string(core.ProviderIDClientTunnel),
			kind:        SourceEnvironment,
			locations:   []string{"CONTROL_PLANE_API_KEY"},
			description: "Lets the Secure MCP Tunnel client reach the OpenAI control plane.",
			action:      "Create a key at platform.openai.com and export CONTROL_PLANE_API_KEY.",
		},
	}
}

// Detect reports what credentials are already available on this machine.
func Detect() []Detected {
	var found []Detected
	for _, c := range candidates() {
		if len(c.locations) == 0 {
			continue
		}
		detected := Detected{
			Provider:    c.provider,
			Kind:        c.kind,
			Location:    c.locations[0],
			Searched:    append([]string(nil), c.locations...),
			Description: c.description,
			Action:      c.action,
		}
		// The first location that carries a credential is the one reported, so
		// the screen names the place the adapter will actually read.
		for _, location := range c.locations {
			var present bool
			switch c.kind {
			case SourceEnvironment:
				present = strings.TrimSpace(os.Getenv(location)) != ""
			case SourceClientConfig:
				present = configFileHasKey(location, c.configKey)
			}
			if present {
				detected.Present = true
				detected.Location = location
				break
			}
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
