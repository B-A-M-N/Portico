package tui

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/store"
)

// Vertical tests: a real IPC server, a real client over a real Unix socket, and
// the real TUI model.
//
// Every unit test here proved one side in isolation. The server test proved the
// response carried dependent connections; the model test proved the screen
// showed them when handed a response containing them. Neither exercised the
// transport between, and the transport is where they disagreed: a refusal is a
// non-2xx response, so the client returns an error with a nil response — and
// the model was reading the response. Both sides green, the path broken.

// verticalHandler answers only what these tests exercise. Everything else is
// the package's own no-op handler, so this does not have to implement
// thirty-one methods it does not care about.
type verticalHandler struct {
	ipc.NullHandler
	removeResponse *ipc.RemoveProviderAccountResponse
	removeErr      error
}

func (h *verticalHandler) HandleRemoveProviderAccount(providerID, accountID string) (
	*ipc.RemoveProviderAccountResponse, error) {
	return h.removeResponse, h.removeErr
}

// liveClient starts a real server on a real socket and returns a real client.
func liveClient(t *testing.T, handler ipc.RequestHandler) *ipc.Client {
	t.Helper()

	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	st, err := store.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	server, err := ipc.NewServer(sock, handler, st)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})

	return ipc.NewClient(sock)
}

// TestAccountRemovalRefusalReachesTheScreenWithNames pins the whole path:
// supervisor refusal, HTTP 409, real client, real model, rendered screen.
//
// The unit tests on each side both passed while this was broken. The refusal
// arrives as an error with a nil response, and the model read the response.
func TestAccountRemovalRefusalReachesTheScreenWithNames(t *testing.T) {
	handler := &verticalHandler{
		removeResponse: &ipc.RemoveProviderAccountResponse{
			Removed:              false,
			DependentConnections: []string{"conn-a"},
			Dependencies: []ipc.AccountDependencyDTO{
				{Kind: "connection", ID: "conn-a", Name: "api-staging",
					Explanation: "this connection uses the account and could not open without it"},
				{Kind: "cleanup_item", ID: "tun-7", Name: "tunnel tun-7",
					Explanation: "Portico still has to remove this from the provider"},
			},
		},
		removeErr: errors.New("1 connection(s) still use this account"),
	}
	client := liveClient(t, handler)

	m := readyModel(&fakeClient{}, accountSnapshot())
	m.client = client
	m.transitionTo(ScreenAccountRemoval)
	row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", ProviderName: "Cloudflare", Label: "Work"}
	m.accountRemovalTarget = &row

	cmd := m.removeAccountCmd(row)
	next, _ := m.Update(cmd())
	m = next.(Model)

	view := m.renderAccountRemoval()
	for _, want := range []string{"api-staging", "tunnel tun-7"} {
		if !strings.Contains(view, want) {
			t.Errorf("the refusal does not name %q on screen:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "could not open without it") {
		t.Errorf("the refusal does not explain the consequence:\n%s", view)
	}
	if m.screen != ScreenAccountRemoval {
		t.Fatalf("a refused removal left the screen: %q", m.screen)
	}
}

// TestASuccessfulRemovalReachesTheScreen guards the other direction through the
// same real transport.
func TestASuccessfulRemovalReachesTheScreen(t *testing.T) {
	handler := &verticalHandler{
		removeResponse: &ipc.RemoveProviderAccountResponse{Removed: true},
	}
	client := liveClient(t, handler)

	m := readyModel(&fakeClient{}, accountSnapshot())
	m.client = client
	m.transitionTo(ScreenAccountRemoval)
	row := accountRow{ProviderID: "cloudflare", AccountID: "acct-work", ProviderName: "Cloudflare", Label: "Work"}
	m.accountRemovalTarget = &row

	cmd := m.removeAccountCmd(row)
	next, _ := m.Update(cmd())
	m = next.(Model)

	if m.accountRemovalError != "" {
		t.Fatalf("a successful removal reported an error: %q", m.accountRemovalError)
	}
	if !strings.Contains(m.status, "Removed") {
		t.Fatalf("the removal was not reported: %q", m.status)
	}
}
