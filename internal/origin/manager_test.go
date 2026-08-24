package origin

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

func TestManager_DirectoryLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/index.html", []byte("portico origin"), 0600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager()
	connectionID := core.ConnectionID("directory-lifecycle")
	source := core.SourceSpec{
		Kind:      core.SourceDirectory,
		Directory: &core.DirectorySpec{Path: root, Mode: core.DirectoryModeRead},
	}
	resolved, err := manager.Plan(connectionID, source)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !resolved.Owned || resolved.URL == "" {
		t.Fatalf("unexpected planned origin: %#v", resolved)
	}
	if err := manager.Start(context.Background(), connectionID, source, resolved.URL); err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := http.Get(resolved.URL)
	if err != nil {
		t.Fatalf("GET owned origin: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "portico origin" {
		t.Fatalf("body = %q", body)
	}
	if err := manager.Stop(context.Background(), connectionID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := http.Get(resolved.URL); err == nil {
		t.Fatal("origin still accepted requests after Stop")
	}
}

func TestManager_CommandLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for command-origin lifecycle test")
	}
	port := reservablePort(t)

	manager := NewManager()
	connectionID := core.ConnectionID("command-lifecycle")
	source := core.SourceSpec{Kind: core.SourceCommand, Command: &core.CommandSpec{
		Executable: "python3", Args: []string{"-m", "http.server", strconv.Itoa(port)}, Port: port,
	}}
	resolved, err := manager.Plan(connectionID, source)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := manager.Start(context.Background(), connectionID, source, resolved.URL); err != nil {
		t.Fatalf("Start: %v", err)
	}
	response, err := http.Get(resolved.URL)
	if err != nil {
		t.Fatalf("GET command origin: %v", err)
	}
	response.Body.Close()
	if err := manager.Stop(context.Background(), connectionID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestBuiltinFileBrowser_DeletesOnlyPermittedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/remove-me.txt", []byte("remove"), 0600); err != nil {
		t.Fatal(err)
	}
	browser, err := NewBuiltinFileBrowser(Config{Path: root, AllowDelete: true})
	if err != nil {
		t.Fatalf("NewBuiltinFileBrowser: %v", err)
	}
	endpoint, err := browser.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer browser.Stop(context.Background())
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

	// Delete with CSRF token in header (required)
	token := browser.CSRFToken("/delete")
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/delete",
		strings.NewReader(url.Values{"path": {"/remove-me.txt"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", token)
	_, err = client.Do(req)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	if _, err := os.Stat(root + "/remove-me.txt"); !os.IsNotExist(err) {
		t.Fatalf("file still exists or stat failed: %v", err)
	}

	// Traversal should be rejected
	travReq, _ := http.NewRequest(http.MethodPost, endpoint+"/delete",
		strings.NewReader(url.Values{"path": {"/../../etc/passwd"}}.Encode()))
	travReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	travReq.Header.Set("X-CSRF-Token", token)
	response, err := client.Do(travReq)
	if err != nil {
		t.Fatalf("traversal request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatalf("traversal status = %d, want a rejected request", response.StatusCode)
	}
}

func TestManager_PlanMCPHTTPDoesNotClaimOwnership(t *testing.T) {
	manager := NewManager()
	resolved, err := manager.Plan("mcp-http", core.SourceSpec{
		Kind: core.SourceMCP,
		MCP:  &core.MCPServiceSpec{Transport: core.MCPTransportStreamable, Endpoint: "https://127.0.0.1:8443/mcp"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if resolved.Owned || resolved.URL != "https://127.0.0.1:8443/mcp" {
		t.Fatalf("unexpected MCP origin: %#v", resolved)
	}
}

func TestManager_RejectsNonHTTPCommandProtocol(t *testing.T) {
	_, err := NewManager().Plan("command", core.SourceSpec{
		Kind:    core.SourceCommand,
		Command: &core.CommandSpec{Executable: "server", Port: 8080, Protocol: core.ProtocolHTTPS},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Plan error = %v, want protocol rejection", err)
	}
}

// reservablePort returns a loopback port for a child process to bind.
//
// Binding to :0 and closing tells you a port was free a moment ago. Between then
// and the child binding it, anything on the machine can take it — including another
// test in the same package, which is why running this test twelve times in a row
// used to fail. When it happens the child exits at once, and the run spends ten
// seconds before reporting it.
//
// A port cannot be handed to another process atomically, so the race cannot be
// closed here. What can be removed is the collision with this suite's own
// concurrent runs: the port comes from a range picked per test name and attempt,
// checked immediately before use, so two tests never draw the same number and a
// port taken by something else is retried rather than handed over.
func reservablePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 40; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		// Claimable a second time, immediately before it is handed over: a port
		// something else has taken in the meantime is discarded here rather than
		// producing a child that exits.
		probe, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		probe.Close()
		return port
	}
	t.Fatal("could not reserve a free loopback port")
	return 0
}
