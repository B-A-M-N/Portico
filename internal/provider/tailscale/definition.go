package tailscale

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
)

// DefinitionConfig carries what the composition root reads from the environment.
type DefinitionConfig struct {
	// Bin is the client binary, overridable for a non-standard install.
	Bin string
}

// Definition describes Tailscale to Portico.
//
// There is no account and no credential. Tailscale authenticates the machine, not
// Portico: `tailscale up` signs the machine in, interactively or with an auth key the
// operator gave the daemon. Declaring an account would make Portico store a secret it
// never uses and would offer the user a setup step that does nothing.
type Definition struct{ cfg DefinitionConfig }

// NewDefinition builds the Tailscale provider definition.
func NewDefinition(cfg DefinitionConfig) *Definition {
	if cfg.Bin == "" {
		cfg.Bin = Binary
	}
	return &Definition{cfg: cfg}
}

func (d *Definition) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{ID: "tailscale", Name: "tailscale", DisplayName: "Tailscale"}
}

func (d *Definition) CatalogEntry() provider.CatalogEntry {
	return provider.CatalogEntry{
		ID:           "tailscale",
		Name:         "tailscale",
		DisplayName:  "Tailscale",
		Availability: provider.AvailabilityReady,
		Stability:    core.StabilityBeta,
		Reason: "Portico joins the tailnet this machine is already signed in to and can " +
			"publish a local service to it; it does not sign the machine in and does not " +
			"expose anything to the internet",
	}
}

// RequiredBinary reports the client this provider cannot run without.
func (d *Definition) RequiredBinary() string { return d.cfg.Bin }

// MissingBinaryEntry describes Tailscale when its client is not installed.
func (d *Definition) MissingBinaryEntry() provider.CatalogEntry {
	entry := d.CatalogEntry()
	entry.Availability = provider.AvailabilityClientMissing
	entry.Reason = fmt.Sprintf("the %q client was not found on PATH", d.cfg.Bin)
	entry.SetupActions = []string{
		"Install Tailscale from https://tailscale.com/download",
		"Run `tailscale up` and sign in to your tailnet",
	}
	return entry
}

// SetupFlow declares guidance rather than a form.
//
// Portico has nothing to store: signing in happens in the client, and a form asking for
// a credential Portico would not use is a question that wastes the user's time. Kind
// "guidance" is what makes the setup screen render instructions rather than fields.
func (d *Definition) SetupFlow() core.SetupFlow {
	return core.SetupFlow{
		Kind: core.SetupGuidance,
		Summary: "Tailscale signs this machine in, not Portico. Once the machine is on your " +
			"tailnet, Portico can publish a service to it.",
		GuidanceReason: "Portico holds no Tailscale credential. The client authenticates the " +
			"machine itself, so there is nothing here for Portico to save.",
		Fields: []core.SetupField{
			{
				ID:    "install",
				Label: "Install the Tailscale client",
				Description: "From https://tailscale.com/download, or your system's package " +
					"manager.",
			},
			{
				ID:    "sign_in",
				Label: "Run `tailscale up`",
				Description: "This signs this machine in to your tailnet and is the step " +
					"Portico cannot do for you. Approve the machine in the admin console if " +
					"your tailnet requires it.",
			},
			{
				ID:    "verify",
				Label: "Check with `tailscale status`",
				Description: "It should report Running. Portico reads the same status to decide " +
					"whether a private-network connection can open.",
			},
		},
	}
}

// PrepareAccount always refuses.
//
// A provider with no credential has no account to prepare, and returning one would let
// the supervisor persist a row nothing reads.
func (d *Definition) PrepareAccount(map[string]string) (provider.PreparedAccount, error) {
	return provider.PreparedAccount{}, fmt.Errorf(
		"tailscale is signed in through its own client, so there is no account for Portico " +
			"to save; run `tailscale up` instead")
}

// Activate builds the runtime, reporting what the machine's actual state allows.
//
// Availability is the machine's, not a constant. A client that is installed but signed
// out cannot open a private-network connection, and saying "ready" would offer the user
// a connection that fails at the first step.
func (d *Definition) Activate(ctx context.Context, req provider.ActivationRequest) (provider.Installation, error) {
	entry := d.CatalogEntry()

	if req.Services.LookPath != nil {
		if _, err := req.Services.LookPath(d.cfg.Bin); err != nil {
			return provider.Installation{Catalog: d.MissingBinaryEntry()}, nil
		}
	}

	adapter := New(NewExecRunner(d.cfg.Bin))

	// Ask the client where the machine stands. This is a read, so it is safe during
	// activation, and it is the difference between offering a connection that can open
	// and one that cannot.
	status, err := readStatus(ctx, adapter.runner)
	switch {
	case err != nil:
		entry.Availability = provider.AvailabilityDegraded
		entry.Reason = "the Tailscale client is installed but did not answer; the daemon may " +
			"not be running"
		entry.SetupActions = []string{
			"Check that the Tailscale daemon is running",
			"Run `tailscale status` to see what it reports",
		}
	case status.NeedsLogin():
		entry.Availability = provider.AvailabilityUnconfigured
		entry.Reason = "this machine is not signed in to a tailnet"
		entry.SetupActions = []string{"Run `tailscale up` and sign in to your tailnet"}
	case status.NeedsMachineAuth():
		entry.Availability = provider.AvailabilityUnconfigured
		entry.Reason = "this machine is signed in but has not been approved for the tailnet"
		entry.SetupActions = []string{"Approve this machine in the Tailscale admin console"}
	case !status.Joined():
		entry.Availability = provider.AvailabilityDegraded
		entry.Reason = fmt.Sprintf("the Tailscale client reports %q rather than running",
			status.BackendState)
		entry.SetupActions = []string{"Run `tailscale status` to see what it reports"}
	default:
		entry.Availability = provider.AvailabilityReady
		if name := status.TailnetName(); name != "" {
			entry.Reason = fmt.Sprintf("this machine is on the tailnet %q", name)
		}
	}

	return provider.Installation{Provider: adapter, Catalog: entry}, nil
}
