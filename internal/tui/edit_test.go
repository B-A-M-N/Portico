package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func exposedDetail() *ipc.ConnectionDetailDTO {
	return &ipc.ConnectionDetailDTO{
		Summary: ipc.ConnectionDTO{
			ID: "conn-a", Name: "alpha", Kind: "service_exposure",
			ProviderID: "cloudflare", ProviderAccountID: "acct-work",
		},
		Revision: 7,
		DesiredSpec: ipc.ConnectionSpecDTO{
			Kind: "service_exposure",
			ServiceExposure: &ipc.ServiceExposureSpecDTO{
				Source:     ipc.SourceDTO{Kind: "existing_service", Existing: &ipc.ExistingSourceDTO{Address: "127.0.0.1:3000"}},
				Exposure:   ipc.ExposureDTO{Mode: "permanent_public", RequestedAddress: "app.example.com"},
				Protection: ipc.ProtectionDTO{Kind: "none"},
			},
		},
		Driver:    ipc.DriverSelectionDTO{ProviderID: "cloudflare", AccountID: "acct-work"},
		Lifecycle: ipc.LifecycleDTO{AutoStart: false, OnDisconnect: "keep_alive"},
	}
}

func forwardDetail() *ipc.ConnectionDetailDTO {
	return &ipc.ConnectionDetailDTO{
		Summary: ipc.ConnectionDTO{
			ID: "pf-1", Name: "database", Kind: "port_forward",
			DesiredState: "closed", UserState: "Closed", ProviderID: "portforward",
		},
		Revision: 3,
		Driver:   ipc.DriverSelectionDTO{ProviderID: "portforward"},
		DesiredSpec: ipc.ConnectionSpecDTO{
			Kind: "port_forward",
			PortForward: &ipc.PortForwardDTO{
				LocalPort: 15432, RemoteHost: "db.internal", RemotePort: 5432,
				Protocol: "tcp", Direction: "local",
			},
		},
		Lifecycle: ipc.LifecycleDTO{AutoStart: true, OnDisconnect: "close"},
	}
}

func clientTunnelDetail() *ipc.ConnectionDetailDTO {
	return &ipc.ConnectionDetailDTO{
		Summary: ipc.ConnectionDTO{
			ID: "ct-1", Name: "secure mcp", Kind: "client_tunnel",
			DesiredState: "closed", UserState: "Closed", ProviderID: "client_tunnel",
		},
		Revision: 9,
		Driver:   ipc.DriverSelectionDTO{ProviderID: "client_tunnel"},
		DesiredSpec: ipc.ConnectionSpecDTO{
			Kind: "client_tunnel",
			ClientTunnel: &ipc.ClientTunnelSpecDTO{
				Client:   "openai_secure_mcp_tunnel",
				TunnelID: "tunnel_0123456789abcdef0123456789abcdef",
				// A persisted legacy value must remain readable without being
				// presented or re-sent by a normal edit.
				Profile: "legacy-native",
				MCP: ipc.MCPSourceDTO{
					Transport: "http", Endpoint: "http://127.0.0.1:9000/mcp",
				},
			},
		},
		Lifecycle: ipc.LifecycleDTO{OnDisconnect: "keep_alive"},
	}
}

func editingModel(t *testing.T, client *fakeClient, detail *ipc.ConnectionDetailDTO) Model {
	t.Helper()
	m := readyModel(client, twoConnectionSnapshot())
	m.selectedID = detail.Summary.ID
	next, _ := m.beginEdit(detail.Summary.ID)
	m = next
	next2, _ := m.Update(connectionDetailMsg{ConnectionID: detail.Summary.ID, Detail: detail})
	return next2.(Model)
}

// TestAConnectionCanBeEdited pins audit finding 11.
//
// The supervisor could edit a connection and preview the edit as a plan — a
// route, a handler, a delta description and a client method, all working and
// called by nothing. A connection created with the wrong hostname could only be
// deleted and made again.
func TestAConnectionCanBeEdited(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	if m.screen != ScreenEdit {
		t.Fatalf("screen = %q, want the edit screen", m.screen)
	}

	// Change the hostname. The cursor is moved to it by identity rather than by a
	// fixed number of presses: the row order is the connection kind's own, and a
	// test that counts keystrokes breaks whenever a kind gains a property.
	var next tea.Model
	for i, row := range m.edit.rows() {
		if row.field == editHostname {
			m.edit.cursor = i
		}
	}
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	if !m.edit.typing {
		t.Fatal("selecting the hostname did not open an editor")
	}
	m.edit.field.SetValue("new.example.com")
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	if !m.edit.dirty() {
		t.Fatal("the edit was not recorded")
	}
	view := m.renderEdit()
	if !strings.Contains(view, "app.example.com → new.example.com") {
		t.Fatalf("the screen does not show what would change:\n%s", view)
	}

	next, cmd := m.Update(keyMsg("p"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("preview did not ask the supervisor what the change would do")
	}
	msg := cmd()

	if client.editRequest == nil {
		t.Fatal("no edit request was sent")
	}
	if client.editRequest.Spec == nil || client.editRequest.Spec.Exposure.RequestedAddress != "new.example.com" {
		t.Fatalf("the request does not carry the new hostname: %#v", client.editRequest.Spec)
	}

	next, _ = m.Update(msg)
	m = next.(Model)
	if m.screen != ScreenPlanPreview {
		t.Fatalf("an edit plan did not reach the preview: screen = %q", m.screen)
	}
}

// TestAnEditCarriesTheRevisionItWasBuiltFrom pins optimistic concurrency.
//
// Without it, an edit computed from a stale view silently overwrites a change
// made elsewhere — the supervisor supports the check and it has to be used.
func TestAnEditCarriesTheRevisionItWasBuiltFrom(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	name := "renamed"
	m.edit.name = &name
	_, cmd := m.Update(keyMsg("p"))
	cmd()

	if client.editRequest == nil {
		t.Fatal("no request was sent")
	}
	if client.editRequest.ExpectedRevision != 7 {
		t.Fatalf("ExpectedRevision = %d, want 7", client.editRequest.ExpectedRevision)
	}
}

// TestAnUntouchedFieldIsNotResubmitted pins that an edit sends only what
// changed. Sending every field back would make each edit a full overwrite.
func TestAnUntouchedFieldIsNotResubmitted(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	name := "renamed"
	m.edit.name = &name
	_, cmd := m.Update(keyMsg("p"))
	cmd()

	req := client.editRequest
	if req.Name == nil || *req.Name != "renamed" {
		t.Fatalf("the name change was not sent: %#v", req.Name)
	}
	if req.Spec != nil {
		t.Fatalf("an untouched spec was resubmitted: %#v", req.Spec)
	}
	if req.Driver != nil {
		t.Fatalf("an untouched driver was resubmitted: %#v", req.Driver)
	}
	if req.Lifecycle != nil {
		t.Fatalf("an untouched lifecycle was resubmitted: %#v", req.Lifecycle)
	}
}

// TestAForwardIsNotOfferedFieldsItCannotHave pins that the edit screen is
// kind-aware: a port forward has no hostname and no access policy, and is told
// so rather than offered a field its spec has nowhere to put.
func TestAForwardIsNotOfferedFieldsItCannotHave(t *testing.T) {
	m := editingModel(t, &fakeClient{}, forwardDetail())

	for _, row := range m.edit.rows() {
		switch row.field {
		case editHostname, editProtection:
			if row.editable {
				t.Errorf("a port forward offers %s", row.label)
			}
			if row.reason == "" {
				t.Errorf("%s is refused without saying why", row.label)
			}
		case editName, editAutoStart:
			if !row.editable {
				t.Errorf("a port forward cannot change its %s", row.label)
			}
		}
	}
}

// TestATemporaryAddressIsNotPresentedAsEditable pins that a hostname the
// provider assigns is not offered as something the user can set.
func TestATemporaryAddressIsNotPresentedAsEditable(t *testing.T) {
	detail := exposedDetail()
	detail.DesiredSpec.ServiceExposure.Exposure.Mode = "temporary_public"
	detail.DesiredSpec.ServiceExposure.Exposure.RequestedAddress = ""
	m := editingModel(t, &fakeClient{}, detail)

	for _, row := range m.edit.rows() {
		if row.field == editHostname && row.editable {
			t.Fatal("a temporary address is offered as editable")
		}
	}
}

// TestARefusedEditStaysOnTheScreenThatMadeIt pins that the values the
// supervisor objected to are still visible and still editable.
func TestARefusedEditStaysOnTheScreenThatMadeIt(t *testing.T) {
	client := &fakeClient{editErr: errors.New("connection was modified by someone else")}
	m := editingModel(t, client, exposedDetail())

	name := "renamed"
	m.edit.name = &name
	next, cmd := m.Update(keyMsg("p"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)

	if m.screen != ScreenEdit {
		t.Fatalf("a refused edit left the screen: %q", m.screen)
	}
	if m.edit == nil {
		t.Fatal("a refused edit discarded the changes")
	}
	if !strings.Contains(m.renderEdit(), "modified by someone else") {
		t.Fatalf("the refusal is not shown:\n%s", m.renderEdit())
	}
}

// TestAnEditThatChangesNothingIsNotSent pins that previewing with no changes
// does not ask the supervisor to plan a no-op.
func TestAnEditThatChangesNothingIsNotSent(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	_, cmd := m.Update(keyMsg("p"))
	if cmd != nil {
		t.Fatal("an empty edit was sent to the supervisor")
	}
}

// TestALateEditPlanDoesNotOpenAPreview pins the correlation rule here too.
func TestALateEditPlanDoesNotOpenAPreview(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	name := "renamed"
	m.edit.name = &name

	next, cmd := m.Update(keyMsg("p"))
	m = next.(Model)
	msg := cmd()

	// The user leaves before the answer arrives. The edit is dirty, so escape
	// asks before discarding; confirming is what actually abandons it.
	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)
	if m.edit == nil || !m.edit.confirmingDiscard {
		t.Fatal("leaving a dirty edit did not ask before discarding it")
	}
	next, _ = m.Update(keyMsg("y"))
	m = next.(Model)
	if m.edit != nil {
		t.Fatal("confirming the discard did not abandon the edit")
	}

	next, _ = m.Update(msg)
	m = next.(Model)
	if m.screen == ScreenPlanPreview {
		t.Fatal("an abandoned edit opened a plan preview")
	}
}

// TestTheEditScreenTakesTypedText pins that the field receives characters
// rather than the screen treating them as commands.
func TestTheEditScreenTakesTypedText(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())

	// Open the name editor and type. "p" would otherwise mean preview.
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	for _, r := range "pjk" {
		next, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(Model)
	}

	if got := m.edit.field.Value(); !strings.HasSuffix(got, "pjk") {
		t.Fatalf("typed text did not reach the field: %q", got)
	}
	if m.screen != ScreenEdit {
		t.Fatalf("typing navigated away: %q", m.screen)
	}
}

// --------------- clone ---------------

func cloningModel(t *testing.T, client *fakeClient, detail *ipc.ConnectionDetailDTO) Model {
	t.Helper()
	m := readyModel(client, twoConnectionSnapshot())
	m.selectedID = detail.Summary.ID
	next, _ := m.beginClone(detail.Summary.ID, detail.Summary.Name)
	m = next
	next2, _ := m.Update(connectionDetailMsg{ConnectionID: detail.Summary.ID, Detail: detail})
	return next2.(Model)
}

// TestAConnectionCanBeCopied pins the other half of audit finding 11. Making a
// second connection like an existing one meant walking the whole wizard again
// and retyping every answer.
func TestAConnectionCanBeCopied(t *testing.T) {
	client := &fakeClient{}
	m := cloningModel(t, client, exposedDetail())

	if m.screen != ScreenClone {
		t.Fatalf("screen = %q, want the copy screen", m.screen)
	}
	if got := m.clone.nameField.Value(); got == "alpha" {
		t.Fatal("the copy is offered the original's name, which cannot be used")
	}

	m.clone.hostField.SetValue("copy.example.com")
	next, cmd := m.Update(keyMsg("enter"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter did not create the copy")
	}
	msg := cmd()

	if client.cloneSource != "conn-a" {
		t.Fatalf("the copy was made from %q", client.cloneSource)
	}
	if client.cloneRequest == nil || client.cloneRequest.Name == "" {
		t.Fatalf("the copy carries no name: %#v", client.cloneRequest)
	}

	next, _ = m.Update(msg)
	m = next.(Model)
	if m.screen == ScreenClone {
		t.Fatal("the copy screen stayed open after creating")
	}
}

// TestTheCopyIsMadeBySupervisorNotRebuiltFromTheDetail pins the correction an
// adversarial review forced.
//
// The first version of this screen built a create request out of the detail
// DTO. That DTO omits a command's environment on purpose, and has no field at
// all for an existing service's health check, so the copy would have started
// without them. The supervisor already had a lossless clone that deep-copies
// the profile; this must use it.
func TestTheCopyIsMadeBySupervisorNotRebuiltFromTheDetail(t *testing.T) {
	client := &fakeClient{}
	m := cloningModel(t, client, exposedDetail())
	m.clone.hostField.SetValue("copy.example.com")

	_, cmd := m.Update(keyMsg("enter"))
	cmd()

	if client.createRequest != nil {
		t.Fatal("the copy was rebuilt from the detail rather than deep-copied by the supervisor")
	}
	if client.cloneRequest == nil {
		t.Fatal("the supervisor's clone was not used")
	}
}

// TestACopyOfAProtectedConnectionIsPossible pins a defect review found: the
// first version downgraded a permanent address to a temporary one while copying
// the access policy — and core validation requires a protected connection to
// have a stable address. Every protected connection was therefore uncopyable,
// on a screen that advertised copying protection.
func TestACopyOfAProtectedConnectionIsPossible(t *testing.T) {
	detail := exposedDetail()
	detail.DesiredSpec.ServiceExposure.Protection = ipc.ProtectionDTO{
		Kind: "email_otp", AllowedEmails: []string{"alice@example.com"},
	}
	client := &fakeClient{}
	m := cloningModel(t, client, detail)

	if !m.clone.needsHostname {
		t.Fatal("a permanent hostname was not asked for")
	}
	m.clone.hostField.SetValue("copy.example.com")

	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("a protected connection could not be copied")
	}
	cmd()

	if client.cloneRequest.RequestedAddress != "copy.example.com" {
		t.Fatalf("the copy did not carry its own hostname: %#v", client.cloneRequest)
	}
}

// TestACopyIsAskedForItsOwnHostname pins that the address is requested rather
// than silently changed, since two connections cannot share one.
func TestACopyIsAskedForItsOwnHostname(t *testing.T) {
	m := cloningModel(t, &fakeClient{}, exposedDetail())

	view := m.renderClone()
	if !strings.Contains(view, "Hostname for the copy") {
		t.Fatalf("the copy is not asked for its own hostname:\n%s", view)
	}
	if !strings.Contains(view, "app.example.com") {
		t.Fatalf("the screen does not say which hostname the original holds:\n%s", view)
	}

	// Leaving it empty is refused rather than sent.
	_, cmd := m.Update(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("a copy with no hostname was sent")
	}
}

// TestATemporaryConnectionIsNotAskedForAHostname pins that the question is only
// asked when it applies.
func TestATemporaryConnectionIsNotAskedForAHostname(t *testing.T) {
	detail := exposedDetail()
	detail.DesiredSpec.ServiceExposure.Exposure.Mode = "temporary_public"
	detail.DesiredSpec.ServiceExposure.Exposure.RequestedAddress = ""
	m := cloningModel(t, &fakeClient{}, detail)

	if m.clone.needsHostname {
		t.Fatal("a temporary connection was asked for a hostname")
	}
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("a temporary connection could not be copied")
	}
}

// TestAForwardCanBeCopied pins that copying is not limited to published
// services. The supervisor deep-copies whatever the profile holds.
func TestAForwardCanBeCopied(t *testing.T) {
	client := &fakeClient{}
	m := cloningModel(t, client, forwardDetail())

	if m.clone.needsHostname {
		t.Fatal("a port forward was asked for a hostname")
	}
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("a port forward could not be copied")
	}
	cmd()
	if client.cloneSource != "pf-1" {
		t.Fatalf("the copy was made from %q", client.cloneSource)
	}
}

// TestACopyIsCreatedClosed pins that copying does not open anything.
func TestACopyIsCreatedClosed(t *testing.T) {
	m := cloningModel(t, &fakeClient{}, exposedDetail())
	if !strings.Contains(m.renderClone(), "created closed") {
		t.Fatalf("the screen does not say the copy is created closed:\n%s", m.renderClone())
	}
}

// TestEditAndCopyAreDiscoverable pins that the actions are findable. An action
// nobody can find is not reachable, which is the state edit was already in.
// TestEditAndCopyAreDiscoverable pins that the two operations reachable from
// nowhere are now offered where the connection is.
//
// The supervisor could edit and copy a connection and no screen asked. Both the
// footer and the help are generated from the screen's action set, so this checks
// the one description rather than two hand-written strings that could drift.
func TestEditAndCopyAreDiscoverable(t *testing.T) {
	m := readyModel(&fakeClient{}, twoConnectionSnapshot())
	m.screen = ScreenHome
	m.selectedID = "conn-a"

	actions := m.actionsFor(ScreenHome)
	for _, id := range []ActionID{ActionEdit, ActionCopy} {
		action, ok := actions.Find(id)
		if !ok {
			t.Fatalf("the home screen does not offer %s at all", id)
		}
		if !action.Enabled {
			t.Errorf("%s is offered but disabled with a connection selected: %s",
				id, action.DisabledReason)
		}
		if action.Label == "" {
			t.Errorf("%s is offered with no label, so nothing can advertise it", id)
		}
	}

	// The footer advertises them.
	home := m.View().Content
	for _, hint := range []string{"Edit", "Copy"} {
		if !strings.Contains(home, hint) {
			t.Errorf("the home screen does not offer %q:\n%s", hint, home)
		}
	}

	// So does the contextual help, from the same list.
	m.prevScreen = ScreenHome
	m.screen = ScreenHelp
	help := m.renderHelp()
	for _, id := range []ActionID{ActionEdit, ActionCopy} {
		action, _ := actions.Find(id)
		if !strings.Contains(help, action.Help) {
			t.Errorf("help does not explain %s:\n%s", id, help)
		}
	}
}

// TestAbandoningThePreviewReturnsToTheEdit pins a defect review found: the edit
// was discarded the moment its plan was previewed, and the edit screen stayed
// on the navigation stack. Pressing escape on the preview returned to a screen
// with nothing on it, no keys, and no way to recover the changes.
func TestAbandoningThePreviewReturnsToTheEdit(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	name := "renamed"
	m.edit.name = &name
	next, cmd := m.Update(keyMsg("p"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.screen != ScreenPlanPreview {
		t.Fatalf("the preview did not open: %q", m.screen)
	}

	next, _ = m.Update(keyMsg("esc"))
	m = next.(Model)

	if m.screen != ScreenEdit {
		t.Fatalf("escaping the preview went to %q, want the edit screen", m.screen)
	}
	if m.edit == nil {
		t.Fatal("escaping the preview discarded the edit")
	}
	if !m.edit.dirty() {
		t.Fatal("the changes were lost")
	}
	view := m.renderEdit()
	if strings.Contains(view, "Loading the connection") {
		t.Fatalf("the edit screen came back empty:\n%s", view)
	}
	if !strings.Contains(view, "renamed") {
		t.Fatalf("the pending change is not shown:\n%s", view)
	}
}

// TestTurningProtectionOnAsksWhoCanSignIn pins the false affordance a review
// found.
//
// The screen cycled protection to email_otp and offered no way to name anyone.
// That builds a policy with no identities, which core validation refuses — so
// the interface offered an operation that could not succeed, and the refusal
// arrived from the server after a round trip.
func TestTurningProtectionOnAsksWhoCanSignIn(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	// Cycle protection from none to email_otp.
	rows := m.edit.rows()
	var protectionRow editRow
	for _, row := range rows {
		if row.field == editProtection {
			protectionRow = row
		}
	}
	m = m.beginEditingField(protectionRow)

	// A field for the identities now exists.
	var hasRules bool
	for _, row := range m.edit.rows() {
		if row.field == editProtectionRules {
			hasRules = true
			if !row.editable {
				t.Error("the identities cannot be edited")
			}
		}
	}
	if !hasRules {
		t.Fatal("turning protection on does not ask who can sign in")
	}

	// Previewing without naming anyone is refused here, not by the server.
	next, cmd := m.Update(keyMsg("p"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("a policy naming nobody was sent to the supervisor")
	}
	if !strings.Contains(m.renderEdit(), "at least one person or domain") {
		t.Fatalf("the screen does not say what is missing:\n%s", m.renderEdit())
	}
}

// TestNamedIdentitiesReachTheRequest pins that the identities are parsed with
// the same authority the wizard uses and sent as structure.
func TestNamedIdentitiesReachTheRequest(t *testing.T) {
	client := &fakeClient{}
	m := editingModel(t, client, exposedDetail())

	otp := "email_otp"
	m.edit.protection = &otp
	m.edit.editing = editProtectionRules
	m.commitEditField("alice@example.com, @example.org")

	if m.edit.err != "" {
		t.Fatalf("valid identities were refused: %q", m.edit.err)
	}

	_, cmd := m.Update(keyMsg("p"))
	if cmd == nil {
		t.Fatal("a complete policy was not previewed")
	}
	cmd()

	spec := client.editRequest.Spec
	if spec == nil {
		t.Fatal("the request carries no spec")
	}
	if len(spec.Protection.AllowedEmails) != 1 || spec.Protection.AllowedEmails[0] != "alice@example.com" {
		t.Fatalf("emails = %#v", spec.Protection.AllowedEmails)
	}
	if len(spec.Protection.AllowedDomains) != 1 || spec.Protection.AllowedDomains[0] != "example.org" {
		t.Fatalf("domains = %#v", spec.Protection.AllowedDomains)
	}
}

// TestAMalformedIdentityIsRefusedWhereItIsTyped pins that parsing uses the
// wizard's rules rather than a second set that would drift from them.
func TestAMalformedIdentityIsRefusedWhereItIsTyped(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	otp := "email_otp"
	m.edit.protection = &otp
	m.edit.editing = editProtectionRules

	m.commitEditField("not an address")
	if m.edit.err == "" {
		t.Fatal("a malformed identity was accepted")
	}
	if m.edit.protectionRules != nil {
		t.Fatal("a malformed identity was recorded")
	}
}

// TestTurningProtectionOffClearsTheIdentities pins that a list of people is not
// left stored against a connection that no longer asks anyone to sign in.
func TestTurningProtectionOffClearsTheIdentities(t *testing.T) {
	detail := exposedDetail()
	detail.DesiredSpec.ServiceExposure.Protection = ipc.ProtectionDTO{
		Kind: "email_otp", AllowedEmails: []string{"alice@example.com"},
	}
	client := &fakeClient{}
	m := editingModel(t, client, detail)

	none := "none"
	m.edit.protection = &none
	_, cmd := m.Update(keyMsg("p"))
	if cmd == nil {
		t.Fatal("turning protection off was not previewed")
	}
	cmd()

	spec := client.editRequest.Spec
	if len(spec.Protection.AllowedEmails) != 0 || len(spec.Protection.AllowedDomains) != 0 {
		t.Fatalf("identities survived turning protection off: %#v", spec.Protection)
	}
}

// TestTheAccountIsChosenNotTyped pins the other false affordance.
//
// The account was a free text field. Typing an ID that does not exist saved a
// profile that failed the next time it was opened — and for a closed connection
// the edit plan generates no reopen steps, so nothing consulted the provider
// and nothing refused it at the time.
func TestTheAccountIsChosenNotTyped(t *testing.T) {
	m := editingModel(t, &fakeClient{}, exposedDetail())
	// The provider's accounts are resolved from the snapshot when the detail
	// lands, so the snapshot is installed and the context refreshed before the
	// rows are read.
	m.snapshot = accountSnapshot()
	m.edit.setProviderContext(
		m.providerCapabilities(m.edit.detail.Driver.ProviderID),
		m.providerAccounts(m.edit.detail.Driver.ProviderID))

	var accountRow editRow
	for _, row := range m.edit.rows() {
		if row.field == editAccount {
			accountRow = row
		}
	}
	m = m.beginEditingField(accountRow)

	if m.edit.typing {
		t.Fatal("the account is still a free text field")
	}
	if m.edit.accountID == nil {
		t.Fatal("selecting the account chose nothing")
	}
	// Whatever it chose must be an account the provider actually reports.
	var known bool
	for _, account := range m.usableAccounts() {
		if account.ID == *m.edit.accountID {
			known = true
		}
	}
	if !known {
		t.Fatalf("selected %q, which the provider does not report", *m.edit.accountID)
	}
}
