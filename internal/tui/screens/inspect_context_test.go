package screens

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Inspect as an operational screen.
//
// It showed provider and account identifiers where a name belongs, told every
// user that traffic telemetry is never collected — a statement about the
// interface presented as one about Portico — drew the route as a bare list of
// segment names with no picture beside it, and offered no way to filter a log.

// inspectFixture is a published service with a route, a log and an account.
func inspectFixture() *InspectModel {
	m := NewInspect(&ipc.ConnectionDTO{
		ID: "conn-1", Name: "web", Kind: "service_exposure",
		UserState: "Open", DesiredState: "open",
		ProviderID: "cloudflare", ProviderAccountID: "acct-9f2b1c",
		PublicAddress: "https://web.example.com",
	})
	m.Detail = &ipc.ConnectionDetailDTO{
		Segments: []ipc.RouteSegmentDTO{
			{ID: "local_service", Status: "healthy", Label: "your service"},
			{ID: "connector", Status: "failed", Label: "connector",
				Error: "the connector process exited"},
			{ID: "address", Status: "unknown", Label: "address"},
		},
	}
	m.LogTail = &ipc.ConnectionLogsDTO{
		Available: true,
		Lines: []ipc.LogLineDTO{
			{Stream: "stdout", Text: "listening on 127.0.0.1:8080"},
			{Stream: "stderr", Text: "connection refused", Severity: "error",
				Timestamp: "2026-01-02T15:04:05Z"},
		},
	}
	return m
}

// tabView renders one tab.
func tabView(m *InspectModel, tab InspectTab) string {
	for m.SelectedTab() != tab {
		if m.SelectedTab() < tab {
			m.HandleKey("right")
		} else {
			m.HandleKey("left")
		}
	}
	return m.View()
}

// TestOverviewNamesTheProviderAndAccount pins that identifiers are not what the
// overview leads with.
func TestOverviewNamesTheProviderAndAccount(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{
		ProviderLabel: "Cloudflare",
		AccountLabel:  "Work account",
	})

	view := tabView(m, InspectTabOverview)
	if !strings.Contains(view, "Cloudflare") {
		t.Errorf("the overview does not name the provider:\n%s", view)
	}
	if !strings.Contains(view, "Work account") {
		t.Errorf("the overview does not name the account:\n%s", view)
	}
	// The opaque IDs are not on the overview. They remain on Technical, where an
	// identifier is what is wanted.
	if strings.Contains(view, "acct-9f2b1c") {
		t.Errorf("the overview shows the raw account ID:\n%s", view)
	}

	technical := tabView(m, InspectTabTechnical)
	if !strings.Contains(technical, "acct-9f2b1c") {
		t.Errorf("the account ID is not available under technical detail:\n%s", technical)
	}
}

// TestAnUnresolvedAccountFallsBackToItsID pins that a connection referring to a
// removed account still says something.
func TestAnUnresolvedAccountFallsBackToItsID(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{ProviderLabel: "Cloudflare"})

	view := tabView(m, InspectTabOverview)
	if !strings.Contains(view, "acct-9f2b1c") {
		t.Errorf("an account with no resolved label shows nothing at all:\n%s", view)
	}
}

// TestTheRouteTabShowsBothThePictureAndTheEvidence pins item 17's requirement
// that the two presentations share one interpretation.
func TestTheRouteTabShowsBothThePictureAndTheEvidence(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{
		RouteDrawing: "◐━━━╳\n  your service   connector",
	})

	view := tabView(m, InspectTabRoute)
	if !strings.Contains(view, "╳") {
		t.Errorf("the route tab does not show the drawing:\n%s", view)
	}
	if !strings.Contains(view, "EVIDENCE") {
		t.Errorf("the route tab does not show the evidence:\n%s", view)
	}
	// Each hop is named, and its status is stated in words rather than as the
	// supervisor's normalisation vocabulary.
	if !strings.Contains(view, "not working") {
		t.Errorf("the failing hop is not described in words:\n%s", view)
	}
	if !strings.Contains(view, "not checked") {
		t.Errorf("the unchecked hop is not distinguished:\n%s", view)
	}
	if strings.Contains(view, "[healthy]") {
		t.Errorf("the route shows the raw status vocabulary:\n%s", view)
	}
	// And what went wrong.
	if !strings.Contains(view, "the connector process exited") {
		t.Errorf("the failing hop's error is not shown:\n%s", view)
	}
}

// TestTheActivityTabReportsWhatTheProviderMeasured pins that telemetry reaches
// the screen.
func TestTheActivityTabReportsWhatTheProviderMeasured(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{
		Telemetry:          &ipc.TelemetryDTO{Available: true, HasCounts: true, RequestCount: 91},
		TelemetryLines:     []string{"Requests since it opened: 91"},
		TelemetrySampledAt: "4s ago",
	})

	view := tabView(m, InspectTabActivity)
	if !strings.Contains(view, "91") {
		t.Errorf("the measured request count is not shown:\n%s", view)
	}
	if !strings.Contains(view, "4s ago") {
		t.Errorf("the sample time is not shown:\n%s", view)
	}
	// The old blanket claim is gone.
	if strings.Contains(view, "not collected for this provider") {
		t.Errorf("the activity tab still claims telemetry is never collected:\n%s", view)
	}
}

// TestTheActivityTabSaysWhyThereAreNoFigures pins the unavailable state.
func TestTheActivityTabSaysWhyThereAreNoFigures(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{
		TelemetryUnavailable: "No traffic figures: this provider does not report traffic",
	})

	view := tabView(m, InspectTabActivity)
	if !strings.Contains(view, "does not report traffic") {
		t.Errorf("the activity tab does not say why there are no figures:\n%s", view)
	}
}

// TestTheLogTabFiltersBySource pins item 21's filtering requirement.
func TestTheLogTabFiltersBySource(t *testing.T) {
	m := inspectFixture()

	// Everything, by default.
	m.SetContext(InspectContext{})
	view := tabView(m, InspectTabLogs)
	for _, want := range []string{"listening on 127.0.0.1:8080", "connection refused"} {
		if !strings.Contains(view, want) {
			t.Errorf("the unfiltered log omits %q:\n%s", want, view)
		}
	}

	// Filtered to one source.
	m.SetContext(InspectContext{LogStream: "stderr"})
	view = m.View()
	if strings.Contains(view, "listening on 127.0.0.1:8080") {
		t.Errorf("the stderr filter still shows stdout:\n%s", view)
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("the stderr filter dropped a stderr line:\n%s", view)
	}
	// What is being shown is stated, so a filtered log is not mistaken for a
	// connector that wrote nothing.
	if !strings.Contains(view, "Showing only stderr") {
		t.Errorf("the log does not say what filter is in effect:\n%s", view)
	}
}

// TestAFilterMatchingNothingSaysSo pins that an empty filter result is not
// reported as an empty log.
func TestAFilterMatchingNothingSaysSo(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{LogStream: "nothing-writes-here"})

	view := tabView(m, InspectTabLogs)
	if !strings.Contains(view, "Nothing matches this filter") {
		t.Errorf("an empty filter result does not say it is a filter:\n%s", view)
	}
	if strings.Contains(view, "has not written any output") {
		t.Errorf("a filtered log is reported as a silent connector:\n%s", view)
	}
}

// TestTheLogShowsTimestampAndSeverityWhenPresent pins that the extended DTO
// fields are used where the connector supplied them, and cause no blank column
// where it did not.
func TestTheLogShowsTimestampAndSeverityWhenPresent(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{})
	view := tabView(m, InspectTabLogs)

	if !strings.Contains(view, "2026-01-02T15:04:05Z") {
		t.Errorf("a line carrying a timestamp does not show it:\n%s", view)
	}
	if !strings.Contains(view, "ERROR") {
		t.Errorf("a line carrying a severity does not show it:\n%s", view)
	}
	// The line without either is not padded with empty columns.
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "listening on 127.0.0.1:8080") {
			if strings.Contains(line, "  [stdout]  ") {
				t.Errorf("a line with no timestamp was padded as if it had one: %q", line)
			}
		}
	}
}

// TestFollowingIsStated pins that the follow mode is visible, since it changes
// what the screen is doing.
func TestFollowingIsStated(t *testing.T) {
	m := inspectFixture()
	m.SetContext(InspectContext{LogFollow: true})

	view := tabView(m, InspectTabLogs)
	if !strings.Contains(view, "Following") {
		t.Errorf("following is not stated:\n%s", view)
	}
}

// TestLogStreamsAreDerivedFromTheLog pins that a filter names what exists rather
// than a fixed pair.
func TestLogStreamsAreDerivedFromTheLog(t *testing.T) {
	m := inspectFixture()
	streams := m.LogStreams()
	if len(streams) != 2 {
		t.Fatalf("streams = %v, want the two the log contains", streams)
	}
	joined := strings.Join(streams, ",")
	for _, want := range []string{"stdout", "stderr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the derived streams omit %q: %v", want, streams)
		}
	}

	// A log with one source offers one.
	m.LogTail = &ipc.ConnectionLogsDTO{
		Available: true,
		Lines:     []ipc.LogLineDTO{{Stream: "stdout", Text: "only one source"}},
	}
	if got := m.LogStreams(); len(got) != 1 {
		t.Errorf("a single-source log offers %v", got)
	}
}
