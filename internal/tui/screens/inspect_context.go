package screens

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// What Inspect needs from the root model.
//
// The screen showed provider and account identifiers where a name belongs, told
// every user that traffic telemetry is never collected — a statement about the
// interface presented as one about Portico — and drew the route as a plain list
// of segment names with no visualization beside it. The lifecycle actions were
// executed by the root model and advertised nowhere.
//
// InspectContext is what the root model supplies: the labels it can resolve from
// the snapshot, the telemetry it fetched, and the drawn route. The screen does
// not fetch any of it — rendering stays pure — and it does not resolve labels
// itself, because the snapshot is the root model's.
type InspectContext struct {
	// ProviderLabel and AccountLabel are the display names for this
	// connection's provider and account. Empty means unresolved, which is
	// distinct from absent: a connection referring to a removed account has an
	// account it cannot name.
	ProviderLabel string
	AccountLabel  string

	// Telemetry is the traffic sample, when the provider supplied one.
	Telemetry *ipc.TelemetryDTO
	// TelemetryUnavailable says why there is none, when there is none.
	TelemetryUnavailable string
	// TelemetryLines are the measured counters, already rendered by the caller
	// so this screen does not reimplement the presence rules.
	TelemetryLines []string
	// TelemetrySampledAt is when the sample was taken, in words.
	TelemetrySampledAt string

	// RouteDrawing is the graphical route, built from the same segments the
	// textual route below it lists. One interpretation, two presentations.
	RouteDrawing string

	// LogStream is the source filter in effect: empty for everything.
	LogStream string
	// LogFollow reports that the log is being re-read while the connection is
	// active.
	LogFollow bool
}

// SetContext installs what the root model resolved.
func (m *InspectModel) SetContext(ctx InspectContext) {
	if m == nil {
		return
	}
	m.ctx = ctx
}

// providerLabel names the provider, falling back to its ID.
//
// The ID is never hidden entirely — it stays on the Technical tab, which is
// where an identifier is useful — but it is not what the overview leads with.
func (m *InspectModel) providerLabel() string {
	if m.ctx.ProviderLabel != "" {
		return m.ctx.ProviderLabel
	}
	if m.Connection != nil {
		return m.Connection.ProviderID
	}
	return ""
}

// accountLabel names the account, falling back to its ID.
func (m *InspectModel) accountLabel() string {
	if m.ctx.AccountLabel != "" {
		return m.ctx.AccountLabel
	}
	if m.Connection != nil {
		return m.Connection.ProviderAccountID
	}
	return ""
}

// filteredLogLines are the log entries after the source filter.
//
// Filtering is here rather than in the fetch because the fetch is what the
// supervisor returns: re-reading the log to change a filter would spend an IPC
// round trip on a decision the client can make.
func (m *InspectModel) filteredLogLines() []ipc.LogLineDTO {
	if m.LogTail == nil {
		return nil
	}
	if m.ctx.LogStream == "" {
		return m.LogTail.Lines
	}
	out := make([]ipc.LogLineDTO, 0, len(m.LogTail.Lines))
	for _, line := range m.LogTail.Lines {
		if line.Stream == m.ctx.LogStream {
			out = append(out, line)
		}
	}
	return out
}

// logFilterDescription says what is being shown, so a filtered log that looks
// empty is not mistaken for a connector that wrote nothing.
func (m *InspectModel) logFilterDescription() string {
	switch m.ctx.LogStream {
	case "":
		return "Showing everything the connector wrote."
	default:
		return "Showing only " + m.ctx.LogStream + ". Press F to change."
	}
}

// LogStreams are the sources present in the current log, so a filter can name
// what actually exists rather than a fixed pair.
func (m *InspectModel) LogStreams() []string {
	if m.LogTail == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range m.LogTail.Lines {
		if line.Stream == "" || seen[line.Stream] {
			continue
		}
		seen[line.Stream] = true
		out = append(out, line.Stream)
	}
	return out
}

// renderLogLine formats one entry, including the timestamp and severity when the
// connector supplied them.
func renderLogLine(entry ipc.LogLineDTO) string {
	var b strings.Builder
	b.WriteString("  ")
	if entry.Timestamp != "" {
		b.WriteString(entry.Timestamp + " ")
	}
	if entry.Severity != "" {
		b.WriteString(strings.ToUpper(entry.Severity) + " ")
	}
	if entry.Stream != "" {
		b.WriteString("[" + entry.Stream + "] ")
	}
	b.WriteString(entry.Text)
	return b.String()
}

var _ = fmt.Sprintf
