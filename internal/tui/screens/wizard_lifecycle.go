package screens

// Lifecycle, asked rather than assumed.
//
// buildRequest hardcoded AutoStart: true and OnDisconnect: keep_alive. Every
// connection anyone created was therefore armed to open by itself whenever the
// supervisor started, and to keep running after the interface closed — two
// decisions with real consequences, made silently, and described nowhere.
//
// The defaults now come from the supervisor's stored settings, and the review
// step states both in plain language and lets the user change them for this
// connection before anything is created.

// LifecycleDefaults are the persisted answers a new connection starts from.
type LifecycleDefaults struct {
	// AutoStart is whether the connection opens when the supervisor starts.
	AutoStart bool
	// OnDisconnect is "keep_alive" or "close": what happens when the client
	// that created the connection goes away.
	OnDisconnect string
}

// WithDefaults applies the installation's lifecycle defaults.
//
// A wizard built without them behaves as though the user had chosen not to arm
// the connection, which is the conservative answer: a connection that opens by
// itself is a decision, and the absence of a decision is not consent to it.
func (m *WizardModel) WithDefaults(defaults LifecycleDefaults) *WizardModel {
	if m == nil {
		return m
	}
	m.state.AutoStart = defaults.AutoStart
	m.state.OnDisconnect = defaults.OnDisconnect
	if m.state.OnDisconnect == "" {
		m.state.OnDisconnect = "keep_alive"
	}
	return m
}

// onDisconnectValue normalises the disconnect policy for the wire.
//
// An empty policy means the wizard was built without defaults. keep_alive is
// what the supervisor's own default is, and is what a connection the user did
// not decide about should do: leaving it running is recoverable, closing
// something they wanted open is not.
func onDisconnectValue(policy string) string {
	if policy == "close" {
		return "close"
	}
	return "keep_alive"
}

// autoStartSentence says what AutoStart does, in the words a user would use.
func autoStartSentence(enabled bool) string {
	if enabled {
		return "Opens by itself whenever the supervisor starts"
	}
	return "Opens only when you ask"
}

// onDisconnectSentence says what the disconnect policy does.
//
// "keep_alive" is not a phrase anyone says. What matters to the user is whether
// quitting Portico takes the connection down with it.
func onDisconnectSentence(policy string) string {
	if policy == "close" {
		return "Closes when you quit Portico"
	}
	return "Keeps running after you quit Portico"
}

// cycleLifecycle steps the lifecycle answers.
//
// The two choices are one action because they are one question in a user's
// terms — when should this be reachable — and four combinations is a short
// enough list to step through.
func (m *WizardModel) cycleLifecycle() {
	switch {
	case !m.state.AutoStart && m.state.OnDisconnect != "close":
		m.state.AutoStart = true
	case m.state.AutoStart && m.state.OnDisconnect != "close":
		m.state.OnDisconnect = "close"
	case m.state.AutoStart && m.state.OnDisconnect == "close":
		m.state.AutoStart = false
	default:
		m.state.OnDisconnect = "keep_alive"
	}
}
