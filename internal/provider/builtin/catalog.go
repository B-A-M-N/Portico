// Package builtin is Portico's provider composition root.
//
// It is the one place that names every provider compiled into the binary. That
// is deliberate: with a statically linked Go binary the alternatives are hidden
// init() self-registration, runtime plugins, or code generation, and init()
// registration makes provider availability depend on import order and become
// hard to test. One explicit list is a composition root, not a failure of
// genericity.
//
// Adding a provider means adding one entry here and nothing in the supervisor.
package builtin

import (
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
	"github.com/B-A-M-N/portico/internal/provider/ngrok"
	"github.com/B-A-M-N/portico/internal/provider/openaitunnel"
	"github.com/B-A-M-N/portico/internal/provider/portforward"
	"github.com/B-A-M-N/portico/internal/provider/tailscale"
)

// Config carries what the composition root reads from configuration and the
// environment, so no provider package depends on either.
type Config struct {
	CloudflaredBin string
	NgrokBin       string
	NgrokEnabled   bool
	// OpenAITunnelBin is empty to use the client's default name.
	OpenAITunnelBin     string
	OpenAITunnelEnabled bool
	// TailscaleBin is empty to use the client's default name.
	TailscaleBin string
}

// Definitions returns every provider compiled into Portico.
func Definitions(cfg Config) []provider.Definition {
	return []provider.Definition{
		cloudflare.NewDefinition(cloudflare.DefinitionConfig{Bin: cfg.CloudflaredBin}),
		ngrok.NewDefinition(ngrok.DefinitionConfig{Bin: cfg.NgrokBin, Enabled: cfg.NgrokEnabled}),
		openaitunnel.NewDefinition(openaitunnel.DefinitionConfig{
			Bin: cfg.OpenAITunnelBin, Enabled: cfg.OpenAITunnelEnabled,
		}),
		portforward.NewDefinition(),
		tailscale.NewDefinition(tailscale.DefinitionConfig{Bin: cfg.TailscaleBin}),
		provider.NewUnimplemented("zrok", "zrok", "Portico ships no zrok adapter yet"),
	}
}
