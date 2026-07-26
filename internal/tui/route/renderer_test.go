package route

import (
	"strings"
	"testing"
)

func TestRenderRouteASCIIContainsNoUnicodeRouteGlyphs(t *testing.T) {
	got := RenderRoute(RouteVM{
		LocalLabel:    "local",
		EndpointLabel: "public.example",
		State:         RouteOpen,
	}, 80, true)
	for _, forbidden := range []string{"●", "◈", "━", "╮", "╭", "╰", "╳"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("ASCII route contains %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "#") || !strings.Contains(got, "=") {
		t.Fatalf("ASCII route missing fallback glyphs:\n%s", got)
	}
}

func TestRenderCompactRouteASCII(t *testing.T) {
	got := RenderRoute(RouteVM{LocalLabel: "local", State: RouteClosed}, 10, true)
	if strings.ContainsAny(got, "○◈━") || !strings.Contains(got, "O===#===O") {
		t.Fatalf("unexpected compact ASCII route: %q", got)
	}
}
