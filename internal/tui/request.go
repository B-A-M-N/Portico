package tui

import "github.com/google/uuid"

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

// requestToken identifies a request by generation and by what it is about.
//
// The generation alone answers "is this still the request I am waiting for?".
// It does not answer "is this about the thing I am looking at?", which needs
// the subject — a connection ID, an operation ID, whatever the reply will be
// attributed to. Both are needed: two requests for the same subject can be
// outstanding at once and return out of order, and a stale generation can still
// carry the subject currently on screen.
//
// This exists because four different correlation schemes had grown up here —
// a generation, a bare connection ID, an integer step counter, and nothing at
// all — and the ones using the weaker schemes are where the defect survived.
type requestToken struct {
	Generation requestGeneration
	Subject    string
}

// subjectTracker issues tokens and reports whether a reply is still wanted.
type subjectTracker struct {
	current requestToken
}

// start begins a request about a subject, abandoning anything outstanding.
func (t *subjectTracker) start(subject string) requestToken {
	t.current.Generation++
	t.current.Subject = subject
	return t.current
}

// accepts reports whether a reply answers the request now outstanding. Both
// halves must match: the right question, about the right thing.
func (t *subjectTracker) accepts(token requestToken) bool {
	return token.Generation != 0 &&
		token.Generation == t.current.Generation &&
		token.Subject == t.current.Subject
}

// cancel abandons the outstanding request.
func (t *subjectTracker) cancel() {
	t.current.Generation++
	t.current.Subject = ""
}

// Applying a plan is the one action here that changes the world.
//
// A response lost to a timeout does not mean the operation did not start: the
// supervisor continues after the client's context is cancelled. Reporting that
// as a failure invites a retry, and a retry without an idempotency key starts a
// second operation — a second tunnel, a second DNS record.
const (
	applyStarted = "started"
	applyRefused = "refused"
	applyUnknown = "unknown"
)

// applyState holds the idempotency key for one approved preview.
type applyState struct {
	planID string
	key    string
	// unknown records that an attempt ended without an answer, so the
	// interface says so rather than claiming a failure it cannot support.
	unknown bool
}

// keyFor returns the key for a plan, minting one the first time it is applied
// and reusing it for every retry of that same approved preview.
func (a *applyState) keyFor(planID string) string {
	if a.planID != planID || a.key == "" {
		a.planID = planID
		a.key = "apply-" + planID + "-" + uuid.NewString()
		a.unknown = false
	}
	return a.key
}

// reset forgets the key, so a newly approved preview is a new attempt rather
// than a retry of the previous one.
func (a *applyState) reset() {
	a.planID = ""
	a.key = ""
	a.unknown = false
}
