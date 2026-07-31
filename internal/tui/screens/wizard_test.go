package screens

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
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

	m := NewWizard(nil, fullCloudflareSnapshot())
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
	if got := NewWizard(nil, quickTunnelOnlySnapshot()).protections(); !reflect.DeepEqual(got, []string{"none"}) {
		t.Fatalf("limited choices = %#v", got)
	}
	full := NewWizard(nil, fullCloudflareSnapshot())
	full.state.ExposureMode = "permanent_public"
	if got := full.protections(); !reflect.DeepEqual(got, []string{"none", "email_otp"}) {
		t.Fatalf("full choices = %#v", got)
	}
}

func TestWizardSelectsConfiguredCloudflareAccount(t *testing.T) {
	// Accounts belong to their provider in the snapshot, so a wizard with more
	// than one provider cannot offer another provider's accounts.
	snapshot := fullCloudflareSnapshot()
	snapshot[0].Accounts = []ipc.ProviderAccountDTO{
		{ID: "account-a", Label: "Personal", Status: "authenticated"},
		{ID: "account-b", Label: "Work", Status: "authenticated"},
	}
	m := NewWizard(nil, snapshot)
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
	m := NewWizard(nil, fullCloudflareSnapshot())
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
	m := NewWizard(nil, fullCloudflareSnapshot())
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
	m := NewWizard(nil, fullCloudflareSnapshot())
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
	m := NewWizard(nil, quickTunnelOnlySnapshot())
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
	full := NewWizard(nil, fullCloudflareSnapshot())
	full.state = WizardState{Step: WizardStepDirectoryMode, SourceType: "directory"}
	fullChoices := full.directoryModeChoices()
	if len(fullChoices) != 4 {
		t.Fatalf("full cloudflare directory choices = %d, want 4", len(fullChoices))
	}
}

func TestMCPWizardMapsTransportAndConstrainsSSEExposure(t *testing.T) {
	limited := NewWizard(nil, quickTunnelOnlySnapshot())
	if got := limited.mcpTransports(); !reflect.DeepEqual(got, []string{"http", "streamable_http"}) {
		t.Fatalf("limited MCP transports = %#v", got)
	}

	m := NewWizard(nil, fullCloudflareSnapshot())
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
	m := NewWizard(nil, fullCloudflareSnapshot())
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
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.state = WizardState{Step: WizardStepPort, SourceType: "mcp_server", MCPCommand: true}
	m.HandleKey("enter")
	if m.Step() != WizardStepPort || m.err == nil || !strings.Contains(m.err.Error(), "requires a local port") {
		t.Fatalf("state after empty MCP command port = %#v, err = %v", m.state, m.err)
	}
}

func TestWizardBuildRequestSetsLifecycleDefaults(t *testing.T) {
	m := NewWizard(nil, quickTunnelOnlySnapshot())
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
	m := NewWizard(nil, quickTunnelOnlySnapshot())
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

	recommendation       *ipc.ProviderRecommendationResponse
	recommendErr         error
	recommendCalls       int
	lastRecommendRequest ipc.ProviderRecommendationRequest
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
	m := NewWizard(client, quickTunnelOnlySnapshot())
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
	// "completed" is the operation's terminal success state. A test asserting
	// "succeeded" describes a state the controller never produces, so it
	// verified the behaviour of an impossible input.
	m.operation.State = ipc.OperationCompleted
	m.HandleOperationLoaded(opLoaded)
	if m.Step() != WizardStepComplete {
		t.Fatalf("step after success = %d, want complete", m.Step())
	}
}

func TestWizardSaveClosedSkipsPlan(t *testing.T) {
	client := &fakeWizardClient{createdID: "conn-new"}
	m := NewWizard(client, quickTunnelOnlySnapshot())
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

// TestWizardUsesEventStreamRatherThanTightPolling pins audit item 20. The
// wizard polled the operation every 750ms regardless of the event stream that
// already existed, so progress was driven by a timer rather than by events.
func TestWizardUsesEventStreamRatherThanTightPolling(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.operation = &ipc.OperationDTO{ID: "op-1", State: "running"}

	// With the stream live, polling backs off to a safety net.
	m.SetStreamConnected(true)
	connectedStart := time.Now()
	cmd := m.pollOperationCmd()
	if cmd == nil {
		t.Fatal("no fallback poll command")
	}

	// The event-driven refresh must not wait on any timer.
	refresh := m.RefreshOperationCmd()
	if refresh == nil {
		t.Fatal("no event-driven refresh command")
	}
	done := make(chan struct{})
	go func() {
		refresh()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("event-driven refresh waited on a timer instead of running immediately")
	}
	if elapsed := time.Since(connectedStart); elapsed > time.Second {
		t.Fatalf("refresh path took %s", elapsed)
	}
}

// TestWizardExposesItsOperationForEventRouting ensures the root model can match
// incoming events to the wizard's operation.
func TestWizardExposesItsOperationForEventRouting(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	if m.OperationID() != "" {
		t.Fatalf("OperationID = %q before an operation exists", m.OperationID())
	}
	m.operation = &ipc.OperationDTO{ID: "op-42"}
	if m.OperationID() != "op-42" {
		t.Fatalf("OperationID = %q, want op-42", m.OperationID())
	}
}

// TestWizardOpensOnAnOutcomeQuestion pins audit item 14. The wizard opened on
// "What should be reachable?", a source-type question, so a user had to
// understand Portico's internal model before stating a goal.
func TestWizardOpensOnAnOutcomeQuestion(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	if m.Step() != WizardStepOutcome {
		t.Fatalf("wizard opens on step %d, want the outcome step", m.Step())
	}
	view := m.View()
	if !strings.Contains(view, "What are you trying to do?") {
		t.Fatalf("first screen is not an outcome question:\n%s", view)
	}
	// The consequence of the highlighted choice must be visible before it is
	// chosen, not after.
	if !strings.Contains(view, "Anyone with the link") {
		t.Fatalf("outcome screen does not explain the consequence:\n%s", view)
	}
}

// TestChoosingAnOutcomeSkipsTheQuestionsItAnswers ensures a recipe presets what
// it determines rather than asking again in provider terminology.
func TestChoosingAnOutcomeSkipsTheQuestionsItAnswers(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	m.HandleKey("enter") // first recipe: temporary web app

	if m.state.SourceType != "existing_service" {
		t.Fatalf("source type = %q", m.state.SourceType)
	}
	if m.state.ExposureMode != "temporary_public" {
		t.Fatalf("exposure = %q, want temporary_public", m.state.ExposureMode)
	}
	if m.state.Protection != "none" {
		t.Fatalf("protection = %q, want none", m.state.Protection)
	}
	if m.Step() != WizardStepName {
		t.Fatalf("step = %d, want the name step", m.Step())
	}
}

// TestAdvancedOutcomeFallsBackToTheSourceQuestion keeps the original flow
// reachable for users who want it.
func TestAdvancedOutcomeFallsBackToTheSourceQuestion(t *testing.T) {
	m := NewWizard(nil, fullCloudflareSnapshot())
	for range len(wizardRecipes) - 1 {
		m.HandleKey("down")
	}
	m.HandleKey("enter")

	if m.Step() != WizardStepIntent {
		t.Fatalf("step = %d, want the source-type step", m.Step())
	}
	if m.state.SourceType != "" {
		t.Fatalf("advanced flow presets a source type: %q", m.state.SourceType)
	}
	if !strings.Contains(m.View(), "What should be reachable?") {
		t.Fatal("advanced flow does not reach the source question")
	}
}

// TestUnavailableOutcomeIsRefusedRatherThanSubstituted pins the safety rule
// from audit item 5: an MCP server bound for ChatGPT must not be silently
// published at a public address instead.
func TestUnavailableOutcomeIsRefusedRatherThanSubstituted(t *testing.T) {
	index := -1
	for i, recipe := range wizardRecipes {
		if strings.Contains(recipe.Label, "ChatGPT") {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("no ChatGPT outcome is offered")
	}

	m := NewWizard(nil, fullCloudflareSnapshot())
	for range index {
		m.HandleKey("down")
	}
	m.HandleKey("enter")

	// It must not proceed into the ordinary public-exposure flow.
	if m.Step() != WizardStepOutcome {
		t.Fatalf("an unavailable outcome advanced the wizard to step %d", m.Step())
	}
	if m.state.ExposureMode == "temporary_public" || m.state.ExposureMode == "permanent_public" {
		t.Fatalf("an unavailable private outcome fell back to public exposure: %q", m.state.ExposureMode)
	}
	view := m.View()
	if !strings.Contains(view, "experimental in Portico and off by default") {
		t.Fatalf("refusal does not explain itself:\n%s", view)
	}
	// It must also say how to enable it, not merely that it is unavailable.
	if !strings.Contains(view, "PORTICO_ENABLE_EXPERIMENTAL_OPENAI_TUNNEL") {
		t.Fatalf("refusal does not say how to enable the feature:\n%s", view)
	}
	if !strings.Contains(view, "expose it to anyone who finds the URL") {
		t.Fatalf("refusal does not explain the risk of the alternative:\n%s", view)
	}
}

// fullCloudflareSnapshot is a Cloudflare with an authenticated account: it can
// own a hostname and apply Access protection.
func fullCloudflareSnapshot() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "ready", Readiness: "ready", Selectable: true,
		Accounts: []ipc.ProviderAccountDTO{{ID: "acct-1", Label: "Personal", Status: "authenticated"}},
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true,
			CustomHostnames:    true,
			ManagedDNS:         true,
			ProtectionModes:    []string{"none", "email_otp"},
			Protocols:          []string{"http", "https"},
		},
	}}
}

// quickTunnelOnlySnapshot is a Cloudflare with no account: temporary addresses
// only, and no protection, because Access needs a hostname it owns.
func quickTunnelOnlySnapshot() []ipc.ProviderDTO {
	return []ipc.ProviderDTO{{
		ID: "cloudflare", DisplayName: "Cloudflare",
		Availability: "ready", Readiness: "ready", Selectable: true,
		Capabilities: &ipc.CapabilitySetDTO{
			TemporaryAddresses: true,
			ProtectionModes:    []string{"none"},
			Protocols:          []string{"http", "https"},
		},
	}}
}

// RecommendProvider lets the fake stand in for a supervisor that evaluates
// providers. The zero response means "no recommendation", which the wizard must
// handle without defaulting to a provider it never evaluated.
func (f *fakeWizardClient) RecommendProvider(_ context.Context, req ipc.ProviderRecommendationRequest) (
	*ipc.ProviderRecommendationResponse, error,
) {
	f.recommendCalls++
	f.lastRecommendRequest = req
	if f.recommendErr != nil {
		return nil, f.recommendErr
	}
	if f.recommendation != nil {
		return f.recommendation, nil
	}
	return &ipc.ProviderRecommendationResponse{
		Recommended: &ipc.ProviderChoiceDTO{ProviderID: "cloudflare", DisplayName: "Cloudflare"},
	}, nil
}
