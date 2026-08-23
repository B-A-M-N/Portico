package tui

import "github.com/B-A-M-N/portico/internal/tui/screens"

// The plan presentation the root model uses.
//
// The interpretation lives in the screens package, because the wizard has to
// reach it and cannot import this one. These are thin: they exist so this
// package's call sites read naturally, and they must never grow a second
// opinion about what a plan says.

// planTitle names a plan in the words a user would use.
func planTitle(intent string) string { return screens.PlanTitle(intent) }

// planConfirmLabel names the button that carries a plan out.
func planConfirmLabel(intent string) string { return screens.PlanConfirmLabel(intent) }

// planIntentSentence says what applying this plan will do.
func planIntentSentence(intent, name string) string {
	return screens.PlanIntentSentence(intent, name)
}

// noopSentence says why a plan has no steps.
func noopSentence(intent, name string) string { return screens.PlanNoopSentence(intent, name) }
