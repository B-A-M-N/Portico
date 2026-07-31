package tui

// Asynchronous replies are correlated rather than trusted.
//
// Every screen here stays navigable while a request is in flight, so a reply
// can arrive describing something the user has since moved away from. Several
// message types carried only a result, and the handler applied it to whatever
// was selected at that moment — so a plan requested for one connection could be
// previewed under another's name, and diagnostics for one could be shown
// against another.
//
// A request generation is compared on arrival: a reply that no longer answers
// the current question is dropped. This is deliberately one mechanism rather
// than a fix per message type, because the third time this defect appeared it
// was in a message that had been added after the first two were corrected.
type requestGeneration uint64

// requestTracker issues generations and reports whether a reply is still
// wanted.
type requestTracker struct {
	current requestGeneration
}

// next starts a new request, invalidating any reply still in flight.
func (t *requestTracker) next() requestGeneration {
	t.current++
	return t.current
}

// accepts reports whether a reply answers the request now outstanding.
func (t *requestTracker) accepts(generation requestGeneration) bool {
	return generation == t.current && generation != 0
}

// cancel abandons the outstanding request, so a reply already in flight is
// ignored when it lands.
func (t *requestTracker) cancel() {
	t.current++
}
