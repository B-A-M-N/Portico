package route

import (
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Drawing the route the supervisor described.
//
// RouteVM carried Segments, Protection, DNSDrift and ActiveFinding, and the
// renderer used none of them: it drew a fixed local → gateway → endpoint shape
// with one style for the whole line. A connection with a working tunnel and a
// broken DNS record was drawn identically to one whose connector had never
// started, and every kind got a provider gateway — including the port forward and
// the client tunnel, which do not have one.

// healthySegments are the hops of a working published service.
func healthySegments() []RouteSegmentVM {
	return []RouteSegmentVM{
		{ID: core.SegmentLocalService, Status: SegmentHealthy},
		{ID: core.SegmentConnector, Status: SegmentHealthy},
		{ID: core.SegmentProviderEdge, Status: SegmentHealthy},
		{ID: core.SegmentAddress, Status: SegmentHealthy},
	}
}

// TestAHealthyRouteIsContinuous pins the baseline: nothing wrong, nothing broken.
func TestAHealthyRouteIsContinuous(t *testing.T) {
	out := RenderSegmentedRoute(RouteVM{
		State:    RouteOpen,
		Segments: healthySegments(),
	}, 80, false)

	if out == "" {
		t.Fatal("a route with segments was not drawn from them")
	}
	line := strings.Split(out, "\n")[0]
	if strings.ContainsRune(line, '╳') {
		t.Errorf("a healthy route was drawn with a break:\n%s", out)
	}
	if !strings.ContainsRune(line, '━') {
		t.Errorf("a healthy route has no connected line:\n%s", out)
	}
	// Every segment is named, so the user can see the shape of the route.
	for _, want := range []string{"your service", "connector", "provider", "address"} {
		if !strings.Contains(out, want) {
			t.Errorf("the route does not name the %q segment:\n%s", want, out)
		}
	}
}

// TestAFailureBreaksTheRouteWhereItFailed pins the central requirement.
//
// A route with a broken hop is not a continuous line. Drawing it as one in a
// different colour asks the user to notice a shade rather than see a gap.
func TestAFailureBreaksTheRouteWhereItFailed(t *testing.T) {
	segments := healthySegments()
	segments[1].Status = SegmentFailed // the connector

	out := RenderSegmentedRoute(RouteVM{
		State:    RouteDegraded,
		Segments: segments,
		ActiveFinding: &FindingVM{
			SegmentID: core.SegmentConnector,
			Summary:   "the connector process is not running",
		},
	}, 80, false)

	line := strings.Split(out, "\n")[0]
	breakAt := strings.IndexRune(line, '╳')
	if breakAt < 0 {
		t.Fatalf("a failed segment was not drawn as a break:\n%s", out)
	}

	// Everything past the break is blank: nothing beyond a broken hop is
	// reachable, and drawing it would say otherwise.
	after := strings.TrimRight(line[breakAt+len("╳"):], " ")
	if strings.ContainsRune(after, '━') {
		t.Errorf("the route continues past the break:\n%s", out)
	}

	// The break is where the connector is, not at the end.
	if breakAt > len(line)*3/4 {
		t.Errorf("the break is drawn at the end rather than at the failing hop:\n%s", out)
	}

	// And what is wrong is stated.
	if !strings.Contains(out, "connector process is not running") {
		t.Errorf("the finding is not shown under the break:\n%s", out)
	}
}

// TestDegradedAndUnknownRenderDistinctly pins that the three non-healthy states
// are three different pictures.
//
// A segment Portico could not check and one it checked and found wrong are
// different facts. Drawing both as healthy hides one; drawing both as broken
// misstates the other.
func TestDegradedAndUnknownRenderDistinctly(t *testing.T) {
	render := func(status SegmentStatus) string {
		segments := healthySegments()
		segments[2].Status = status
		out := RenderSegmentedRoute(RouteVM{State: RouteOpen, Segments: segments}, 80, false)
		return strings.Split(out, "\n")[0]
	}

	healthy := render(SegmentHealthy)
	degraded := render(SegmentDegraded)
	unknown := render(SegmentUnknown)
	failed := render(SegmentFailed)

	for _, pair := range []struct {
		name string
		a, b string
	}{
		{"healthy and degraded", healthy, degraded},
		{"healthy and unknown", healthy, unknown},
		{"degraded and unknown", degraded, unknown},
		{"degraded and failed", degraded, failed},
	} {
		if pair.a == pair.b {
			t.Errorf("%s are drawn identically:\n%s", pair.name, pair.a)
		}
	}

	if !strings.ContainsRune(degraded, '┅') {
		t.Errorf("a degraded segment has no distinct glyph:\n%s", degraded)
	}
	if !strings.ContainsRune(unknown, '┄') {
		t.Errorf("an unknown segment has no distinct glyph:\n%s", unknown)
	}
}

// TestProtectionIsACheckpointOnTheRoute pins that access control is drawn as a
// place traffic passes through.
func TestProtectionIsACheckpointOnTheRoute(t *testing.T) {
	withProtection := RenderSegmentedRoute(RouteVM{
		State:      RouteOpen,
		Segments:   healthySegments(),
		Protection: &CheckpointVM{Label: "sign-in", Active: true},
	}, 80, false)

	without := RenderSegmentedRoute(RouteVM{
		State:    RouteOpen,
		Segments: healthySegments(),
	}, 80, false)

	if withProtection == without {
		t.Fatal("protection makes no difference to the drawn route")
	}
	if !strings.ContainsRune(strings.Split(withProtection, "\n")[0], '┃') {
		t.Errorf("protection is not drawn as a checkpoint on the route:\n%s", withProtection)
	}
	if !strings.Contains(withProtection, "sign-in") {
		t.Errorf("the checkpoint is not named:\n%s", withProtection)
	}
}

// TestDNSDriftIsDisplacedRatherThanBroken pins its own visual state.
//
// The route works and arrives somewhere other than where the user's hostname
// points. Healthy hides the problem; broken misstates it.
func TestDNSDriftIsDisplacedRatherThanBroken(t *testing.T) {
	drifted := RenderSegmentedRoute(RouteVM{
		State:    RouteOpen,
		Segments: healthySegments(),
		DNSDrift: true,
	}, 80, false)

	healthy := RenderSegmentedRoute(RouteVM{State: RouteOpen, Segments: healthySegments()}, 80, false)
	if drifted == healthy {
		t.Fatal("DNS drift is drawn as a healthy route")
	}

	line := strings.Split(drifted, "\n")[0]
	if !strings.ContainsRune(line, '⇥') {
		t.Errorf("DNS drift has no displaced marker:\n%s", drifted)
	}
	// Displaced is not broken: the line continues, because the traffic does.
	if strings.ContainsRune(line, '╳') {
		t.Errorf("DNS drift is drawn as a break in the route:\n%s", drifted)
	}
}

// TestTopologyFollowsTheSegmentsGiven pins that the drawing has no opinion about
// how many hops a connection has.
//
// A port forward is a listener and a target: two hops, no provider edge and no
// public address. The fixed drawing gave every kind a provider gateway.
func TestTopologyFollowsTheSegmentsGiven(t *testing.T) {
	forward := RenderSegmentedRoute(RouteVM{
		State: RouteOpen,
		Segments: []RouteSegmentVM{
			{ID: core.SegmentLocalRoute, Status: SegmentHealthy, Label: "listener"},
			{ID: core.SegmentEndpoint, Status: SegmentHealthy, Label: "target"},
		},
	}, 80, false)

	if strings.Contains(forward, "provider") {
		t.Errorf("a port forward was drawn with a provider it does not have:\n%s", forward)
	}
	for _, want := range []string{"listener", "target"} {
		if !strings.Contains(forward, want) {
			t.Errorf("the forward's own segments are missing %q:\n%s", want, forward)
		}
	}
}

// TestNoSegmentsMeansNoSegmentedDrawing pins the fallback.
//
// Inventing segments for a connection whose detail has not loaded would be worse
// than drawing the summary: it would state a route shape Portico has not been
// told.
func TestNoSegmentsMeansNoSegmentedDrawing(t *testing.T) {
	if out := RenderSegmentedRoute(RouteVM{State: RouteOpen}, 80, false); out != "" {
		t.Fatalf("a route with no segments was drawn anyway:\n%s", out)
	}
	// RenderRoute still produces something, from the summary.
	if out := RenderRoute(RouteVM{
		State: RouteOpen, LocalLabel: "web", EndpointLabel: "https://x.example.com",
	}, 80, false); out == "" {
		t.Fatal("a connection with no segments rendered no route at all")
	}
}

// TestSegmentedRouteFitsItsWidth pins that the drawing respects the terminal.
func TestSegmentedRouteFitsItsWidth(t *testing.T) {
	for _, width := range []int{24, 40, 60, 80, 110, 200} {
		out := RenderSegmentedRoute(RouteVM{
			State:         RouteDegraded,
			Segments:      healthySegments(),
			Protection:    &CheckpointVM{Label: "sign-in", Active: true},
			ActiveFinding: &FindingVM{SegmentID: core.SegmentAddress, Summary: "the record points elsewhere"},
		}, width, false)
		for i, line := range strings.Split(out, "\n") {
			if w := DisplayWidth(line); w > width {
				t.Errorf("width %d: line %d is %d cells:\n%q", width, i, w, line)
			}
		}
	}
}

// TestCompactRouteBreaksAtTheFailure pins the one-line form.
func TestCompactRouteBreaksAtTheFailure(t *testing.T) {
	segments := healthySegments()
	segments[1].Status = SegmentFailed

	out := RenderCompactRoute(RouteVM{
		State:    RouteDegraded,
		Segments: segments,
		ActiveFinding: &FindingVM{
			SegmentID: core.SegmentConnector,
			Summary:   "connector stopped",
		},
		EndpointLabel: "https://x.example.com",
	}, 60, false)

	if !strings.ContainsRune(out, '╳') {
		t.Errorf("the compact route does not show the break:\n%s", out)
	}
	// The endpoint is not drawn past a break: it is not reachable.
	if strings.Contains(out, "x.example.com") {
		t.Errorf("the compact route shows an endpoint past the break:\n%s", out)
	}
	if !strings.Contains(out, "connector stopped") {
		t.Errorf("the compact route does not say what is wrong:\n%s", out)
	}
	if w := DisplayWidth(out); w > 60 {
		t.Errorf("the compact route is %d cells wide, want at most 60", w)
	}
}

// TestASCIIFallbackDrawsNoBoxGlyphs pins the fallback for terminals that cannot
// render the box-drawing set.
func TestASCIIFallbackDrawsNoBoxGlyphs(t *testing.T) {
	segments := healthySegments()
	segments[2].Status = SegmentFailed

	out := RenderSegmentedRoute(RouteVM{
		State:      RouteDegraded,
		Segments:   segments,
		Protection: &CheckpointVM{Label: "sign-in", Active: true},
		DNSDrift:   true,
	}, 80, true)

	for _, forbidden := range []rune{'━', '┅', '┄', '╳', '┃', '⇥'} {
		if strings.ContainsRune(out, forbidden) {
			t.Errorf("the ASCII route contains %q:\n%s", forbidden, out)
		}
	}
	if !strings.ContainsRune(out, 'X') {
		t.Errorf("the ASCII route does not mark the break:\n%s", out)
	}
}
