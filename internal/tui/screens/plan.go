package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// One plan presentation, used by every surface that shows a plan.
//
// There were two. The root preview described the outcome, who would be able to
// reach the service, the local and provider changes, what could be undone, the
// warnings, and then the exact steps. The wizard's printed "OPEN CONNECTION
// PLAN" over the intent, the provider, and a numbered list — regardless of what
// the plan's intent actually was, so a plan for anything else was mislabelled.
//
// It lives here, in the package the wizard can reach, and the root model calls
// it too. That is what makes it one presentation rather than a third.

// PlanTitle names a plan in the words a user would use.
func PlanTitle(intent string) string {
	switch intent {
	case "open":
		return "OPEN CONNECTION"
	case "close":
		return "CLOSE CONNECTION"
	case "edit":
		return "APPLY CHANGES"
	case "repair":
		return "REPAIR CONNECTION"
	case "delete":
		return "DELETE CONNECTION"
	default:
		return "REVIEW PLAN"
	}
}

// PlanConfirmLabel names the button that carries a plan out.
//
// The intent is known, so the label states the outcome. "Apply" is reserved for
// a plan whose intent Portico does not recognise, where naming an outcome would
// be a guess.
func PlanConfirmLabel(intent string) string {
	switch intent {
	case "open":
		return "Open connection"
	case "close":
		return "Close connection"
	case "edit":
		return "Apply changes"
	case "repair":
		return "Repair connection"
	case "delete":
		return "Delete connection"
	default:
		return "Apply"
	}
}

// PlanIntentSentence says what applying this plan will do, in one sentence.
func PlanIntentSentence(intent, name string) string {
	if name == "" {
		name = "this connection"
	}
	switch intent {
	case "open":
		return "Portico will make " + name + " reachable by carrying out these steps:"
	case "close":
		return "Portico will stop " + name + " being reachable. The saved configuration is kept."
	case "edit":
		return "Portico will change " + name + " as follows:"
	case "repair":
		return "Portico will try to restore " + name + " with the smallest change that fixes it:"
	case "delete":
		return "Portico will delete " + name + " and the resources it created for it:"
	default:
		return "Portico will carry out these steps on " + name + ":"
	}
}

// PlanNoopSentence says why a plan has no steps, per intent.
//
// A plan with nothing to do is an answer, not a failure, and the answer differs
// by intent: an already-open connection needs no opening, and a healthy one
// needs no repair.
func PlanNoopSentence(intent, name string) string {
	if name == "" {
		name = "This connection"
	}
	switch intent {
	case "open":
		return name + " is already open."
	case "close":
		return name + " is already closed."
	case "edit":
		return "That change would have no effect."
	case "repair":
		return "Nothing is wrong with " + name + " that Portico can repair."
	case "delete":
		return "There is nothing left to delete for " + name + "."
	default:
		return "There is nothing to do."
	}
}

// PlanView is the presentation-neutral content of a plan preview: labelled
// sections of plain lines, in the order they should be shown.
//
// Returning content rather than a rendered string is what lets the root model
// apply its theme and the wizard render it plainly, without either of them
// deciding what a plan preview contains.
type PlanView struct {
	Title    string
	Subject  string
	Sentence string
	Sections []PlanSection
	// Confirm is what the action that applies this plan should be called.
	Confirm string
	// Irreversible reports that at least one step cannot be undone, so the
	// surface can say so next to the confirmation rather than only in the list.
	Irreversible bool
}

// PlanSection is one labelled group in a plan preview.
type PlanSection struct {
	Title string
	Lines []string
	// Attention marks a section that is a warning rather than information.
	Attention bool
}

// DescribePlan is the one interpretation of a plan for display.
func DescribePlan(plan *ipc.PlanDTO, connectionName string) PlanView {
	if plan == nil {
		return PlanView{Title: "REVIEW PLAN", Sentence: "Loading the plan..."}
	}

	view := PlanView{
		Title:   PlanTitle(plan.Intent),
		Subject: connectionName,
		Confirm: PlanConfirmLabel(plan.Intent),
	}

	if len(plan.Steps) == 0 {
		view.Sentence = PlanNoopSentence(plan.Intent, connectionName)
	} else {
		view.Sentence = PlanIntentSentence(plan.Intent, connectionName)
	}

	if plan.Outcome != "" {
		view.Sections = append(view.Sections, PlanSection{
			Title: "What this achieves", Lines: []string{plan.Outcome}})
	}
	if plan.Access != "" {
		view.Sections = append(view.Sections, PlanSection{
			Title: "Who will be able to reach it", Lines: []string{plan.Access}})
	}
	if len(plan.LocalChanges) > 0 {
		view.Sections = append(view.Sections, PlanSection{
			Title: "On this machine", Lines: plan.LocalChanges})
	}
	if len(plan.ProviderChanges) > 0 {
		view.Sections = append(view.Sections, PlanSection{
			Title: "At the provider", Lines: plan.ProviderChanges})
	}
	if len(plan.Reversibility) > 0 {
		view.Sections = append(view.Sections, PlanSection{
			Title: "What can be undone", Lines: plan.Reversibility})
	}
	if len(plan.Warnings) > 0 {
		view.Sections = append(view.Sections, PlanSection{
			Title: "Warnings", Lines: plan.Warnings, Attention: true})
	}

	if steps := planStepLines(plan); len(steps) > 0 {
		view.Sections = append(view.Sections, PlanSection{
			Title: "Exactly these steps, in this order", Lines: steps})
	}
	for _, step := range plan.Steps {
		if step.Irreversible {
			view.Irreversible = true
		}
	}
	return view
}

// planStepLines numbers the steps and marks the ones that cannot be undone.
func planStepLines(plan *ipc.PlanDTO) []string {
	out := make([]string, 0, len(plan.Steps))
	for i, step := range plan.Steps {
		mark := " "
		switch {
		case step.Irreversible:
			mark = "X"
		case step.Destructive:
			mark = "!"
		}
		line := fmt.Sprintf("[%s] %d. %s", mark, i+1, step.Summary)
		switch {
		case step.Irreversible:
			line += " — cannot be undone"
		case step.Destructive:
			line += " — removes something"
		}
		out = append(out, line)
	}
	return out
}

// RenderPlan draws a plan view plainly. The root model renders the same view
// with its theme; this is what the wizard uses.
func RenderPlan(view PlanView) string {
	var b strings.Builder
	b.WriteString(view.Title + "\n\n")
	if view.Sentence != "" {
		b.WriteString(view.Sentence + "\n")
	}
	for _, section := range view.Sections {
		b.WriteString("\n" + section.Title + "\n")
		for _, line := range section.Lines {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}
