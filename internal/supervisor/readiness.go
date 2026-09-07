package supervisor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/credentials"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
)

// HandleReadiness answers one question: what does Portico need, and what is
// already satisfied?
//
// The information existed but was scattered — provider availability in one
// place, credential sources in another, per-connection blockers in a third — so
// setting Portico up meant assembling it yourself from several screens. This
// aggregates it, and reports what was found rather than asking for it again.
func (h *supervisorHandler) HandleReadiness() (*ipc.ReadinessDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	readiness := &ipc.ReadinessDTO{
		LaunchMode: h.sup.launchMode(),
	}
	if _, pinned := launchModeOverride(); pinned {
		readiness.LaunchModePinned = true
		readiness.LaunchModePinnedBy = launchModeEnv
	}

	detected := credentials.Detect()
	bySource := map[string][]credentials.Detected{}
	for _, d := range detected {
		bySource[d.Provider] = append(bySource[d.Provider], d)
	}

	for _, snap := range h.sup.registry.DiscoverIdentities(ctx) {
		entry := ipc.ProviderReadinessDTO{
			ID:           string(snap.ID),
			DisplayName:  snap.DisplayName,
			Availability: string(snap.Availability),
			Reason:       snap.Reason,
			SetupActions: append([]string(nil), snap.SetupActions...),
			SetupKind:    h.sup.setupKindFor(string(snap.ID)),
			Accounts:     len(snap.Accounts),
		}

		for _, d := range bySource[string(snap.ID)] {
			entry.Credentials = append(entry.Credentials, ipc.CredentialSourceDTO{
				Kind:        string(d.Kind),
				Location:    d.Location,
				Searched:    append([]string(nil), d.Searched...),
				Present:     d.Present,
				Description: d.Description,
				Action:      d.Action,
			})
		}
		// An account configured inside Portico is itself a credential source,
		// and is the one most people will have.
		if len(snap.Accounts) > 0 {
			entry.Credentials = append(entry.Credentials, ipc.CredentialSourceDTO{
				Kind:        string(credentials.SourcePortico),
				Location:    "saved in Portico",
				Present:     true,
				Description: fmt.Sprintf("%d account(s) configured in Portico.", len(snap.Accounts)),
			})
		}
		// A saved but unusable account is reported as a gap, not as a
		// credential. Counting it as present is what made an unverified
		// account look like a working one.
		if len(snap.PendingAccounts) > 0 {
			entry.Credentials = append(entry.Credentials, ipc.CredentialSourceDTO{
				Kind:     string(credentials.SourcePortico),
				Location: "saved in Portico, not verified",
				Present:  false,
				Description: fmt.Sprintf("%d account(s) saved but never confirmed against the provider.",
					len(snap.PendingAccounts)),
				Action: "Open setup for this provider to finish or replace the credential.",
			})
		}

		entry.Summary, entry.Blocked = summariseProviderReadiness(snap, entry.Credentials)
		readiness.Providers = append(readiness.Providers, entry)
	}
	sort.Slice(readiness.Providers, func(i, j int) bool {
		// Usable providers first, then those needing action, then the rest.
		if readiness.Providers[i].Blocked != readiness.Providers[j].Blocked {
			return !readiness.Providers[i].Blocked
		}
		return readiness.Providers[i].ID < readiness.Providers[j].ID
	})

	for _, profile := range h.sup.controller.ListProfiles() {
		entry := ipc.ConnectionReadinessDTO{
			ID:        string(profile.ID),
			Name:      profile.Name,
			Kind:      string(profile.EffectiveKind()),
			Provider:  string(profile.Driver.ProviderID),
			AutoStart: profile.Lifecycle.AutoStart,
			Desired:   string(profile.Desired),
		}
		entry.Blockers = connectionBlockers(profile, h.sup.registry)
		entry.Ready = len(entry.Blockers) == 0
		readiness.Connections = append(readiness.Connections, entry)
	}

	// The health checks, computed once here so doctor, the setup screen and the
	// recovery flow all read the same answers rather than each deriving their own
	// from the provider list.
	readiness.Checks = h.healthChecks(ctx)

	readiness.Summary = summariseReadiness(readiness)
	return readiness, nil
}

// summariseProviderReadiness states a provider's position in one sentence.
func summariseProviderReadiness(snap provider.ProviderSnapshot, sources []ipc.CredentialSourceDTO) (string, bool) {
	hasCredential := false
	for _, s := range sources {
		if s.Present {
			hasCredential = true
			break
		}
	}

	switch snap.Availability {
	case provider.AvailabilityReady:
		return "Ready to use.", false
	case provider.AvailabilityNotImplemented:
		return "Portico does not support this provider yet.", true
	case provider.AvailabilityClientMissing:
		return "Its client is not installed on this machine.", true
	case provider.AvailabilityExperimental:
		return "Experimental. Enable it explicitly before relying on it.", true
	case provider.AvailabilityDegraded:
		return "Configured, but not responding right now.", true
	case provider.AvailabilityUnconfigured:
		if hasCredential {
			// This is the case worth calling out: the machine already has what
			// is needed and only the last step is missing.
			return "A credential was found on this machine. Finish setup to use it.", true
		}
		return "Needs an account before it can be used.", true
	}
	return "State unknown.", true
}

// connectionBlockers lists what stands between a connection and opening.
func connectionBlockers(profile *core.ConnectionProfile, registry provider.Registry) []string {
	var blockers []string

	providerID := profile.Driver.ProviderID
	if providerID == "" {
		blockers = append(blockers, "No provider is selected.")
		return blockers
	}
	if registry.Get(providerID) == nil {
		blockers = append(blockers, fmt.Sprintf("The %s provider is not loaded.", providerID))
	}

	if spec := profile.Spec.ServiceExposure; spec != nil {
		if spec.Exposure.Mode == core.ExposurePermanent && spec.Exposure.RequestedAddress == "" {
			blockers = append(blockers, "A permanent address needs a hostname.")
		}
		if spec.Protection.Kind != "" && spec.Protection.Kind != core.ProtectionNone &&
			len(spec.Protection.AllowedEmails) == 0 && len(spec.Protection.AllowedDomains) == 0 {
			blockers = append(blockers, "Protection is on but nobody is allowed through yet.")
		}
	}
	if tunnel := profile.Spec.ClientTunnel; tunnel != nil && tunnel.TunnelID == "" {
		blockers = append(blockers, "No tunnel ID has been supplied.")
	}
	return blockers
}

// summariseReadiness states the overall position in one sentence, so the first
// line of the screen answers "can I use this yet?".
func summariseReadiness(r *ipc.ReadinessDTO) string {
	usable := 0
	for _, p := range r.Providers {
		if !p.Blocked {
			usable++
		}
	}
	ready := 0
	for _, c := range r.Connections {
		if c.Ready {
			ready++
		}
	}

	switch {
	case usable == 0 && len(r.Connections) == 0:
		return "No provider is ready yet. Set one up to make your first connection."
	case usable == 0:
		return "No provider is ready, so no connection can open."
	case len(r.Connections) == 0:
		return fmt.Sprintf("%d provider(s) ready. Create a connection to get started.", usable)
	case ready == len(r.Connections):
		return fmt.Sprintf("%d provider(s) ready and all %d connection(s) can open.", usable, len(r.Connections))
	}
	return fmt.Sprintf("%d provider(s) ready. %d of %d connection(s) need attention.",
		usable, len(r.Connections)-ready, len(r.Connections))
}
