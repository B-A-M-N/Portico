package clienttunnel

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries values the composition root reads from the
// environment.
type DefinitionConfig struct {
	Bin string
	// Enabled reports the experimental opt-in.
	Enabled bool
}

// Definition describes the OpenAI Secure MCP Tunnel to Portico.
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the OpenAI tunnel provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	if cfg.Bin == "" {
		cfg.Bin = ClientBinary
	}
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID: ProviderID, Name: string(ProviderID), DisplayName: "Client-mediated MCP transport",
	}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	entry := provider.CatalogEntry{
		ID:           ProviderID,
		Name:         string(ProviderID),
		DisplayName:  "Client-mediated MCP transport",
		Availability: provider.AvailabilityExperimental,
		Stability:    core.StabilityExperimental,
		Reason: "the adapter has not been exercised against a live tunnel; it can start and " +
			"observe the client but does not create tunnels or verify ChatGPT app registration",
		SetupActions: []string{
			"Install tunnel-client from the OpenAI platform's tunnel settings",
			"Create a tunnel there and note its ID; Portico does not create tunnels",
			fmt.Sprintf("Export %s before starting the supervisor", CredentialEnvVar),
		},
	}
	if !d.cfg.Enabled {
		entry.SetupActions = append(entry.SetupActions,
			"Set PORTICO_ENABLE_CLIENT_TUNNEL=1 to register the provider")
	}
	return entry
}

// Enabled reports whether the operator opted in.
func (d *Definition) Enabled() bool { return d.cfg.Enabled }

// RefreshConfig re-reads the durable settings so a settings change made while
// the supervisor is running can take effect without restarting it. The
// supervisor calls this through the activation coordinator before rebuilding
// the provider.
func (d *Definition) RefreshConfig() {
	d.cfg.Enabled = config.ClientTunnelEnabled()
	if bin := config.ClientTunnelBin(); bin != "" {
		d.cfg.Bin = bin
	}
}

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes OpenAI when its client is not installed.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	return d.CatalogEntry()
}

// SetupFlow declares what the provider needs, for the generic setup screen.
//
// The control-plane key is stored encrypted in Portico's credential repository
// through the standard account contract: the CLI collects it via hidden TTY,
// stdin or an inherited file descriptor, the supervisor encrypts it, and the
// adapter receives it decrypted only when building a child process. The
// environment variable remains a fallback for machines configured before
// stored credentials existed.
func (d *Definition) SetupFlow() core.SetupFlow { return openAITunnelSetupFlow() }

// PotentialCapabilities is the same static contract the adapter serves: the
// tunnel is private by design and delivers no public address however it is
// configured. It does not implement SetupDefinition — the client is
// configured externally — so clients should read the answer as informational
// rather than as an after-setup promise.
func (d *Definition) PotentialCapabilities(ctx context.Context) (core.Capabilities, error) {
	return (&Provider{}).Capabilities(ctx)
}

func openAITunnelSetupFlow() core.SetupFlow {
	return core.SetupFlow{
		Kind:        core.SetupAccount,
		Summary:     "Connect a local MCP server to ChatGPT over a private, outbound-only tunnel.",
		SecretField: "credential",
		Fields: []core.SetupField{
			{
				ID:          "credential",
				Label:       "Control plane API key",
				Description: "A restricted runtime key with Tunnels Read + Use. Stored encrypted; supplied to the client through its environment, never a command line.",
				Secret:      true,
				Required:    true,
				EnvVars:     []string{CredentialEnvVar},
			},
		},
		CapabilityNotes: []string{
			"The connection is outbound-only: no inbound port is opened and no public address is created.",
			"Creating the tunnel and registering the app in ChatGPT happen on OpenAI's platform, not in Portico.",
			"Portico cannot confirm this key with OpenAI from here; its validity is proven when the client first reaches the control plane, and a refused key shows up as a not-ready connection rather than a silent failure.",
		},
	}
}

// runtimeCredentialAccountID names the transport's single implicit account.
// The OpenAI tunnel is accountless upstream, but Portico's encrypted
// credential store keys secrets by account reference, so the runtime key gets
// exactly one durable identity.
func runtimeCredentialAccountID() core.ProviderAccountID {
	return core.ProviderAccountID(ProviderID)
}

// PrepareAccount turns submitted setup values into the transport's canonical
// runtime-credential account.
//
// Only the credential belongs here. The tunnel ID is per-connection state
// (ClientTunnelSpec.TunnelID), collected in the wizard, and must not be
// mistaken for account identity.
func (d *Definition) PrepareAccount(values map[string]string) (provider.PreparedAccount, error) {
	credential := strings.TrimSpace(values["credential"])
	if credential == "" {
		return provider.PreparedAccount{}, fmt.Errorf(
			"a control plane API key is required; create a restricted runtime key with Tunnels Read + Use in the OpenAI platform")
	}
	if err := validateRuntimeKeyShape(credential); err != nil {
		return provider.PreparedAccount{}, err
	}
	accountID := runtimeCredentialAccountID()
	return provider.PreparedAccount{
		Account: core.ProviderAccount{
			ID:            accountID,
			Provider:      ProviderID,
			Label:         "OpenAI runtime key",
			CredentialRef: fmt.Sprintf("%s:runtime-key", ProviderID),
		},
		Secret: []byte(credential),
	}, nil
}

// VerifyAccount implements the optional verification capability with a
// deliberately local check.
//
// A real control-plane round trip would need more of the key's own authority
// than a least-privilege runtime key should carry, so the authoritative proof
// that the key works is /readyz after launch — which observation surfaces
// honestly as a not-ready connection when the key is refused. What setup CAN
// rule out locally is a truncated paste or an obviously malformed value, and
// refusing those here beats storing them.
func (d *Definition) VerifyAccount(_ context.Context, account provider.PreparedAccount) (core.SetupValidation, error) {
	if err := validateRuntimeKeyShape(string(account.Secret)); err != nil {
		return core.SetupValidation{}, err
	}
	return core.SetupValidation{
		Notes: []string{
			"The key was checked locally only. Its authority is confirmed when the tunnel client first reaches the OpenAI control plane.",
			"Use a restricted runtime key with Tunnels Read + Use — not an admin key.",
		},
	}, nil
}

// VerificationStrength declares what a passing VerifyAccount proved: nothing
// more than the key's shape. Declaring it is what keeps setup from recording
// this credential as authenticated — a live probe of the real client showed
// /readyz answering 200 while the control plane was actively refusing the key
// with 401 invalid_api_key, so no local check can speak for OpenAI.
func (d *Definition) VerificationStrength() provider.VerificationStrength {
	return provider.VerificationLocalShape
}

// validateRuntimeKeyShape rules out values no real key could be: empty,
// whitespace-padded pastes, embedded whitespace, or absurd lengths.
func validateRuntimeKeyShape(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("a control plane API key is required")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("the API key contains whitespace; copy it again without surrounding spaces")
	}
	if len(key) < 8 || len(key) > 4096 {
		return fmt.Errorf("the API key length (%d characters) is outside anything a real key produces", len(key))
	}
	return nil
}

// Activate builds the accountless runtime when no stored credential exists,
// falling back to the environment. With a stored runtime key the adapter is
// built from that decrypted secret instead — see Activate's credential
// resolution below.
func (d *Definition) Activate(ctx context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()

	// Check for the binary.
	if req.Services.LookPath != nil {
		if _, err := req.Services.LookPath(d.cfg.Bin); err != nil {
			entry.Availability = provider.AvailabilityClientMissing
			entry.Reason = fmt.Sprintf("the %q client was not found on PATH", d.cfg.Bin)
			entry.SetupActions = []string{
				"Install tunnel-client from the OpenAI platform's tunnel settings",
				fmt.Sprintf("Export %s before starting the supervisor", CredentialEnvVar),
			}
			if !d.cfg.Enabled {
				entry.SetupActions = append(entry.SetupActions,
					"Set PORTICO_ENABLE_CLIENT_TUNNEL=1 to register the provider")
			}
			return provider.Installation{Catalog: entry}, nil
		}
	}

	// Check for the credential. A stored, decrypted runtime key wins; the
	// environment is the fallback. Both are resolved through the adapter's
	// single credential source so plan-time validation and the child env can
	// never disagree about where the key came from.
	var credential string
	for _, material := range req.Accounts {
		if len(material.Secret) > 0 {
			credential = string(material.Secret)
			break
		}
	}
	envConfigured := req.Services.Getenv != nil && req.Services.Getenv(CredentialEnvVar) != ""
	if credential == "" && !envConfigured {
		entry.Availability = provider.AvailabilityUnconfigured
		entry.Reason = fmt.Sprintf(
			"no control plane API key is stored; configure one with 'portico provider login %s' (or export %s)",
			ProviderID, CredentialEnvVar)
		entry.SetupActions = []string{
			fmt.Sprintf("Run: portico provider login %s", ProviderID),
			fmt.Sprintf("Or export %s before starting the supervisor", CredentialEnvVar),
		}
		if !d.cfg.Enabled {
			entry.SetupActions = append(entry.SetupActions,
				"Set PORTICO_ENABLE_CLIENT_TUNNEL=1 to register the provider")
		}
		return provider.Installation{Catalog: entry}, nil
	}

	adapter := NewWithGateway(d.cfg.Bin, req.Services.Processes, req.Services.Gateways)
	if credential != "" {
		adapter.SetCredential(credential)
	}
	if req.Services.ConnectorDir != "" {
		// Per-connection health URL files live under Portico's private state,
		// not the shared temp dir.
		adapter.SetRuntimeDir(filepath.Dir(req.Services.ConnectorDir))
	}

	// The adapter is selectable when explicitly enabled. The experimental stability
	// warning remains in the catalog Reason so the UI surfaces it.
	if d.cfg.Enabled {
		entry.Availability = provider.AvailabilityReady
	} else {
		entry.Availability = provider.AvailabilityExperimental
	}
	entry.Stability = core.StabilityExperimental

	return provider.Installation{Provider: adapter, Catalog: entry}, nil
}
