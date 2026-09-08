package tui

import (
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// The first thing a new user sees.
//
// A fresh installation landed on the word "No connections" and a footer of
// single letters. Everything Portico can do was reachable and none of it was
// offered: the user was expected to know that n starts a wizard, a scans for
// local services, and s explains what is missing.
//
// The empty state is where the product has to introduce itself, so this offers
// the three things a person can actually do at that moment and says what each
// one is for.

// firstRunTask is one thing offered on an empty Home.
type firstRunTask struct {
	action ActionID
	key    string
	title  string
	body   string
}

// emptyHomeTasks are the ways in, in the order most people want them.
//
// Setup is included only when it is genuinely required. Portico can create a
// temporary public address with no account at all, so demanding setup first
// would be a gate the product does not actually have — and a new user told to
// configure a provider before they can try anything is a new user who stops.
func (m *Model) emptyHomeTasks() []firstRunTask {
	tasks := []firstRunTask{
		{
			action: ActionNew, key: "n",
			title: "Make something reachable",
			body: "Describe what you want reachable and who should be able to reach it. " +
				"Portico picks the provider and the steps.",
		},
		{
			action: ActionDiscover, key: "a",
			title: "Find something already running",
			body: "Look for services listening on this machine and publish one of them without " +
				"typing an address.",
		},
	}

	if required, reason := m.setupRequired(); required {
		tasks = append(tasks, firstRunTask{
			action: ActionSetup, key: "s",
			title: "Finish setting Portico up",
			body:  reason,
		})
	} else {
		tasks = append(tasks, firstRunTask{
			action: ActionProviders, key: "p",
			title: "Check the providers",
			body: "See what Portico can publish through and add an account. You do not need one " +
				"for a temporary address.",
		})
	}
	return tasks
}

// setupRequired reports whether anything must be configured before a connection
// can be made, and says what.
//
// The answer comes from the supervisor's readiness, which is the authority on
// what this machine can do. It is deliberately not "is any provider
// unconfigured": an unconfigured provider alongside a usable one blocks nothing.
func (m *Model) setupRequired() (bool, string) {
	if m.readiness == nil {
		// Readiness has not been read. Claiming setup is required would be a
		// guess, and claiming it is not would be a different guess — so nothing
		// is claimed and the providers task is offered instead.
		return false, ""
	}
	for _, provider := range m.readiness.Providers {
		if !provider.Blocked {
			// At least one provider can do something. Portico works.
			return false, ""
		}
	}
	if len(m.readiness.Providers) == 0 {
		return true, "No providers are installed, so Portico cannot publish anything yet."
	}
	// Every provider is blocked. The first reason is the one to show: a list of
	// every provider's complaint is not a next action.
	for _, provider := range m.readiness.Providers {
		if provider.Reason != "" {
			return true, provider.DisplayName + " " + strings.TrimSuffix(provider.Reason, ".") +
				". Setup explains what each provider needs."
		}
	}
	return true, "Every provider needs configuring before Portico can publish anything."
}

// renderEmptyHome draws the first-run state.
//
// Every prose row is wrapped to the cells that remain after its own indent,
// through the one wrap facility — not written as single physical lines and
// left to the terminal's wrap, which moved the overflow onto rows the
// renderer counted as other lines and amputated the sentence at 80x24.
func (m *Model) renderEmptyHome() string {
	var b strings.Builder
	b.WriteString(renderHeader(m.width, m.theme, m.useASCII))
	b.WriteString("\n\n")

	const indent = 2
	intro := screens.WrapProse(
		"Portico makes something on this machine reachable from somewhere else, "+
			"and keeps it that way after you close this window.",
		m.width, indent)
	b.WriteString(m.theme.Style("title").Render("  Nothing is published yet."))
	b.WriteString("\n\n")
	for _, line := range intro {
		b.WriteString(m.theme.Style("muted").Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	actions := m.actionsFor(ScreenHome)
	for _, task := range m.emptyHomeTasks() {
		// The key comes from the action set, so what is offered here cannot
		// name a key the screen does not accept.
		key := task.key
		if action, ok := actions.Find(task.action); ok {
			if primary := action.primaryKey(); primary != "" {
				key = primary
			}
		}
		b.WriteString("  " + m.theme.Style("selected").Render("["+keyLabel(key)+"]") +
			"  " + task.title + "\n")
		// The body hangs under the title at seven cells: wrapped to
		// width - 7, then drawn with the indent restored on every row.
		for _, line := range screens.WrapProse(task.body, m.width, 7) {
			b.WriteString(m.theme.Style("muted").Render(line))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if m.status != "" {
		for _, line := range screens.WrapProse(m.status, m.width, indent) {
			b.WriteString(m.theme.Style("intervention").Render(line))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(m.renderUserFacingError())
		b.WriteString("\n")
	}

	b.WriteString(actions.footer(m.theme, m.width))
	b.WriteString("\n")
	return b.String()
}

// readinessForEmptyHome asks for readiness when Home has nothing to show.
//
// It is requested because the empty state's content depends on it, not on a
// timer: this is the one moment the answer changes what the user is told to do.
func (m *Model) readinessForEmptyHome() bool {
	return m.ready && len(m.ConnectionList()) == 0 && m.readiness == nil
}

// storeReadiness keeps what the supervisor reported, so the empty Home and the
// setup screen read one answer rather than each asking separately.
func (m *Model) storeReadiness(readiness *ipc.ReadinessDTO) {
	if readiness == nil {
		return
	}
	m.readiness = readiness
	if m.setup != nil {
		m.setup.Readiness = readiness
	}
}
