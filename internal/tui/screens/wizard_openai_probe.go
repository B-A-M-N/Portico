package screens

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/profile/openai"
)

// OpenAI-compatibility probing in the wizard (audit item 26).
//
// When the user picks "Share a local OpenAI-compatible API", the local
// endpoint is probed with the real protocol checks — /v1/models, auth
// requirement — before anything is published. The result is shown in the
// user's words: which models answered, whether the server wants a token.
// The internal term "profile kind" never appears; the probe is just part of
// "Portico checks the endpoint speaks the OpenAI API", which the recipe's
// explanation already promised.

// OpenAICompatProbedMsg carries the probe outcome back to the wizard. The
// wizard identity/generation pattern applies: a reply from a wizard the user
// has since restarted or re-driven is dropped by the caller. The root model
// forwards it into the live wizard.
type OpenAICompatProbedMsg struct {
	id         WizardID
	generation WizardGeneration
	result     *openai.OpenAICompatibility
	err        error
}

// probeOpenAICompatibilityCmd runs the compatibility probe off the UI thread.
func (m WizardModel) probeOpenAICompatibilityCmd() tea.Cmd {
	wizardID := m.id
	generation := m.generation
	endpoint := m.state.SourceAddress
	if port := m.state.Port; port != "" {
		if joined, err := existingServiceAddress(endpoint, port); err == nil {
			endpoint = joined
		}
	}
	return func() tea.Msg {
		result, err := openai.ProbeOpenAICompatibility(context.Background(), endpoint, "")
		return OpenAICompatProbedMsg{id: wizardID, generation: generation, result: result, err: err}
	}
}

// handleOpenAICompatProbed records a probe reply if it belongs to this wizard
// generation. Returns true when the message was consumed.
// HandleOpenAICompatProbed records a probe reply if it belongs to this wizard
// generation. Returns true when the message was consumed.
func (m *WizardModel) HandleOpenAICompatProbed(msg OpenAICompatProbedMsg) bool {
	if msg.id != m.id || msg.generation != m.generation {
		return false // stale reply from a superseded wizard drive
	}
	if msg.err != nil {
		m.err = msg.err
		return true
	}
	m.openAIProbeResult = msg.result
	var summary strings.Builder
	if len(msg.result.Models) > 0 {
		names := msg.result.Models
		if len(names) > 3 {
			names = names[:3]
		}
		summary.WriteString("Models detected: " + strings.Join(names, ", "))
		if len(msg.result.Models) > 3 {
			summary.WriteString(" and ")
			n := len(msg.result.Models) - 3
			summary.WriteString(itoa(n))
			summary.WriteString(" more")
		}
	} else {
		summary.WriteString("No models listed")
	}
	if msg.result.AuthRequired {
		summary.WriteString("; this server requires an auth token (configure one after setup)")
	}
	m.probeSummary = summary.String()
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
