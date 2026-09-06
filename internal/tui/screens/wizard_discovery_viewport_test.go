package screens

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestWizardDiscoveryViewportAt60x18(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	services := make([]ipc.DiscoveredServiceDTO, 50)
	for i := range services {
		services[i] = ipc.DiscoveredServiceDTO{
			Address:     fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol:    "http",
			DisplayName: fmt.Sprintf("Service %03d", i),
		}
	}
	m.discovered = services
	for i := 0; i < 20; i++ {
		m.HandleKey("down")
	}
	m.HandleKey("pgdown")
	m.HandleKey("home")
	m.HandleKey("end")
	if m.selected != 50 {
		t.Fatalf("after end: selected=%d, want 50", m.selected)
	}
	wizView := m.View()
	lines := strings.Split(wizView, "\n")
	for _, line := range lines {
		if w := ansi.StringWidth(line); w > 60 {
			t.Errorf("line is %d cells wide, exceeds 60: %q", w, line)
		}
	}
	if !strings.Contains(wizView, "Enter an address manually") {
		t.Error("selected row NOT visible after End")
	}
}

func TestWizardViewportKeepsSelectionVisibleForLongDiscovery(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	m.discovered = make([]ipc.DiscoveredServiceDTO, 200)
	for i := range m.discovered {
		m.discovered[i] = ipc.DiscoveredServiceDTO{
			Address:  fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol: "http",
		}
	}
	m.HandleKey("end")
	if m.selected != 200 {
		t.Fatalf("selected = %d, want manual entry at 200", m.selected)
	}
	view := m.View()
	if !strings.Contains(view, "Enter an address manually") {
		t.Fatalf("manual entry is not visible after End:\n%s", view)
	}
	if strings.Contains(view, "Service 000") || strings.Contains(view, "127.0.0.1:3000") {
		t.Fatalf("viewport rendered the first off-screen service:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > 60 {
			t.Fatalf("line %q uses %d cells, want <= 60", line, width)
		}
	}
}

func TestWizardMenuStepPageDownFitsWindow(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	m.discovered = make([]ipc.DiscoveredServiceDTO, 50)
	for i := range m.discovered {
		m.discovered[i] = ipc.DiscoveredServiceDTO{
			Address:  fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol: "http",
		}
	}
	m.HandleKey("pgdown")
	if m.selected == 0 {
		t.Fatal("pgdown did not move selection")
	}
}

func TestWizardMenuStepHomeEnd(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	m.discovered = make([]ipc.DiscoveredServiceDTO, 30)
	for i := range m.discovered {
		m.discovered[i] = ipc.DiscoveredServiceDTO{
			Address:  fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol: "http",
		}
	}
	m.HandleKey("end")
	if m.selected != 30 {
		t.Fatalf("end: selected = %d, want 30", m.selected)
	}
	m.HandleKey("home")
	if m.selected != 0 {
		t.Fatalf("home: selected = %d, want 0", m.selected)
	}
}

func TestWizardViewportStaysReachableAt60x18(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepOutcome
	m.HandleKey("down")
	m.HandleKey("down")
	m.HandleKey("down")
	view := m.View()
	if view == "" {
		t.Fatal("empty view at 60x18")
	}
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > 60 {
			t.Fatalf("line %q is %d cells wide, exceeds 60", line, width)
		}
	}
}

func TestWizardDiscoveryFilterAndManualShortcut(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.Step = WizardStepDiscovery
	m.discovered = []ipc.DiscoveredServiceDTO{
		{Address: "127.0.0.1:3000", Protocol: "http", DisplayName: "Dashboard", Category: "application"},
		{Address: "127.0.0.1:4000", Protocol: "http", DisplayName: "Worker", Category: "application"},
	}
	m.discoveryFilter = "dashboard"
	choices := m.discoveryChoiceList()
	if len(choices) != 2 || choices[0].Label != "Dashboard · 127.0.0.1:3000 · http" {
		t.Fatalf("filtered choices = %#v", choices)
	}
	m.HandleKey("m")
	if m.state.Step != WizardStepSource || m.state.SourceAddress != "" {
		t.Fatalf("manual shortcut state = %#v", m.state)
	}
}

func TestWizardRepeatedDown(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	m.discovered = make([]ipc.DiscoveredServiceDTO, 25)
	for i := range m.discovered {
		m.discovered[i] = ipc.DiscoveredServiceDTO{
			Address:  fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol: "http",
		}
	}
	for i := 0; i < 30; i++ {
		m.HandleKey("down")
	}
	want := 25
	if m.selected != want {
		t.Fatalf("after 30 downs: selected=%d, want %d", m.selected, want)
	}
}

func TestWizardShowAllToggle(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.Step = WizardStepDiscovery
	m.discovered = []ipc.DiscoveredServiceDTO{
		{Address: "127.0.0.1:3000", Protocol: "http", Confidence: "high"},
		{Address: "127.0.0.1:4000", Protocol: "http", Confidence: "possible"},
	}
	choicesAll := m.discoveryChoiceList()
	if len(choicesAll) != 3 {
		t.Fatalf("show all: choices=%d, want 3", len(choicesAll))
	}
	m.HandleKey("a")
	choicesRestricted := m.discoveryChoiceList()
	if len(choicesRestricted) != 2 {
		t.Fatalf("show restricted: choices=%d, want 2", len(choicesRestricted))
	}
	m.HandleKey("a")
	choicesAll2 := m.discoveryChoiceList()
	if len(choicesAll2) != 3 {
		t.Fatalf("show all again: choices=%d, want 3", len(choicesAll2))
	}
}

func TestWizardEvidenceDisplay(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state.Step = WizardStepDiscovery
	m.discovered = []ipc.DiscoveredServiceDTO{
		{
			Address: "127.0.0.1:3000", Port: 3000, Protocol: "http",
			Framework: "Next.js", Confidence: "likely", Process: "node", PID: 4242,
			Evidence:   "responded to GET / with x-powered-by: Next.js",
			Selectable: true,
		},
	}
	m.HandleKey("enter")
	if m.state.SourceAddress != "127.0.0.1:3000" {
		t.Fatalf("source address = %q", m.state.SourceAddress)
	}
}

func TestWizardResize(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot()).SetSize(60, 18)
	m.state.Step = WizardStepDiscovery
	m.discovered = make([]ipc.DiscoveredServiceDTO, 200)
	for i := range m.discovered {
		m.discovered[i] = ipc.DiscoveredServiceDTO{
			Address:  fmt.Sprintf("127.0.0.1:%d", 3000+i),
			Protocol: "http",
		}
	}
	m.HandleKey("down")
	m.HandleKey("down")
	m.HandleKey("down")
	view := m.View()
	if view == "" {
		t.Fatal("empty view at 60x18")
	}
	for _, line := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(line); w > 60 {
			t.Fatalf("line %q is %d cells wide, exceeds 60", line, w)
		}
	}
	m.SetSize(80, 24)
	m.HandleKey("end")
	if m.selected != 200 {
		t.Fatalf("after resize+end: selected=%d, want 200", m.selected)
	}
}
