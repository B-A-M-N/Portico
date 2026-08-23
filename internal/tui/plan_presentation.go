package tui

import "github.com/B-A-M-N/portico/internal/tui/screens"

// The plan presentation the root model uses.
//
// The interpretation lives in the screens package, because the wizard has to
// reach it and cannot import this one. What is here is the single entry this
// package needs: the label for the action that carries a plan out.
//
// There were shims for the title, the intent sentence and the no-op sentence too.
// They were forwarding calls nothing made — the root preview reads the whole
// PlanView from screens.DescribePlan, which already carries all three — so they
// were a second way to reach the same answers, kept for no caller.

// planConfirmLabel names the button that carries a plan out.
//
// The intent is known, so the label states the outcome: "Delete connection", not
// "Apply". A user approving a deletion should be told that is what they are
// approving.
func planConfirmLabel(intent string) string { return screens.PlanConfirmLabel(intent) }
