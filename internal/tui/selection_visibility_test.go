package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestDiscoverySelectionStaysPhysicallyVisible pins the boundary the audit
// failed at: moving a selection must move the view with it.
//
// The scroll offset and the selection were independent, so on a list longer
// than the screen Down walked the cursor below the clipped viewport. The
// selection still existed; the user just could not see what they were
// steering. Every move below asserts the selected row is physically inside the
// rendered viewport.
func TestDiscoverySelectionStaysPhysicallyVisible(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.width, m.height = 80, 24
	m.scroll.setHeight(24)

	services := make([]ipc.DiscoveredServiceDTO, 200)
	for i := range services {
		services[i] = ipc.DiscoveredServiceDTO{
			Address:    fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol:   "http",
			Confidence: "likely",
			Selectable: true,
		}
	}
	m.discovery = services
	m.discoveryErr = nil
	m.discoverySelected = 0
	m.transitionTo(ScreenDiscovery)

	assertSelectedVisible := func(stage string) {
		t.Helper()
		content := m.renderDiscovery()
		lines := strings.Split(content, "\n")
		// The selection marker is drawn at the start of the selected row.
		row := -1
		for i, line := range lines {
			if strings.Contains(line, "> 127.0.0.1:") || strings.HasPrefix(line, "> ") {
				row = i
				break
			}
		}
		if row < 0 {
			t.Fatalf("%s: the selected row was not rendered at all:\n%s", stage, content)
		}
		// Clip the content the way View does and confirm the marker survives.
		clipped := clipToViewport(content, m.height, m.scroll.offset)
		if !strings.Contains(clipped, "> 127.0.0.1:") && !containsSelectedRow(clipped, m.discoverySelected) {
			t.Fatalf("%s: selected row %d is outside the visible viewport (offset %d):\n%s",
				stage, m.discoverySelected, m.scroll.offset, clipped)
		}
		for _, line := range strings.Split(clipped, "\n") {
			if w := ansi.StringWidth(line); w > m.width {
				t.Fatalf("%s: line %d is %d cells wide, exceeds %d", stage, row, w, m.width)
			}
		}
	}

	for i := 0; i < 60; i++ {
		m.moveSelection(1)
		assertSelectedVisible(fmt.Sprintf("down %d", i+1))
	}
	for i := 0; i < 60; i++ {
		m.moveSelection(-1)
		assertSelectedVisible(fmt.Sprintf("up %d", i+1))
	}
	for i := 0; i < 10; i++ {
		m.scroll.scrollBy(m.scroll.page())
	}
	m.moveSelection(1)
	assertSelectedVisible("after page down drift")
	m.scroll.toBottom()
	m.moveSelection(1)
	assertSelectedVisible("after scroll-to-bottom drift")
	m.moveSelection(1)
	assertSelectedVisible("second move after drift")
}

// containsSelectedRow checks that the address of the currently selected
// service appears with the selection marker in the clipped view.
func containsSelectedRow(clipped string, selected int) bool {
	want := fmt.Sprintf("> 127.0.0.1:%d", 3000+selected)
	return strings.Contains(clipped, want)
}

// TestDiscoveryEndKeyReachesTheLastRow pins Home/End across the full list.
func TestDiscoveryEndKeyReachesTheLastRow(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.width, m.height = 80, 24
	m.scroll.setHeight(24)

	services := make([]ipc.DiscoveredServiceDTO, 120)
	for i := range services {
		services[i] = ipc.DiscoveredServiceDTO{
			Address:    fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol:   "http",
			Confidence: "likely",
			Selectable: true,
		}
	}
	m.discovery = services
	m.discoveryErr = nil
	m.transitionTo(ScreenDiscovery)

	m.moveSelection(len(services) - 1)
	if m.discoverySelected != len(services)-1 {
		t.Fatalf("selected = %d, want %d", m.discoverySelected, len(services)-1)
	}
	clipped := clipToViewport(m.renderDiscovery(), m.height, m.scroll.offset)
	if !strings.Contains(clipped, fmt.Sprintf("> 127.0.0.1:%d", 3000+len(services)-1)) {
		t.Fatalf("End did not bring the last row into view (offset %d):\n%s",
			m.scroll.offset, clipped)
	}

	m.moveSelection(-(len(services) - 1))
	if m.discoverySelected != 0 {
		t.Fatalf("selected = %d, want 0", m.discoverySelected)
	}
	if m.scroll.offset != 0 {
		t.Fatalf("returning to the first row left the viewport at offset %d", m.scroll.offset)
	}
}

// TestDiscoveryFailedScanIsNotAnEmptyMachine pins the Discovery screen's
// honesty requirement: a failed scan explains itself and never claims that
// nothing is listening.
func TestDiscoveryFailedScanIsNotAnEmptyMachine(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.width, m.height = 80, 24
	m.scroll.setHeight(24)

	m.discovery = nil
	m.discoveryErr = fmt.Errorf("context deadline exceeded")
	m.transitionTo(ScreenDiscovery)

	view := m.renderDiscovery()
	if !strings.Contains(view, "scan itself failed") {
		t.Fatalf("a failed scan is presented as an empty result:\n%s", view)
	}
	if strings.Contains(view, "No local listeners found") {
		t.Fatalf("a failed scan claims the machine is idle:\n%s", view)
	}
	if !strings.Contains(view, "did not establish that nothing is running") {
		t.Fatalf("the failed scan does not bound its claim:\n%s", view)
	}
}

// TestDiscoveryEvidenceToggleRendersEvidence pins [i] Why this?: toggling the
// evidence flag must put the classification's evidence on the screen. The
// action used to flip a field no renderer read, so the advertised key did
// nothing a user could observe.
func TestDiscoveryEvidenceToggleRendersEvidence(t *testing.T) {
	m := readyModel(&fakeClient{}, testSnapshot())
	m.width, m.height = 80, 24
	m.scroll.setHeight(24)

	m.discovery = []ipc.DiscoveredServiceDTO{{
		Address:    "127.0.0.1:3000",
		Protocol:   "http",
		Confidence: "likely",
		Process:    "node",
		Evidence:   "responded to GET / with x-powered-by: Next.js",
		Selectable: true,
	}}
	m.discoveryErr = nil
	m.discoverySelected = 0
	m.transitionTo(ScreenDiscovery)

	before := m.renderDiscovery()
	if strings.Contains(before, "x-powered-by") {
		t.Fatal("evidence is shown before [i] was pressed")
	}

	m.discoveryEvidence = true
	after := m.renderDiscovery()
	if !strings.Contains(after, "x-powered-by: Next.js") {
		t.Fatalf("[i] did not reveal the evidence:\n%s", after)
	}
	if !strings.Contains(after, "Probably right") {
		t.Fatalf("the confidence is not explained in words:\n%s", after)
	}
}
