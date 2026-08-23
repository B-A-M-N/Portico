package tui

import "strings"

// Contextual help, generated from the action model.
//
// Help was a hand-written encyclopedia: one switch listing keys per screen, a
// "Global" section, and nothing for the wizard, Edit, Clone, Setup, Settings,
// account removal, or the plan preview. It had drifted from what the screens
// accepted in every direction the audit found — Inspect's Space, Repair and
// Delete were missing, Setup's report export was missing, the scroll keys were
// missing, and Providers listed only "add account".
//
// Generating it from the same ActionSet the footer and dispatch read means Help
// cannot describe a key the screen does not accept, and cannot omit one it does.

// screenHelp describes the task a screen is for, above the list of actions.
// Help that only lists keys tells a user what they can press, not what they are
// doing.
func screenHelp(screen ScreenID) (title string, body []string) {
	switch screen {
	case ScreenHome:
		return "Your connections", []string{
			"Everything Portico is managing. A connection describes something on this machine,",
			"how it should be reachable, and who should be allowed to reach it.",
			"",
			"Opening and closing a connection always shows you a plan first: Portico states what",
			"it will do before it does anything.",
		}
	case ScreenInspect:
		return "One connection in detail", []string{
			"Overview is its current address and state. Route draws each hop between the caller",
			"and your service, and marks the hop that is failing. Activity is traffic the",
			"provider reports, when it reports any. Technical carries identifiers for a bug",
			"report. Logs is the connector's own output.",
		}
	case ScreenNewConnection:
		return "Making a connection", []string{
			"Three questions decide everything: what should be reachable, how it should be",
			"reachable, and who should be allowed to reach it. Portico chooses the provider and",
			"the steps from your answers.",
			"",
			"Nothing is created until the review at the end, and nothing is published until you",
			"approve a plan.",
		}
	case ScreenEdit:
		return "Changing a connection", []string{
			"Change any property the connection's kind can carry. A property this kind has",
			"nowhere to put says so rather than being hidden.",
			"",
			"Nothing is saved as you type. Preview asks the supervisor exactly what the change",
			"would do, and the change is applied only when you approve that plan.",
		}
	case ScreenClone:
		return "Copying a connection", []string{
			"The copy carries the same source, provider, account and access protection as the",
			"original, and is created closed. A permanent address must be different, because two",
			"connections cannot claim the same hostname.",
		}
	case ScreenPlanPreview:
		return "What Portico will do", []string{
			"Every step Portico will carry out, in order, with the resources it will create.",
			"Nothing here has happened yet.",
			"",
			"The plan is checked against the provider again before it starts, so a plan that has",
			"gone stale is refused rather than applied against changed state.",
		}
	case ScreenOperationProgress:
		return "Work in progress", []string{
			"The supervisor is carrying out a plan. Leaving this screen does not cancel it: the",
			"supervisor owns the work and finishes it whether or not this interface is watching.",
		}
	case ScreenOperations:
		return "History", []string{
			"Everything Portico has done, most recent first, with what it was for and how it",
			"ended. A failure carries the reason it failed.",
		}
	case ScreenRepair:
		return "Diagnosis", []string{
			"Portico checks each hop of the route and reports what it found. A finding names the",
			"segment that is wrong and what would correct it.",
			"",
			"Diagnosis changes nothing. A repair is a plan like any other, and you see it before",
			"it runs.",
		}
	case ScreenDiscovery:
		return "Services already running", []string{
			"Ports listening on this machine, with what Portico could learn about each one. The",
			"confidence is how sure Portico is that it identified the service correctly, and the",
			"evidence behind that is available for any of them.",
		}
	case ScreenProviders:
		return "Providers and accounts", []string{
			"The services Portico can publish through, and the accounts you have given it. A",
			"credential is stored encrypted by the supervisor and is never displayed, logged, or",
			"included in a plan or a support report.",
		}
	case ScreenSetup:
		return "What Portico needs", []string{
			"Each provider's position: what it can do, what it still needs, and how to give it",
			"that. Some connections need no account at all, so an unconfigured provider does not",
			"necessarily block you.",
		}
	case ScreenSettings:
		return "How Portico behaves", []string{
			"Choices that outlive this session and are not properties of any one connection. The",
			"supervisor stores them, so they survive a restart.",
		}
	case ScreenAccountRemoval:
		return "Removing an account", []string{
			"Portico forgets the credential it stored. Nothing is deleted at the provider, and",
			"the removal is refused while any connection still depends on the account.",
		}
	case ScreenRecovery:
		return "Portico cannot reach its supervisor", []string{
			"The supervisor is the part of Portico that owns your connections and keeps them",
			"alive after this interface closes. Without it, nothing can be read or changed.",
			"",
			"Retry runs the whole sequence again: start the supervisor, check that it answers,",
			"and load the current state.",
		}
	default:
		return "Portico", nil
	}
}

// renderHelp draws the contextual help for the screen Help was opened from.
func (m *Model) renderHelp() string {
	var b strings.Builder
	b.WriteString(m.theme.Style("header").Render(" HELP "))
	b.WriteString("\n\n")

	source := m.prevScreen
	if source == "" {
		source = ScreenHome
	}

	title, body := screenHelp(source)
	b.WriteString(m.theme.Style("title").Render(title))
	b.WriteString("\n\n")
	for _, line := range body {
		b.WriteString(m.theme.Style("muted").Render(line))
		b.WriteString("\n")
	}

	// The actions are the screen's own list, so this cannot describe a key the
	// screen does not accept.
	actions := m.actionsFor(source)
	if advertised := actions.Advertised(); len(advertised) > 0 {
		b.WriteString("\nWhat you can do here:\n\n")
		for _, a := range advertised {
			b.WriteString(helpLine(m.theme, a))
		}
	}

	b.WriteString("\n" + m.theme.Style("muted").Render(
		"Ctrl+C quits Portico from anywhere. Connections that are open stay open: the "+
			"supervisor keeps running.") + "\n")
	b.WriteString("\n" + m.actionsFor(ScreenHelp).footer(m.theme, m.width) + "\n")
	return b.String()
}

// helpLine renders one action: its key, its label, why it is unavailable when it
// is, and its longer explanation.
func helpLine(th Theme, a Action) string {
	key := a.primaryKey()
	if key == "" {
		key = "—"
	} else {
		key = keyLabel(key)
	}

	head := padToWidth(key, 8) + a.Label
	if !a.Enabled {
		if a.DisabledReason != "" {
			head += " (unavailable: " + a.DisabledReason + ")"
		} else {
			head += " (unavailable)"
		}
		head = th.Style("muted").Render(head)
	}

	out := "  " + head + "\n"
	if a.Help != "" {
		out += "          " + th.Style("muted").Render(a.Help) + "\n"
	}
	return out
}

// padToWidth pads a string to a display width, measured in terminal cells.
func padToWidth(s string, width int) string {
	pad := width - displayWidth(s)
	if pad <= 0 {
		return s + " "
	}
	return s + strings.Repeat(" ", pad)
}
