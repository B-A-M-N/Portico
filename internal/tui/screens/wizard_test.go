package screens

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestExistingServiceAddressPreservesPortsAndFormatsIPv6(t *testing.T) {
	tests := []struct {
		name    string
		address string
		port    string
		want    string
	}{
		{name: "empty defaults to loopback", port: "8080", want: "127.0.0.1:8080"},
		{name: "hostname", address: "localhost", port: "8080", want: "localhost:8080"},
		{name: "ipv4", address: "127.0.0.1", port: "8080", want: "127.0.0.1:8080"},
		{name: "raw ipv6", address: "::1", port: "8080", want: "[::1]:8080"},
		{name: "bracketed ipv6", address: "[::1]", port: "8080", want: "[::1]:8080"},
		{name: "existing port", address: "[::1]:3000", port: "8080", want: "[::1]:3000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := existingServiceAddress(test.address, test.port)
			if err != nil {
				t.Fatalf("existingServiceAddress(%q, %q): %v", test.address, test.port, err)
			}
			if got != test.want {
				t.Fatalf("existingServiceAddress(%q, %q) = %q, want %q", test.address, test.port, got, test.want)
			}
		})
	}
}

func TestExistingServiceAddressRejectsAmbiguousColonAddress(t *testing.T) {
	if _, err := existingServiceAddress("example.com:3000:extra", "8080"); err == nil {
		t.Fatal("expected invalid host:port address to be rejected")
	}
}

func TestEmailOTPProtectionRulesAreValidatedAndIncludedInRequest(t *testing.T) {
	emails, domains, err := parseProtectionRules("Person@example.com, @Example.com, person@example.com")
	if err != nil {
		t.Fatalf("parseProtectionRules: %v", err)
	}
	if want := []string{"person@example.com"}; !reflect.DeepEqual(emails, want) {
		t.Fatalf("emails = %#v, want %#v", emails, want)
	}
	if want := []string{"example.com"}; !reflect.DeepEqual(domains, want) {
		t.Fatalf("domains = %#v, want %#v", domains, want)
	}

	m := NewWizard(nil, true, nil)
	m.state = WizardState{
		Name:           "Protected service",
		SourceType:     "existing_service",
		SourceAddress:  "127.0.0.1",
		SourceProtocol: "http",
		Port:           "8080",
		ExposureMode:   "permanent_public",
		Hostname:       "demo.example.com",
		Protection:     "email_otp",
		AllowedEmails:  emails,
		AllowedDomains: domains,
		Provider:       "cloudflare",
	}
	req := m.buildRequest()
	if req.Protection.Kind != "email_otp" || !reflect.DeepEqual(req.Protection.AllowedEmails, emails) || !reflect.DeepEqual(req.Protection.AllowedDomains, domains) {
		t.Fatalf("protection request = %#v", req.Protection)
	}
}

func TestEmailOTPProtectionRulesRejectEmptyAndMalformedValues(t *testing.T) {
	for _, input := range []string{"", "not an address", "@bad_domain"} {
		if _, _, err := parseProtectionRules(input); err == nil {
			t.Fatalf("parseProtectionRules(%q) unexpectedly succeeded", input)
		}
	}
}

func TestProtectionChoicesFollowConfiguredCapabilities(t *testing.T) {
	if got := NewWizard(nil, false, nil).protections(); !reflect.DeepEqual(got, []string{"none"}) {
		t.Fatalf("limited choices = %#v", got)
	}
	full := NewWizard(nil, true, nil)
	full.state.ExposureMode = "permanent_public"
	if got := full.protections(); !reflect.DeepEqual(got, []string{"none", "email_otp"}) {
		t.Fatalf("full choices = %#v", got)
	}
}

func TestWizardSelectsConfiguredCloudflareAccount(t *testing.T) {
	m := NewWizard(nil, true, []ipc.ProviderAccountDTO{
		{ID: "account-a", Label: "Personal"},
		{ID: "account-b", Label: "Work"},
	})
	m.state = WizardState{
		Step: WizardStepProvider, Name: "Service", SourceType: "existing_service",
		SourceAddress: "127.0.0.1", SourceProtocol: "http", Port: "8080",
		ExposureMode: "permanent_public", Hostname: "service.example.com", Protection: "none",
	}
	m.HandleKey("enter")
	if m.Step() != WizardStepAccount {
		t.Fatalf("step = %d, want account selection", m.Step())
	}
	m.HandleKey("down")
	m.HandleKey("enter")
	if m.Step() != WizardStepReview || m.state.AccountID != "account-b" {
		t.Fatalf("selected state = %#v", m.state)
	}
	if got := m.buildRequest().Provider.AccountID; got != "account-b" {
		t.Fatalf("request account ID = %q, want account-b", got)
	}
}

func TestWizardDoesNotOfferUnsupportedProtectionOrExposure(t *testing.T) {
	m := NewWizard(nil, true, nil)
	if view := m.renderExposure(); !strings.Contains(view, "Permanently") {
		t.Fatalf("full Cloudflare exposure options omit permanent exposure: %s", view)
	}
	m.state.ExposureMode = "temporary_public"
	if view := m.renderProtection(); strings.Contains(view, "one-time passcode") {
		t.Fatalf("quick-tunnel protection options include unsupported email OTP: %s", view)
	}
}

func TestCommandWizardMapsArgumentsAndWorkingDirectory(t *testing.T) {
	args, err := parseCommandArgs(`serve --host 127.0.0.1 --title "hello world"`)
	if err != nil {
		t.Fatalf("parseCommandArgs: %v", err)
	}
	if want := []string{"serve", "--host", "127.0.0.1", "--title", "hello world"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	m := NewWizard(nil, true, nil)
	m.state = WizardState{
		Name: "Command service", SourceType: "command", SourceAddress: "npm", Port: "3000",
		CommandArgs: args, WorkingDir: "/work/app", ExposureMode: "temporary_public", Protection: "none", Provider: "cloudflare",
	}
	req := m.buildRequest()
	if req.Source.Command == nil || !reflect.DeepEqual(req.Source.Command.Args, args) || req.Source.Command.WorkingDir != "/work/app" {
		t.Fatalf("command request = %#v", req.Source.Command)
	}
}

// TestCommandArgsSupportValuesContainingCommas pins the replacement of the
// comma separator. A comma was previously the argument delimiter, so an
// argument containing one could not be expressed at all.
func TestCommandArgsSupportValuesContainingCommas(t *testing.T) {
	args, err := parseCommandArgs(`python -m my_server --name "Example, Inc."`)
	if err != nil {
		t.Fatalf("parseCommandArgs: %v", err)
	}
	want := []string{"python", "-m", "my_server", "--name", "Example, Inc."}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

// TestCommandArgsRoundTripThroughTheInputLine ensures an argument survives being
// rendered back into the editable line and parsed again.
func TestCommandArgsRoundTripThroughTheInputLine(t *testing.T) {
	original := []string{"serve", "--title", "hello world", "--name", "Example, Inc.", "--path", `C:\tmp`, "--empty", ""}
	line := commandArgsInput(original)
	parsed, err := parseCommandArgs(line)
	if err != nil {
		t.Fatalf("parseCommandArgs(%q): %v", line, err)
	}
	if !reflect.DeepEqual(parsed, original) {
		t.Fatalf("round trip changed arguments:\n line   = %s\n parsed = %#v\n want   = %#v", line, parsed, original)
	}
}

// TestCommandArgsRejectUnbalancedQuoting ensures a quoting mistake is reported
// rather than silently producing the wrong argv.
func TestCommandArgsRejectUnbalancedQuoting(t *testing.T) {
	if _, err := parseCommandArgs(`serve --title "unclosed`); err == nil {
		t.Fatal("expected an unclosed quote to be rejected")
	}
	if _, err := parseCommandArgs(`serve --path trailing\\`); err != nil {
		t.Fatalf("an escaped backslash should be accepted: %v", err)
	}
}

// TestArgvPreviewShowsExactArguments ensures the user can see the argument
// vector before the command is created.
func TestArgvPreviewShowsExactArguments(t *testing.T) {
	preview := renderArgvPreview("python", []string{"-m", "my_server", "--name", "Example, Inc."})
	for _, want := range []string{"Executable: python", "1. -m", "2. my_server", "4. Example, Inc."} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestDirectoryWizardMapsServingModeAndPermissions(t *testing.T) {
	m := NewWizard(nil, true, nil)
	m.state = WizardState{
		Name: "Files", SourceType: "directory", SourceAddress: "/srv/files",
		DirectoryMode: "writes", AllowUpload: true, AllowDelete: true,
		ExposureMode: "temporary_public", Protection: "none", Provider: "cloudflare",
	}
	req := m.buildRequest()
	if req.Source.Directory == nil || req.Source.Directory.Mode != "writes" || !req.Source.Directory.AllowUpload || !req.Source.Directory.AllowDelete {
		t.Fatalf("directory request = %#v", req.Source.Directory)
	}
	if got := directoryModeSummary(m.state); got != "file browser with uploads and deletes" {
		t.Fatalf("directory mode summary = %q", got)
	}
}

func TestDirectoryWizardQuickTunnelFiltersWriteModes(t *testing.T) {
	// Without full Cloudflare (Quick Tunnel only), write-enabled modes
	// should not be offered because they require protection, which
	// requires a permanent hostname.
	m := NewWizard(nil, false, nil)
	m.state = WizardState{Step: WizardStepDirectoryMode, SourceType: "directory"}

	choices := m.directoryModeChoices()
	if len(choices) != 2 {
		t.Fatalf("quick tunnel directory choices = %d, want 2", len(choices))
	}
	for _, c := range choices {
		if c.allowUpload || c.allowDelete {
			t.Fatalf("quick tunnel offered write-enabled mode: %+v", c)
		}
	}

	// With full Cloudflare, all 4 modes should be available.
	full := NewWizard(nil, true, nil)
	full.state = WizardState{Step: WizardStepDirectoryMode, SourceType: "directory"}
	fullChoices := full.directoryModeChoices()
	if len(fullChoices) != 4 {
		t.Fatalf("full cloudflare directory choices = %d, want 4", len(fullChoices))
	}
}

func TestMCPWizardMapsTransportAndConstrainsSSEExposure(t *testing.T) {
	limited := NewWizard(nil, false, nil)
	if got := limited.mcpTransports(); !reflect.DeepEqual(got, []string{"http", "streamable_http"}) {
		t.Fatalf("limited MCP transports = %#v", got)
	}

	m := NewWizard(nil, true, nil)
	m.state = WizardState{
		Name: "MCP service", SourceType: "mcp_server", SourceAddress: "http://127.0.0.1:3000/mcp",
		MCPTransport: "sse", ExposureMode: "permanent_public", Protection: "none", Provider: "cloudflare",
	}
	if got := m.exposures(); !reflect.DeepEqual(got, []string{"permanent_public"}) {
		t.Fatalf("SSE exposures = %#v", got)
	}
	req := m.buildRequest()
	if req.Source.MCP == nil || req.Source.MCP.Transport != "sse" || req.Source.MCP.Endpoint != m.state.SourceAddress {
		t.Fatalf("MCP request = %#v", req.Source.MCP)
	}
}

func TestMCPCommandWizardMapsOwnedCommand(t *testing.T) {
	m := NewWizard(nil, true, nil)
	m.state = WizardState{
		Name: "Managed MCP", SourceType: "mcp_server", MCPCommand: true, SourceAddress: "mcp-server",
		Port: "3001", CommandArgs: []string{"--port", "3001"}, WorkingDir: "/work/mcp",
		MCPTransport: "streamable_http", ExposureMode: "temporary_public", Protection: "none", Provider: "cloudflare",
	}
	req := m.buildRequest()
	if req.Source.MCP == nil || req.Source.MCP.Endpoint != "" || req.Source.MCP.Command == nil {
		t.Fatalf("MCP request = %#v", req.Source.MCP)
	}
	command := req.Source.MCP.Command
	if command.Executable != "mcp-server" || command.Port != 3001 || command.WorkingDir != "/work/mcp" || !reflect.DeepEqual(command.Args, []string{"--port", "3001"}) {
		t.Fatalf("MCP command = %#v", command)
	}
}

func TestCommandOriginRequiresPortBeforeAdvancing(t *testing.T) {
	m := NewWizard(nil, true, nil)
	m.state = WizardState{Step: WizardStepPort, SourceType: "mcp_server", MCPCommand: true}
	m.HandleKey("enter")
	if m.Step() != WizardStepPort || m.err == nil || !strings.Contains(m.err.Error(), "requires a local port") {
		t.Fatalf("state after empty MCP command port = %#v, err = %v", m.state, m.err)
	}
}

func TestWizardBuildRequestSetsLifecycleDefaults(t *testing.T) {
	m := NewWizard(nil, false, nil)
	m.state = WizardState{
		Step:           WizardStepReview,
		SourceType:     "existing_service",
		Name:           "test",
		SourceAddress:  "localhost",
		Port:           "8080",
		SourceProtocol: "https",
		ExposureMode:   "temporary_public",
		Protection:     "none",
		Provider:       "cloudflare",
	}
	req := m.buildRequest()
	if !req.Lifecycle.AutoStart {
		t.Fatal("wizard request should default AutoStart to true")
	}
	if req.Lifecycle.OnDisconnect != "keep_alive" {
		t.Fatalf("wizard request OnDisconnect = %q, want keep_alive", req.Lifecycle.OnDisconnect)
	}
	if req.Source.Existing == nil {
		t.Fatal("existing service source should be set")
	}
	if req.Source.Existing.Protocol != "https" {
		t.Fatalf("protocol = %q, want https", req.Source.Existing.Protocol)
	}
}

func TestWizardProtocolSelection(t *testing.T) {
	m := NewWizard(nil, false, nil)
	m.state = WizardState{
		Step:       WizardStepProtocol,
		SourceType: "existing_service",
	}

	// Default should be HTTP (index 0)
	if m.selected != 0 {
		t.Fatalf("initial protocol selection = %d, want 0", m.selected)
	}

	// Select HTTPS
	m.HandleKey("down")
	if m.selected != 1 {
		t.Fatalf("after down, protocol selection = %d, want 1", m.selected)
	}

	// Confirm selection
	m.HandleKey("enter")
	if m.state.SourceProtocol != "https" {
		t.Fatalf("protocol = %q, want https", m.state.SourceProtocol)
	}
	if m.state.Step != WizardStepExposure {
		t.Fatalf("step = %d, want exposure", m.state.Step)
	}
}

// fakeWizardClient implements ConnectionCreator for wizard tests.
type fakeWizardClient struct {
	createErr   error
	planErr     error
	applyErr    error
	getOpErr    error
	createdID   string
	plan        *ipc.PlanDTO
	operation   *ipc.OperationDTO
	createCalls int
	planCalls   int
	applyCalls  int
	getOpCalls  int
}

func (f *fakeWizardClient) CreateConnection(ctx context.Context, req ipc.CreateConnectionRequest) (*ipc.ConnectionDTO, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &ipc.ConnectionDTO{ID: f.createdID, Name: req.Name}, nil
}

func (f *fakeWizardClient) PlanOpen(ctx context.Context, connID string) (*ipc.PlanDTO, error) {
	f.planCalls++
	if f.planErr != nil {
		return nil, f.planErr
	}
	return f.plan, nil
}

func (f *fakeWizardClient) ApplyPlan(ctx context.Context, planID string) (*ipc.OperationDTO, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return f.operation, nil
}

func (f *fakeWizardClient) GetOperation(ctx context.Context, operationID string) (*ipc.OperationDTO, error) {
	f.getOpCalls++
	if f.getOpErr != nil {
		return nil, f.getOpErr
	}
	return f.operation, nil
}

func TestWizardOpenAfterCreateRequestsPlan(t *testing.T) {
	plan := &ipc.PlanDTO{
		ID:       "plan-1",
		Intent:   "open",
		Provider: "cloudflare",
		Steps:    []ipc.StepDTO{{Summary: "Create tunnel"}},
	}
	client := &fakeWizardClient{
		createdID: "conn-new",
		plan:      plan,
		operation: &ipc.OperationDTO{ID: "op-1", State: "running"},
	}
	m := NewWizard(client, false, nil)
	m.state = WizardState{
		Step:          WizardStepReview,
		SourceType:    "existing_service",
		Name:          "test",
		SourceAddress: "localhost",
		Port:          "8080",
		ExposureMode:  "temporary_public",
		Protection:    "none",
		Provider:      "cloudflare",
	}

	// Select "Save and open" (second option)
	m.selected = 1
	cmd := m.HandleKey("enter")
	if cmd == nil {
		t.Fatal("expected create command")
	}
	if m.Step() != WizardStepCreating {
		t.Fatalf("step = %d, want creating", m.Step())
	}

	// Execute create command
	msg := cmd()
	created, ok := msg.(ConnectionCreatedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want ConnectionCreatedMsg", msg)
	}

	// Handle created — should transition to plan preview and request plan
	planCmd := m.HandleCreated(created)
	if m.Step() != WizardStepPlanPreview {
		t.Fatalf("step after create = %d, want plan preview", m.Step())
	}
	if !m.openAfterCreate {
		t.Fatal("openAfterCreate should be true")
	}
	if planCmd == nil {
		t.Fatal("expected plan request command")
	}

	// Execute plan command
	planMsg := planCmd()
	planLoaded, ok := planMsg.(WizardPlanLoadedMsg)
	if !ok {
		t.Fatalf("plan cmd returned %T, want WizardPlanLoadedMsg", planMsg)
	}
	if client.planCalls != 1 {
		t.Fatalf("plan calls = %d, want 1", client.planCalls)
	}

	// Handle plan loaded
	m.HandlePlanLoaded(planLoaded)
	if m.plan == nil || m.plan.ID != "plan-1" {
		t.Fatal("plan not stored on wizard")
	}

	// Apply the plan
	applyCmd := m.HandleKey("enter")
	if applyCmd == nil {
		t.Fatal("expected apply command")
	}
	if m.Step() != WizardStepApplying {
		t.Fatalf("step after apply = %d, want applying", m.Step())
	}

	// Execute apply command
	applyMsg := applyCmd()
	applied, ok := applyMsg.(WizardPlanAppliedMsg)
	if !ok {
		t.Fatalf("apply cmd returned %T, want WizardPlanAppliedMsg", applyMsg)
	}
	if client.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", client.applyCalls)
	}

	// Handle applied — should transition to operation wait and start polling
	pollCmd := m.HandlePlanApplied(applied)
	if m.Step() != WizardStepOperationWait {
		t.Fatalf("step after apply = %d, want operation wait", m.Step())
	}
	if pollCmd == nil {
		t.Fatal("expected poll command")
	}

	// Execute poll — operation succeeded
	opMsg := pollCmd()
	opLoaded, ok := opMsg.(WizardOperationLoadedMsg)
	if !ok {
		t.Fatalf("poll cmd returned %T, want WizardOperationLoadedMsg", opMsg)
	}
	m.operation.State = "succeeded"
	m.HandleOperationLoaded(opLoaded)
	if m.Step() != WizardStepComplete {
		t.Fatalf("step after success = %d, want complete", m.Step())
	}
}

func TestWizardSaveClosedSkipsPlan(t *testing.T) {
	client := &fakeWizardClient{createdID: "conn-new"}
	m := NewWizard(client, false, nil)
	m.state = WizardState{
		Step:          WizardStepReview,
		SourceType:    "existing_service",
		Name:          "test",
		SourceAddress: "localhost",
		Port:          "8080",
		ExposureMode:  "temporary_public",
		Protection:    "none",
		Provider:      "cloudflare",
	}

	// Select "Save closed" (first option, default)
	m.selected = 0
	cmd := m.HandleKey("enter")
	if cmd == nil {
		t.Fatal("expected create command")
	}

	// Execute create command
	msg := cmd()
	created := msg.(ConnectionCreatedMsg)

	// Handle created — should go directly to complete, no plan
	planCmd := m.HandleCreated(created)
	if m.Step() != WizardStepComplete {
		t.Fatalf("step after create = %d, want complete", m.Step())
	}
	if m.openAfterCreate {
		t.Fatal("openAfterCreate should be false")
	}
	if planCmd != nil {
		t.Fatal("save closed should not request a plan")
	}
	if client.planCalls != 0 {
		t.Fatalf("plan calls = %d, want 0", client.planCalls)
	}
}

// TestWizardEditInputIsRuneAware pins the wizard's own text input against the
// same byte-slicing defect as the root model's editor.
func TestWizardEditInputIsRuneAware(t *testing.T) {
	typed := ""
	for _, r := range "café中🔥" {
		typed = editInput(typed, string(r))
	}
	if typed != "café中🔥" {
		t.Fatalf("typed %q, want café中🔥", typed)
	}
	for range utf8.RuneCountInString(typed) {
		typed = editInput(typed, "backspace")
		if !utf8.ValidString(typed) {
			t.Fatalf("backspace produced invalid UTF-8: %q", typed)
		}
	}
	if typed != "" {
		t.Fatalf("field = %q after deleting every rune", typed)
	}
	if got := editInput("", "backspace"); got != "" {
		t.Fatalf("backspace on empty field = %q", got)
	}
}
