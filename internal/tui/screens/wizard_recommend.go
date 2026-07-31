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
	return ipc.ProviderRecommendationRequest{
		ConnectionKind:   "service_exposure",
		SourceKind:       m.state.SourceType,
		MCPTransport:     m.state.MCPTransport,
		ExposureMode:     m.state.ExposureMode,
		Protocol:         m.state.SourceProtocol,
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

	view := renderChoices(title, m.providerChoices(), m.selected)
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
