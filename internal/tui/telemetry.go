package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/tui/screens"
)

// Traffic telemetry, as far as the interface.
//
// Portico had the whole chain already: an ngrok adapter reading the agent's
// counters, an optional provider.TelemetryProvider interface, a supervisor
// handler translating it, an IPC route, and ipc.Client.Telemetry. Nothing ever
// called it. Meanwhile the Inspect screen told users that traffic telemetry is
// never collected — a statement about the interface, presented as a statement
// about Portico.
//
// It is fetched when a surface that shows it opens, and when an event says the
// connection changed. There is no ticker: a permanently polling interface costs
// a provider API call every interval whether or not anyone is looking.

// telemetryLoadedMsg carries a traffic sample, correlated to its connection.
type telemetryLoadedMsg struct {
	Token     requestToken
	Telemetry *ipc.TelemetryDTO
	Err       error
}

// telemetryCmd asks for the traffic sample for one connection.
func (m *Model) telemetryCmd(connID string) tea.Cmd {
	if connID == "" {
		return nil
	}
	token := m.telemetryRequests.start(connID)
	client := m.client
	ctx := m.rootCtx
	return func() tea.Msg {
		reply := telemetryLoadedMsg{Token: token}
		if client == nil {
			reply.Err = fmt.Errorf("no supervisor connection")
			return reply
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		reply.Telemetry, reply.Err = client.Telemetry(reqCtx, connID)
		return reply
	}
}

// applyTelemetry installs a sample, if it is still the one being waited for.
func (m *Model) applyTelemetry(msg telemetryLoadedMsg) {
	if !m.telemetryRequests.accepts(msg.Token) {
		return
	}
	m.telemetryFor = msg.Token.Subject
	if msg.Err != nil {
		// A failed read is a failed read. Rendering it as a connection with no
		// traffic would be a measurement Portico did not make.
		m.telemetry = nil
		m.telemetryUnavailable = describeError(msg.Err).Summary
		return
	}
	m.telemetry = msg.Telemetry
	m.telemetryUnavailable = ""
	if msg.Telemetry != nil && !msg.Telemetry.Available {
		m.telemetryUnavailable = msg.Telemetry.Unavailable
	}
}

// telemetryForSelection is the sample for the connection on screen, or nothing
// when what is held belongs to a different one.
func (m *Model) telemetryForSelection() (*ipc.TelemetryDTO, string) {
	conn := m.SelectedConnection()
	if conn == nil || m.telemetryFor != conn.ID {
		return nil, ""
	}
	return m.telemetry, m.telemetryUnavailable
}

// renderTelemetry draws the activity section: what the provider measured, and
// what it did not.
func (m *Model) renderTelemetry() string {
	conn := m.SelectedConnection()
	if conn == nil {
		return ""
	}

	sample, unavailable := m.telemetryForSelection()
	switch {
	case unavailable != "":
		return m.theme.Style("muted").Render("No traffic figures: "+unavailable) + "\n"
	case sample == nil:
		return m.theme.Style("muted").Render("Reading traffic figures...") + "\n"
	case !sample.Available:
		reason := sample.Unavailable
		if reason == "" {
			reason = "this provider does not report traffic"
		}
		return m.theme.Style("muted").Render("No traffic figures: "+reason) + "\n"
	}

	var b strings.Builder
	for _, line := range telemetryLines(sample) {
		b.WriteString("  " + line + "\n")
	}
	if sample.SampledAt != "" {
		b.WriteString(m.theme.Style("muted").Render(
			"  Measured "+relativeTime(sample.SampledAt, time.Now())) + "\n")
	}
	return b.String()
}

// telemetryLines states each counter the provider measured, and says plainly
// which ones it does not report rather than showing them as zero.
func telemetryLines(sample *ipc.TelemetryDTO) []string {
	var out []string
	if sample.HasCounts {
		out = append(out,
			fmt.Sprintf("Requests since it opened: %d", sample.RequestCount),
			fmt.Sprintf("Connections open now:     %d", sample.ConnectionCount),
		)
	}
	if sample.HasBytes {
		out = append(out,
			fmt.Sprintf("Received:                 %s", humanBytes(sample.BytesIn)),
			fmt.Sprintf("Sent:                     %s", humanBytes(sample.BytesOut)),
		)
	} else {
		out = append(out, "This provider does not report byte totals.")
	}
	if sample.HasErrors {
		out = append(out, fmt.Sprintf("Provider errors:          %d", sample.ProviderErrors))
	}
	return out
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// relativeTime says how long ago a timestamp was, in the coarsest useful unit.
func relativeTime(timestamp string, now time.Time) string {
	at, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "at " + timestamp
	}
	d := now.Sub(at)
	if d < 0 {
		return "just now"
	}
	if d < 2*time.Second {
		return "just now"
	}
	return humanDuration(d) + " ago"
}

// inspectTabCmd fetches what the newly selected tab needs and nothing else.
//
// Telemetry is a provider API call, so it is made when the tab that shows it is
// opened rather than on every refresh of every tab.
func (m *Model) inspectTabCmd() tea.Cmd {
	conn := m.SelectedConnection()
	if conn == nil || m.inspect == nil {
		return nil
	}
	switch m.inspect.SelectedTab() {
	case screens.InspectTabActivity:
		if m.telemetryFor == conn.ID && (m.telemetry != nil || m.telemetryUnavailable != "") {
			// Already held for this connection. Refresh is explicit.
			return nil
		}
		return m.telemetryCmd(conn.ID)
	case screens.InspectTabLogs:
		if m.connectionLogs == nil {
			return m.connectionLogsCmd(conn.ID)
		}
	}
	return nil
}
