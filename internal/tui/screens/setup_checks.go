package screens

import (
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// What Portico can see about this machine.
//
// These are the supervisor's health checks, carried on the readiness response. The
// setup screen and `portico doctor` render the same set, so the screen a user opens
// when something is wrong and the command they run cannot disagree about what is
// wrong — which they did, because each derived its own verdicts from the provider
// list.
//
// Only the checks that need reading are shown. A screen listing a dozen ticks
// buries the one line that matters, so a healthy machine gets a sentence and a
// machine with something wrong gets the detail.

// renderChecks draws the health section, or nothing when there is nothing to say.
func (m *SetupModel) renderChecks() string {
	if m.Readiness == nil || len(m.Readiness.Checks) == 0 {
		return ""
	}

	var problems, attention, unknown []ipc.HealthCheckDTO
	for _, check := range m.Readiness.Checks {
		// Per-provider checks are not repeated here: the Providers section below
		// already lists every provider with its own summary and setup actions.
		if strings.HasPrefix(check.ID, "provider:") {
			continue
		}
		switch check.State {
		case "problem":
			problems = append(problems, check)
		case "attention":
			attention = append(attention, check)
		case "unknown":
			unknown = append(unknown, check)
		}
	}

	if len(problems) == 0 && len(attention) == 0 && len(unknown) == 0 {
		return "THIS MACHINE\n  Nothing is wrong that Portico can see.\n\n"
	}

	var b strings.Builder
	b.WriteString("THIS MACHINE\n")
	// Problems first: a reader stops at the first thing that will stop their work.
	for _, group := range [][]ipc.HealthCheckDTO{problems, attention, unknown} {
		for _, check := range group {
			b.WriteString("  " + checkMark(check.State) + " " + checkTitle(check) + "\n")
			if check.Summary != "" {
				b.WriteString("      " + check.Summary + "\n")
			}
			if check.Detail != "" {
				b.WriteString("      " + check.Detail + "\n")
			}
			if check.NextAction != "" {
				b.WriteString("      → " + check.NextAction + "\n")
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// checkMark is the leading glyph for a check's state.
func checkMark(state string) string {
	switch state {
	case "ok":
		return "✓"
	case "attention":
		return "~"
	case "problem":
		return "✗"
	default:
		// Unknown is not a pass: a check that could not be run must not be drawn
		// with a tick, because that is how a broken machine reads as a healthy one.
		return "?"
	}
}

// checkTitle names the check, falling back to its ID.
func checkTitle(check ipc.HealthCheckDTO) string {
	if check.Title != "" {
		return check.Title
	}
	return check.ID
}
