package screens

import (
	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The setup handoff.
//
// The provider question could refuse a provider that needed configuring and
// explain why, and that was the end of the road: the user had to leave the
// wizard, configure the provider from the providers screen, and answer every
// question again from the beginning.
//
// The wizard does not perform setup itself. It holds a ConnectionCreator, not
// the account-configuration surface, and provider-specific behaviour belongs to
// the provider adapter reached through the supervisor. So the wizard records
// which provider the user asked to configure, the root model performs the
// existing setup flow, and ProvidersChanged brings the wizard back with its
// answers intact and the recommendation recomputed.

// PendingProviderSetup reports a provider the user asked to configure from the
// provider question, and clears the request.
//
// It is consumed rather than read so the root model cannot start the same setup
// twice from two consecutive renders.
func (m *WizardModel) PendingProviderSetup() (string, bool) {
	if m == nil || m.setupProviderID == "" {
		return "", false
	}
	id := m.setupProviderID
	m.setupProviderID = ""
	return id, true
}

// ResumeAfterSetup returns the wizard to the provider question after setup,
// keeping every answer.
//
// The recomputation is ProvidersChanged's: which provider now suits the stated
// requirements is the recommendation engine's answer, and deciding it here would
// be a second, partial copy of it. This adds only what setup implies — the
// wizard is put back on the provider question, and the provider just configured
// is preferred if it now fits.
func (m *WizardModel) ResumeAfterSetup(providerID string, providers []ipc.ProviderDTO) tea.Cmd {
	if m == nil {
		return nil
	}
	m.err = nil
	m.preferredProvider = providerID
	m.state.Step = WizardStepProvider
	m.selected = 0

	// ProvidersChanged returns early when nothing that decides suitability
	// moved — which is the correct answer for a setup that did not change the
	// provider's position. In that case the question still has to be asked
	// again, because the user is being returned to it.
	if cmd := m.ProvidersChanged(providers); cmd != nil {
		return cmd
	}
	m.caps = providerCapabilities{providers: providers}
	m.recommendation = nil
	m.recommendFingerprint = ""
	m.recommendPending = false
	m.recommendErr = nil
	return m.recommendCmd()
}
