package screens

import (
	"context"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// Finding a service from inside the wizard.
//
// Publishing something already running required knowing that Home had a separate
// discovery shortcut and using it before starting the wizard. Otherwise the
// wizard asked for an address, and the user had to leave to find one.

// discoveringClient is a wizard client that can also scan.
type discoveringClient struct {
	nullCreator
	services []ipc.DiscoveredServiceDTO
	err      error
	scans    int
}

func (c *discoveringClient) Discovery(context.Context) (*ipc.DiscoveryDTO, error) {
	return c.refresh()
}

func (c *discoveringClient) RefreshDiscovery(context.Context) (*ipc.DiscoveryDTO, error) {
	c.scans++
	return c.refresh()
}

func (c *discoveringClient) refresh() (*ipc.DiscoveryDTO, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &ipc.DiscoveryDTO{Services: c.services}, nil
}

// nullCreator satisfies ConnectionCreator without doing anything, so a test can
// supply only the half it cares about.
type nullCreator struct{}

func (nullCreator) CreateConnection(context.Context, ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	return &ipc.ConnectionDTO{ID: "conn-new"}, nil
}
func (nullCreator) RecommendProvider(context.Context, ipc.ProviderRecommendationRequest) (
	*ipc.ProviderRecommendationResponse, error) {
	return &ipc.ProviderRecommendationResponse{}, nil
}
func (nullCreator) PlanOpen(context.Context, string) (*ipc.PlanDTO, error) {
	return &ipc.PlanDTO{ID: "plan-1", Intent: "open"}, nil
}
func (nullCreator) ApplyPlan(context.Context, string) (*ipc.OperationDTO, error) {
	return &ipc.OperationDTO{ID: "op-1"}, nil
}
func (nullCreator) ApplyPlanWithIdempotency(context.Context, string, string) (*ipc.OperationDTO, error) {
	return &ipc.OperationDTO{ID: "op-1"}, nil
}
func (nullCreator) GetOperation(context.Context, string) (*ipc.OperationDTO, error) {
	return &ipc.OperationDTO{ID: "op-1", State: "succeeded"}, nil
}

// discoveredServices are two plausible scan results, one identified and one
// not. The grades are the discovery engine's own (internal/discovery); a test
// fixture using a grade production never emits proved only the behaviour of an
// impossible input.
func discoveredServices() []ipc.DiscoveredServiceDTO {
	return []ipc.DiscoveredServiceDTO{
		{
			Address: "127.0.0.1:3000", Port: 3000, Protocol: "http",
			Framework: "Next.js", Confidence: "likely", Process: "node", PID: 4242,
			Evidence:   "responded to GET / with x-powered-by: Next.js",
			Selectable: true,
		},
		{
			Address: "127.0.0.1:5432", Port: 5432, Protocol: "tcp",
			Confidence: "possible", Process: "postgres", PID: 900,
			Selectable: true,
		},
	}
}

// wizardAtDiscovery drives the wizard to the service question.
func wizardAtDiscovery(t *testing.T, client *discoveringClient) *WizardModel {
	t.Helper()
	m := NewWizard(client, quickTunnelOnlySnapshot())
	// Outcome: share a web app temporarily. That fixes the source kind as an
	// existing service, which is what makes the service question apply.
	m.HandleKey("enter")
	m.setInput("demo")
	cmd := m.HandleKey("enter")
	if m.Step() != WizardStepDiscovery {
		t.Fatalf("step after naming = %d, want the service question", m.Step())
	}
	if cmd == nil {
		t.Fatal("reaching the service question did not start a scan")
	}
	msg, ok := cmd().(WizardDiscoveryMsg)
	if !ok {
		t.Fatal("the service question did not produce a scan result")
	}
	m.HandleDiscovery(msg)
	return m
}

// TestTheWizardScansForTheService pins that the wizard asks Portico rather than
// asking the user.
func TestTheWizardScansForTheService(t *testing.T) {
	client := &discoveringClient{services: discoveredServices()}
	m := wizardAtDiscovery(t, client)

	if client.scans != 1 {
		t.Fatalf("scans = %d, want 1", client.scans)
	}

	view := m.View()
	// Address, protocol and the recognisable name of each service.
	for _, want := range []string{"127.0.0.1:3000", "http", "Next.js", "127.0.0.1:5432", "postgres"} {
		if !strings.Contains(view, want) {
			t.Errorf("the service question does not show %q:\n%s", want, view)
		}
	}
	// Manual entry is always offered.
	if !strings.Contains(view, "Enter an address manually") {
		t.Errorf("the service question does not offer manual entry:\n%s", view)
	}
}

// TestTheServiceQuestionShowsTheEvidence pins that the confidence is explained.
//
// "likely" is Portico's internal grade. The Evidence — what was probed and what
// answered — was gathered, carried across IPC and never displayed.
func TestTheServiceQuestionShowsTheEvidence(t *testing.T) {
	m := wizardAtDiscovery(t, &discoveringClient{services: discoveredServices()})

	// The highlighted choice expands with its evidence.
	view := m.View()
	if !strings.Contains(view, "x-powered-by: Next.js") {
		t.Errorf("the evidence behind the classification is not shown:\n%s", view)
	}
	if strings.Contains(view, "Confidence: likely.") {
		t.Error("the raw confidence grade is shown instead of what it means")
	}
	if !strings.Contains(view, "Probably right") {
		t.Errorf("the confidence is not explained in words:\n%s", view)
	}

	// The unidentified one is described as a guess rather than as a failure.
	lines := DiscoveryEvidenceLines(discoveredServices()[1])
	joined := strings.Join(lines, " ")
	if !strings.Contains(joined, "guess") {
		t.Errorf("an unidentified service is not described as a guess: %q", joined)
	}
	if !strings.Contains(joined, "no evidence") {
		t.Errorf("a service with no evidence does not say so: %q", joined)
	}
}

// TestChoosingADiscoveredServiceCarriesItsAddress pins the whole point: the
// answer to the question becomes the connection's source.
func TestChoosingADiscoveredServiceCarriesItsAddress(t *testing.T) {
	m := wizardAtDiscovery(t, &discoveringClient{services: discoveredServices()})

	m.HandleKey("enter") // take the highlighted service
	if m.state.SourceAddress != "127.0.0.1:3000" {
		t.Fatalf("source address = %q, want the discovered one", m.state.SourceAddress)
	}
	// The discovered protocol is taken as found rather than re-asked.
	if m.state.SourceProtocol != "http" {
		t.Fatalf("source protocol = %q, want http", m.state.SourceProtocol)
	}
	// A discovered host:port is complete, so the port question is behind us.
	if m.Step() == WizardStepDiscovery {
		t.Fatal("choosing a service did not advance the wizard")
	}
}

// TestManualEntryOpensTheAddressField pins that the list is not the only way on.
func TestManualEntryOpensTheAddressField(t *testing.T) {
	m := wizardAtDiscovery(t, &discoveringClient{services: discoveredServices()})

	// Manual entry is the last option.
	choices := m.discoveryChoiceList()
	m.selected = len(choices) - 1
	m.HandleKey("enter")

	if m.Step() != WizardStepSource {
		t.Fatalf("step = %d, want the address question", m.Step())
	}
	if m.state.SourceAddress != "" {
		t.Fatalf("the address field was prefilled with %q", m.state.SourceAddress)
	}
}

// TestFindingNothingIsNotADeadEnd pins the empty result.
//
// A service on a port the scan cannot see is still a service. Reporting that
// nothing was found and offering no way on is where the old Discovery screen
// stopped.
func TestFindingNothingIsNotADeadEnd(t *testing.T) {
	client := &discoveringClient{}
	m := wizardAtDiscovery(t, client)

	view := m.View()
	if !strings.Contains(view, "did not find anything") {
		t.Errorf("an empty scan does not say so:\n%s", view)
	}
	if !strings.Contains(view, "Enter an address manually") {
		t.Errorf("an empty scan offers no way forward:\n%s", view)
	}

	// Scanning again is offered, and works.
	cmd := m.HandleKey("r")
	if cmd == nil {
		t.Fatal("scanning again produced no command")
	}
	cmd()
	if client.scans != 2 {
		t.Fatalf("scans = %d, want 2", client.scans)
	}

	// And the action set advertises both ways out.
	var labels []string
	for _, action := range m.Actions() {
		labels = append(labels, action.Label)
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"Scan again", "Back"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the empty service question does not advertise %q: %s", want, joined)
		}
	}
}

// TestAFailedScanIsNotAnEmptyResult pins that the two are distinguished.
//
// Rendering a failed scan as "nothing is running" states something Portico never
// established.
func TestAFailedScanIsNotAnEmptyResult(t *testing.T) {
	m := NewWizard(&discoveringClient{err: context.DeadlineExceeded}, quickTunnelOnlySnapshot())
	m.HandleKey("enter")
	m.setInput("demo")
	cmd := m.HandleKey("enter")
	msg, _ := cmd().(WizardDiscoveryMsg)
	m.HandleDiscovery(msg)

	view := m.View()
	if !strings.Contains(view, "scan itself failed") {
		t.Errorf("a failed scan is presented as an empty result:\n%s", view)
	}
	if m.discovered != nil {
		t.Error("a failed scan left a service list behind")
	}
}

// TestGoingBackToTheServiceQuestionKeepsTheChoice pins that back-navigation does
// not silently move the answer.
func TestGoingBackToTheServiceQuestionKeepsTheChoice(t *testing.T) {
	m := wizardAtDiscovery(t, &discoveringClient{services: discoveredServices()})

	// Choose the second service, then go back to the question.
	m.selected = 1
	m.HandleKey("enter")
	if m.state.SourceAddress != "127.0.0.1:5432" {
		t.Fatalf("source address = %q", m.state.SourceAddress)
	}

	m.state.Step = WizardStepDiscovery
	m.restoreStepInput()
	if m.selected != 1 {
		t.Fatalf("returning to the question moved the cursor to %d, want 1", m.selected)
	}
}
