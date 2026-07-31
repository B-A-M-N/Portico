package screens

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// ProviderRecommender asks the supervisor which provider suits a set of
// requirements.
type ProviderRecommender interface {
	RecommendProvider(ctx context.Context, req ipc.ProviderRecommendationRequest) (
		*ipc.ProviderRecommendationResponse, error)
}

// ProviderRecommendationMsg is delivered when a recommendation arrives.
type ProviderRecommendationMsg struct {
	// Fingerprint identifies the requirements this answer was computed for.
	//
	// The user can change an answer while the request is in flight, and two
	// requests can complete out of order. Comparing what the answer was
	// computed from against what is currently true makes ordering irrelevant;
	// a sequence number would not, because a stale request can finish last.
	Fingerprint string
	Response    *ipc.ProviderRecommendationResponse
	Err         error
}

// recommendationRequest builds the requirements from the answers so far.
func (m *WizardModel) recommendationRequest() ipc.ProviderRecommendationRequest {
	// The protocol default is applied here as well as when the connection is
	// built. Applying it in only one of them scores a provider against no
	// protocol constraint and then creates a connection that has one, so the
	// evaluation and the connection describe different things.
	protocol := m.state.SourceProtocol
	if protocol == "" && m.state.SourceType == "existing_service" {
		protocol = "http"
	}
	return ipc.ProviderRecommendationRequest{
		ConnectionKind:   "service_exposure",
		SourceKind:       m.state.SourceType,
		MCPTransport:     m.state.MCPTransport,
		ExposureMode:     m.state.ExposureMode,
		Protocol:         protocol,
		ProtectionKind:   m.state.Protection,
		RequestedAddress: m.state.Hostname,
		PreferredAccount: m.state.AccountID,
	}
}

// requirementFingerprint is a stable description of what a recommendation was
// computed from.
func requirementFingerprint(req ipc.ProviderRecommendationRequest) string {
	return strings.Join([]string{
		req.ConnectionKind, req.SourceKind, req.MCPTransport, req.ExposureMode,
		req.Protocol, req.ProtectionKind, req.RequestedAddress, req.PreferredAccount,
	}, "|")
}

// recommendCmd asks for a recommendation off the update loop.
//
// The request is built before the closure runs, so the command never reads
// wizard state while the update loop is mutating it.
func (m *WizardModel) recommendCmd() tea.Cmd {
	client, _ := m.client.(ProviderRecommender)
	ctx := m.ctx
	req := m.recommendationRequest()
	fingerprint := requirementFingerprint(req)

	m.recommendPending = true
	m.recommendFingerprint = fingerprint
	m.recommendErr = nil

	return func() tea.Msg {
		if client == nil {
			return ProviderRecommendationMsg{
				Fingerprint: fingerprint,
				Err:         fmt.Errorf("this build cannot ask for a recommendation"),
			}
		}
		resp, err := client.RecommendProvider(ctx, req)
		return ProviderRecommendationMsg{Fingerprint: fingerprint, Response: resp, Err: err}
	}
}

// HandleRecommendation installs an answer, if it still answers the current
// question.
func (m *WizardModel) HandleRecommendation(msg ProviderRecommendationMsg) {
	if msg.Fingerprint != m.recommendFingerprint {
		// The user changed an answer while this was in flight. Installing it
		// would describe a connection they are no longer creating.
		return
	}
	m.recommendPending = false
	if msg.Err != nil {
		// A failed recommendation is reported, not defaulted around: choosing a
		// provider on the user's behalf without having evaluated anything is
		// how a connection gets created that cannot work.
		m.recommendErr = msg.Err
		return
	}
	m.recommendation = msg.Response
	// Start on the recommended provider rather than whichever happens to be
	// first, without preventing the user from choosing another.
	if msg.Response != nil && msg.Response.Recommended != nil {
		m.selected = choiceIndex(m.providerChoices(), msg.Response.Recommended.ProviderID)
	}
}

// providerChoices lists every provider Portico knows about, ordered and
// annotated by the recommendation when one has arrived.
//
// Listing only usable providers hid the fact that others exist, so a user could
// not discover what installing or configuring one would give them. Listing them
// without the engine's reasoning made the list a menu with no guidance.
func (m *WizardModel) providerChoices() []wizardChoice {
	if m.recommendation != nil {
		return m.recommendedChoices()
	}
	return m.snapshotChoices()
}

// recommendedChoices renders the engine's answer: what it chose, what else
// would work, and what cannot and why.
func (m *WizardModel) recommendedChoices() []wizardChoice {
	rec := m.recommendation
	choices := make([]wizardChoice, 0, len(m.caps.providers))

	if rec.Recommended != nil {
		choices = append(choices, recommendedChoice(*rec.Recommended, true))
	}
	for _, alt := range rec.Alternatives {
		choices = append(choices, recommendedChoice(alt, false))
	}
	for _, filtered := range rec.Filtered {
		name := filtered.DisplayName
		if name == "" {
			name = filtered.ProviderID
		}
		choice := wizardChoice{
			Value:  filtered.ProviderID,
			Label:  name,
			Reason: filtered.Reason,
		}
		// The reasons are the engine's, so the refusal is explained in the same
		// terms the engine used to decide. A single reason already appears on
		// the line, so repeating it under the line says nothing twice.
		if len(filtered.Reasons) > 1 {
			choice.Detail = append(choice.Detail, filtered.Reasons...)
		}
		if len(filtered.SetupActions) > 0 {
			choice.Detail = append(choice.Detail, "", "To make this usable:")
			for _, action := range filtered.SetupActions {
				choice.Detail = append(choice.Detail, "  • "+action)
			}
		}
		choices = append(choices, choice)
	}
	return choices
}

// recommendedChoice renders one viable provider with the reasoning behind it.
func recommendedChoice(c ipc.ProviderChoiceDTO, best bool) wizardChoice {
	name := c.DisplayName
	if name == "" {
		name = c.ProviderID
	}
	if best {
		name += "   (recommended)"
	}
	choice := wizardChoice{Value: c.ProviderID, Label: name, Available: true}
	choice.Detail = append(choice.Detail, c.Reasons...)
	if len(c.Tradeoffs) > 0 {
		choice.Detail = append(choice.Detail, "")
		for _, tradeoff := range c.Tradeoffs {
			choice.Detail = append(choice.Detail, "Trade-off: "+tradeoff)
		}
	}
	if len(c.SetupActions) > 0 {
		choice.Detail = append(choice.Detail, "", "Before this can be used:")
		for _, action := range c.SetupActions {
			choice.Detail = append(choice.Detail, "  • "+action)
		}
	}
	return choice
}

// snapshotChoices lists providers from the snapshot alone, for before a
// recommendation has arrived or when asking for one failed.
func (m *WizardModel) snapshotChoices() []wizardChoice {
	choices := make([]wizardChoice, 0, len(m.caps.providers))
	for _, p := range m.caps.providers {
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		choice := wizardChoice{Value: p.ID, Label: name}
		switch {
		case !usableProvider(p):
			choice.Reason = providerUnavailableReason(p)
		case p.Readiness == "needs_config" || p.Readiness == "needs_auth":
			choice.Reason = "needs setup"
			choice.Detail = append(choice.Detail, p.SetupActions...)
		default:
			choice.Available = true
		}
		if p.LastError != "" {
			choice.Detail = append(choice.Detail, p.LastError)
		}
		choices = append(choices, choice)
	}
	return choices
}

// providerUnavailableReason states why a provider cannot be used at all.
func providerUnavailableReason(p ipc.ProviderDTO) string {
	switch p.Availability {
	case "not_implemented":
		return "Portico has no adapter for this yet"
	case "client_missing":
		return "its client is not installed"
	}
	return "not available"
}

// renderProvider draws the provider question, including what it is waiting for
// and what it could not find out.
func (m *WizardModel) renderProvider() string {
	title := "Which provider should carry the connection?"

	if m.recommendPending {
		return title + "\n\n  Choosing a provider for " + m.requirementSummary() +
			"…\n\n  Esc Back"
	}

	choices := m.providerChoices()
	view := renderChoices(title, choices, m.selected)

	// A provider that is eligible but has only unusable accounts is a distinct
	// state from having none: the user configured something and it did not take
	// effect, and telling them "no account" would send them to repeat work they
	// have already done.
	if pending := m.pendingAccountsFor(m.state.Provider); len(pending) > 0 &&
		len(m.accountsFor(m.state.Provider)) == 0 {
		view += "\n\n  This provider has saved accounts that cannot be used yet:"
		for _, account := range pending {
			label := account.Label
			if label == "" {
				label = account.ID
			}
			view += "\n    • " + label + " — " + account.Status
		}
		view += "\n  Finish or replace it from the provider screen."
	}

	if m.recommendErr != nil {
		// Say that the list is unevaluated rather than presenting it as a
		// recommendation.
		view += "\n\n  Could not evaluate providers: " + m.recommendErr.Error() +
			"\n  The list above is every installed provider, not a recommendation."
	} else if m.recommendation != nil && m.recommendation.Recommended == nil {
		view += "\n\n  " + m.recommendation.Summary
	}
	return view
}

// requirementSummary restates the requirements in the user's own terms, so a
// wait says what is being evaluated.
func (m *WizardModel) requirementSummary() string {
	parts := []string{}
	switch m.state.ExposureMode {
	case "permanent_public":
		if m.state.Hostname != "" {
			parts = append(parts, "a permanent address at "+m.state.Hostname)
		} else {
			parts = append(parts, "a permanent address")
		}
	case "temporary_public":
		parts = append(parts, "a temporary address")
	}
	if m.state.Protection == "email_otp" {
		parts = append(parts, "limited to people you name")
	}
	if len(parts) == 0 {
		return "this connection"
	}
	return strings.Join(parts, ", ")
}

// accountsFor returns the accounts belonging to one provider that can actually
// be used.
//
// The wizard previously held a single flat list, so with more than one provider
// configured it would have offered another provider's accounts. Only usable
// accounts appear: selecting a pending one would bind the connection to an
// account its provider cannot serve, and the failure would surface at open.
func (m *WizardModel) accountsFor(providerID string) []ipc.ProviderAccountDTO {
	var accounts []ipc.ProviderAccountDTO
	for _, p := range m.caps.providers {
		if p.ID != providerID {
			continue
		}
		accounts = append(accounts, p.Accounts...)
	}
	if len(accounts) == 0 {
		// Before the snapshot carried per-provider accounts the wizard was
		// handed a flat list; honour it so an older caller still works.
		accounts = m.accounts
	}
	return accounts
}

// pendingAccountsFor returns the accounts that exist but cannot be used, so the
// wizard can say why a provider with accounts still has none to offer.
func (m *WizardModel) pendingAccountsFor(providerID string) []ipc.ProviderAccountDTO {
	for _, p := range m.caps.providers {
		if p.ID == providerID {
			return p.PendingAccounts
		}
	}
	return nil
}

// selectAccountFor decides whether the account question needs asking.
//
// It is asked only when there is a real choice. The recommendation's preferred
// account is taken when it names one, because the engine scored the provider
// against that specific account and picking a different one silently evaluates
// something else.
func (m *WizardModel) selectAccountFor(providerID string) {
	accounts := m.accountsFor(providerID)

	if preferred := m.recommendedAccountFor(providerID); preferred != "" {
		for _, account := range accounts {
			if account.ID == preferred {
				m.state.AccountID = preferred
				m.state.Step = WizardStepReview
				return
			}
		}
	}

	switch len(accounts) {
	case 0:
		m.state.AccountID = ""
		m.state.Step = WizardStepReview
	case 1:
		m.state.AccountID = accounts[0].ID
		m.state.Step = WizardStepReview
	default:
		m.state.Step = WizardStepAccount
	}
}

// recommendedAccountFor returns the account the engine evaluated, if it named
// one for this provider.
func (m *WizardModel) recommendedAccountFor(providerID string) string {
	if m.recommendation == nil {
		return ""
	}
	if r := m.recommendation.Recommended; r != nil && r.ProviderID == providerID {
		return r.AccountID
	}
	for _, alt := range m.recommendation.Alternatives {
		if alt.ProviderID == providerID {
			return alt.AccountID
		}
	}
	return ""
}

// ProvidersChanged updates the wizard when the provider landscape moves under
// it.
//
// A wizard open across a provider being configured, uninstalled or switched off
// is holding options and a recommendation computed against a world that no
// longer exists. The user's answers are kept — their intent did not change —
// but anything derived from the providers is discarded and, if a choice has
// already been made that is no longer viable, they are returned to make it
// again rather than carrying a selection that cannot work.
func (m *WizardModel) ProvidersChanged(providers []ipc.ProviderDTO) {
	if sameProviderLandscape(m.caps.providers, providers) {
		return
	}
	m.caps = providerCapabilities{providers: providers}
	m.recommendation = nil
	m.recommendFingerprint = ""
	m.recommendPending = false
	m.recommendErr = nil

	if m.state.Provider == "" {
		return
	}
	for _, choice := range m.snapshotChoices() {
		if choice.Value == m.state.Provider && choice.Available {
			return
		}
	}
	// The chosen provider cannot carry this any more. Saying so beats
	// discovering it when the connection refuses to open.
	m.err = fmt.Errorf("%s is no longer available; choose another provider", m.state.Provider)
	m.state.Provider = ""
	m.state.AccountID = ""
	if m.state.Step > WizardStepProvider {
		m.state.Step = WizardStepProvider
		m.selected = 0
	}
}

// sameProviderLandscape reports whether anything the wizard derives from has
// changed. Comparing only what is used avoids discarding a recommendation
// because an unrelated field moved.
func sameProviderLandscape(before, after []ipc.ProviderDTO) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.ID != b.ID || a.Availability != b.Availability || a.Readiness != b.Readiness {
			return false
		}
		if len(a.Accounts) != len(b.Accounts) || len(a.PendingAccounts) != len(b.PendingAccounts) {
			return false
		}
		if (a.Capabilities == nil) != (b.Capabilities == nil) {
			return false
		}
		if a.Capabilities != nil && !sameCapabilities(*a.Capabilities, *b.Capabilities) {
			return false
		}
	}
	return true
}

// sameCapabilities compares the capability fields the wizard derives options
// from.
func sameCapabilities(a, b ipc.CapabilitySetDTO) bool {
	return a.TemporaryAddresses == b.TemporaryAddresses &&
		a.CustomHostnames == b.CustomHostnames &&
		a.PrivateExposure == b.PrivateExposure &&
		a.ManagedDNS == b.ManagedDNS &&
		equalStringSlices(a.ProtectionModes, b.ProtectionModes) &&
		equalStringSlices(a.Protocols, b.Protocols)
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
