package tui

import "strings"

// One authoritative action model.
//
// Key handling, the footer, and Help were three separate descriptions of what a
// screen can do, and they had drifted in every direction the audit found:
// Inspect executed Space, Repair and Delete without advertising any of them;
// Setup implemented `E` and never said so; the scroll keys worked everywhere
// and were documented nowhere; the Providers help omitted removal; Edit, Clone
// and the wizard had no contextual help at all; and helpRow() hardcoded a list
// of actions that KeyMap did not contain.
//
// An Action is the single description. Dispatch reads it, the footer renders it,
// and Help explains it, so an action that is advertised is an action that runs,
// and an action that runs is one the user was told about.

// ActionID identifies one thing a screen can do. It is the identity dispatch
// switches on, so a rebinding changes one Keys field rather than a switch case
// in one file and a help string in another.
type ActionID string

const (
	// Global navigation and lifecycle.
	ActionQuit     ActionID = "quit"
	ActionBack     ActionID = "back"
	ActionHelp     ActionID = "help"
	ActionHome     ActionID = "home"
	ActionRefresh  ActionID = "refresh"
	ActionConfirm  ActionID = "confirm"
	ActionUp       ActionID = "up"
	ActionDown     ActionID = "down"
	ActionLeft     ActionID = "left"
	ActionRight    ActionID = "right"
	ActionPageUp   ActionID = "page_up"
	ActionPageDown ActionID = "page_down"
	ActionTop      ActionID = "top"
	ActionBottom   ActionID = "bottom"

	// Connection lifecycle.
	ActionInspect    ActionID = "inspect"
	ActionToggleOpen ActionID = "toggle_open"
	ActionNew        ActionID = "new"
	ActionEdit       ActionID = "edit"
	ActionCopy       ActionID = "copy"
	ActionDelete     ActionID = "delete"
	ActionRepair     ActionID = "repair"
	ActionDiscover   ActionID = "discover"
	ActionOperations ActionID = "operations"
	ActionProviders  ActionID = "providers"
	ActionSetup      ActionID = "setup"
	ActionSettings   ActionID = "settings"

	// Provider and account management.
	ActionConfigureProvider ActionID = "configure_provider"
	ActionRemoveAccount     ActionID = "remove_account"
	ActionVerifyAccount     ActionID = "verify_account"
	ActionReplaceCredential ActionID = "replace_credential"

	// Screen-specific.
	ActionPreview       ActionID = "preview"
	ActionApply         ActionID = "apply"
	ActionSupportExport ActionID = "support_export"
	ActionLaunchMode    ActionID = "launch_mode"
	// The settings rows are actions in their own right, so the row under the
	// cursor names the field to change rather than the change being decided by
	// the cursor's index.
	ActionDefaultAutoStart    ActionID = "default_auto_start"
	ActionDefaultOnDisconnect ActionID = "default_on_disconnect"
	ActionClientTunnelEnabled ActionID = "client_tunnel_enabled"
	ActionClientTunnelBin     ActionID = "client_tunnel_bin"
	ActionRotateSecretKey     ActionID = "rotate_secret_key"
	ActionShowMore            ActionID = "show_more"
	ActionFilter              ActionID = "filter"
	ActionFollowLogs          ActionID = "follow_logs"
	ActionManualEntry         ActionID = "manual_entry"
	ActionEvidence            ActionID = "evidence"
	ActionNextField           ActionID = "next_field"
	ActionDiscard             ActionID = "discard"
	ActionKeepEditing         ActionID = "keep_editing"
	ActionRetry               ActionID = "retry"
)

// Action describes one thing the current screen can do: its identity, the keys
// that invoke it, how to name it to a user, whether it is available now, and
// why not when it is not.
type Action struct {
	ID ActionID
	// Keys are every binding that invokes this action. The first is the one
	// advertised in the footer; the rest are accepted aliases (j/k for
	// down/up, for example).
	Keys []string
	// Label names the action in the words a user would use.
	Label string
	// Enabled is false when the action cannot be taken right now. A disabled
	// action is still advertised — drawn dimmed with its reason — because
	// hiding it makes the interface change shape as state changes.
	Enabled bool
	// DisabledReason says why, in one clause, so a dimmed action is not a
	// mystery.
	DisabledReason string
	// Help is the longer explanation shown on the contextual Help screen.
	// Empty means Label is sufficient.
	Help string
	// Primary marks an action as one of the screen's main tasks, so the
	// footer can show those first when space is short.
	Primary bool
}

// primaryKey is the binding the footer advertises.
func (a Action) primaryKey() string {
	if len(a.Keys) == 0 {
		return ""
	}
	return a.Keys[0]
}

// accepts reports whether a key invokes this action. A disabled action accepts
// nothing: that is the whole point of the enabled flag, and it is checked here
// rather than at each dispatch site so no site can forget.
func (a Action) accepts(key string) bool {
	if !a.Enabled {
		return false
	}
	for _, k := range a.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// ActionSet is every action a screen offers, in the order it advertises them.
type ActionSet []Action

// Lookup returns the action a key invokes, and whether one exists and is
// enabled. Dispatch uses this so a disabled action cannot be executed by
// pressing its key.
func (s ActionSet) Lookup(key string) (Action, bool) {
	for _, a := range s {
		if a.accepts(key) {
			return a, true
		}
	}
	return Action{}, false
}

// Find returns the action with the given ID whether or not it is enabled, so a
// caller that needs to explain a refusal can read the reason.
func (s ActionSet) Find(id ActionID) (Action, bool) {
	for _, a := range s {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// FindByKey returns the action a key names whether or not it is enabled.
//
// Dispatch uses Lookup, which refuses a disabled action. This is how the
// refusal is explained: a key that names a disabled action produces its reason
// rather than nothing at all, which is what made a dimmed action look broken.
func (s ActionSet) FindByKey(key string) (Action, bool) {
	for _, a := range s {
		for _, k := range a.Keys {
			if k == key {
				return a, true
			}
		}
	}
	return Action{}, false
}

// Advertised is every action the footer and Help should mention. Both read the
// same list, which is what stops one of them drifting from the other.
func (s ActionSet) Advertised() []Action {
	out := make([]Action, 0, len(s))
	for _, a := range s {
		if a.Label == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// footerIndent is the leading space before the action bar. It is part of the
// width budget: leaving it out made every footer two cells wider than the
// terminal, which wrapped the bar onto a second line and pushed the screen up.
const footerIndent = "  "

// footer renders the action bar: the keys and labels of everything on offer,
// with unavailable actions dimmed.
//
// The bar must fit. A footer wider than the terminal wraps, and a wrapped action
// bar is both unreadable and the wrong height, so whatever is drawn is what fits:
// the primary actions first, then as many of the rest as there is room for. An
// action dropped for space is still in the contextual Help, which is not
// width-constrained — so nothing becomes undiscoverable, it just stops competing
// for a narrow line.
func (s ActionSet) footer(th Theme, width int) string {
	entries := s.footerEntries()
	if len(entries) == 0 {
		return ""
	}

	// No width has been reported yet: show everything rather than guessing.
	if width <= 0 {
		return th.Style("help").Render(footerIndent + joinFooter(th, entries))
	}

	budget := width - displayWidth(footerIndent)
	if budget <= 0 {
		return ""
	}

	// Primary actions come first and are kept in preference to the rest, because
	// they are the screen's actual tasks. Within each group the declared order is
	// preserved, so the bar does not reshuffle as state changes.
	ordered := make([]footerEntry, 0, len(entries))
	for _, e := range entries {
		if e.primary {
			ordered = append(ordered, e)
		}
	}
	for _, e := range entries {
		if !e.primary {
			ordered = append(ordered, e)
		}
	}

	var kept []footerEntry
	used := 0
	for _, e := range ordered {
		cost := displayWidth(e.text)
		if len(kept) > 0 {
			cost += displayWidth(footerSeparator)
		}
		if used+cost > budget {
			continue
		}
		kept = append(kept, e)
		used += cost
	}
	if len(kept) == 0 {
		// Not even one action fits whole. The first is clipped rather than the
		// bar disappearing, so the screen still shows something to press.
		return th.Style("help").Render(footerIndent + truncateToWidth(ordered[0].text, budget))
	}

	// Redrawn in declared order, so a narrow bar is a subset of the wide one
	// rather than a differently-ordered list.
	final := make([]footerEntry, 0, len(kept))
	for _, e := range entries {
		for _, k := range kept {
			if k.text == e.text {
				final = append(final, e)
				break
			}
		}
	}
	return th.Style("help").Render(footerIndent + joinFooter(th, final))
}

// footerSeparator is the gap between entries on the action bar.
const footerSeparator = "   "

// footerEntry is one advertised action as it appears on the bar.
type footerEntry struct {
	text    string
	enabled bool
	primary bool
}

// footerEntries is every advertised action that has a key to press.
func (s ActionSet) footerEntries() []footerEntry {
	out := make([]footerEntry, 0, len(s))
	for _, a := range s.Advertised() {
		key := a.primaryKey()
		if key == "" {
			continue
		}
		out = append(out, footerEntry{
			text:    "[" + keyLabel(key) + "] " + a.Label,
			enabled: a.Enabled,
			primary: a.Primary,
		})
	}
	return out
}

// joinFooter renders the entries, dimming the unavailable ones.
//
// The styling is applied per entry rather than to a joined run of them, so an
// available action next to an unavailable one is not swept into the same colour.
func joinFooter(th Theme, entries []footerEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.enabled {
			parts = append(parts, e.text)
		} else {
			parts = append(parts, th.Style("muted").Render(e.text))
		}
	}
	return strings.Join(parts, footerSeparator)
}

// keyLabel names a key the way a keyboard does.
func keyLabel(key string) string {
	switch key {
	case " ", "space":
		return "space"
	case "esc":
		return "esc"
	case "enter":
		return "enter"
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case "pgup":
		return "pgup"
	case "pgdown":
		return "pgdn"
	default:
		return key
	}
}

// screenTakesTextInput reports whether a screen may be collecting typed text.
//
// On such a screen a printable key is input, not a command: `q` in a hostname is
// the letter q. These screens own their own keyboard and are delegated to before
// the action set is consulted; this is what keeps `q` out of their advertised
// actions, so no footer offers a quit that the field would swallow.
func screenTakesTextInput(screen ScreenID) bool {
	switch screen {
	case ScreenNewConnection, ScreenEdit, ScreenClone, ScreenProviderSetup:
		return true
	default:
		return false
	}
}

// navigationActions are the bindings every non-text screen accepts. They are
// declared once here so no screen has to remember them and none can advertise
// a set that differs from what it accepts.
//
// The scroll keys are included: they worked on every freely-scrolled screen and
// were documented nowhere, which is exactly the drift this model exists to
// remove.
func navigationActions(screen ScreenID, canScroll bool) ActionSet {
	set := ActionSet{
		{
			ID: ActionHelp, Keys: []string{"?"}, Label: "Help", Enabled: true,
			Help: "Explain this screen and everything it can do.",
		},
	}
	if canScroll {
		set = append(set,
			Action{
				ID: ActionPageDown, Keys: []string{"pgdown"}, Label: "Page down", Enabled: true,
				Help: "Scroll down a screenful.",
			},
			Action{
				ID: ActionPageUp, Keys: []string{"pgup"}, Label: "Page up", Enabled: true,
				Help: "Scroll up a screenful.",
			},
			Action{
				ID: ActionTop, Keys: []string{"home"}, Label: "Top", Enabled: true,
				Help: "Jump to the start of the content.",
			},
			Action{
				ID: ActionBottom, Keys: []string{"end"}, Label: "Bottom", Enabled: true,
				Help: "Jump to the end of the content.",
			},
		)
	}
	if screen == ScreenHome {
		set = append(set, Action{
			ID: ActionQuit, Keys: []string{"q"}, Label: "Quit", Enabled: true, Primary: true,
			Help: "Leave Portico. Connections that are open stay open — the supervisor keeps running.",
		})
	} else {
		set = append(set, Action{
			ID: ActionBack, Keys: []string{"esc"}, Label: "Back", Enabled: true, Primary: true,
			Help: "Return to the previous screen. Anything in progress here is abandoned.",
		})
		// q quits, on every screen that is not collecting text. It used to
		// navigate Home from some screens and quit from others, while footers
		// advertised "[q] quit" in both cases — so the same key did two
		// different things under one label. Esc is how you go back.
		if !screenTakesTextInput(screen) {
			set = append(set, Action{
				ID: ActionQuit, Keys: []string{"q"}, Label: "Quit", Enabled: true,
				Help: "Leave Portico. Connections that are open stay open, and work the " +
					"supervisor has already started carries on.",
			})
		}
	}
	// Ctrl+C always quits and is always true, so it is described even though
	// the footer has no room to advertise it on every screen.
	set = append(set, Action{
		ID: ActionQuit, Keys: []string{"ctrl+c"}, Enabled: true,
		Help: "Quit Portico immediately from anywhere.",
	})
	return set
}
