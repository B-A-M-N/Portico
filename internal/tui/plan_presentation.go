package tui

// One plan presentation, shared by every intent.
//
// The root preview screen was richer than the wizard's, and the wizard's said
// "OPEN CONNECTION PLAN" over whatever it had been given. Both then offered
// "Apply" — a word that describes the mechanism rather than the outcome, so a
// user approving a deletion was told only that something would be applied.
//
// These are the two answers every plan surface needs: what to call this plan,
// and what to call the button that carries it out.

// planTitle names a plan in the words a user would use.
func planTitle(intent string) string {
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

// planConfirmLabel names the button that carries a plan out.
//
// The intent is known, so the label states the outcome. "Apply" is reserved for
// a plan whose intent Portico does not recognise, where naming an outcome would
// be a guess.
func planConfirmLabel(intent string) string {
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

// planIntentSentence says what applying this plan will do, in one sentence,
// for the line above the steps.
func planIntentSentence(intent, name string) string {
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

// noopSentence says why a plan has no steps, per intent. A plan with nothing to
// do is an answer, not a failure, and it differs by intent: an already-open
// connection needs no opening, and a healthy one needs no repair.
func noopSentence(intent, name string) string {
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
