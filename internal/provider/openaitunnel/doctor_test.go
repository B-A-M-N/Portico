package openaitunnel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// TestVerifyOriginAcceptsGet405WithValidMCPInitialize pins the protocol-aware
// origin check. A legitimate Streamable HTTP server rejects GET with 405 while
// correctly handling an initialize POST; the old generic GET called that
// server broken.
func TestVerifyOriginAcceptsGet405WithValidMCPInitialize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.Header.Get("Content-Type"), "json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	p, _ := testProvider(t)
	step := core.PlanStep{ID: "v", Kind: core.StepVerifyOrigin, Technical: core.TechnicalOperation{
		Parameters: map[string]string{"mcp_server_url": srv.URL},
	}}
	res := p.verifyOrigin(context.Background(), step)
	if !res.Succeeded {
		t.Fatalf("a GET-405 server answering MCP initialize was rejected: %v", res.Error)
	}
}

// TestDoctorPreflightBlocksOnReportedIssues pins item 6: the upstream doctor
// is the preflight authority. A client whose doctor reports problems must stop
// the operation before any process starts.
func TestDoctorPreflightBlocksOnReportedIssues(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tunnel-client")
	writeExecutable(t, script, "#!/bin/sh\nif [ \"$1\" = doctor ]; then echo '{\"issues\":[\"credential rejected: 403\"]}'; exit 0; fi\nexit 0\n")

	p, _ := testProvider(t)
	p.binPath = script

	report, err := p.runDoctorPreflight(context.Background(), "tunnel_0123456789abcdef0123456789abcdef", "")
	if err != nil {
		t.Fatalf("doctor invocation failed: %v", err)
	}
	if len(report.Issues) != 1 || !strings.Contains(report.Issues[0], "403") {
		t.Fatalf("issues = %#v", report.Issues)
	}

	step := core.PlanStep{ID: "v", Kind: core.StepValidateAccount, Technical: core.TechnicalOperation{
		Parameters: map[string]string{"tunnel_id": "tunnel_0123456789abcdef0123456789abcdef"},
	}}
	t.Setenv(CredentialEnvVar, "test-key-0001")
	res := p.validateClient(context.Background(), step)
	if res.Succeeded {
		t.Fatal("validateClient succeeded despite doctor reporting problems")
	}
	if !strings.Contains(res.Error.Error(), "403") {
		t.Fatalf("the refusal does not carry the doctor's reason: %v", res.Error)
	}
}

// TestDoctorOutputIsRedacted pins that a doctor report echoing configuration
// values cannot leak the stored credential into logs or findings.
func TestDoctorOutputIsRedacted(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tunnel-client")
	writeExecutable(t, script, "#!/bin/sh\necho 'configured key sk-live-abcdef1234567890 accepted'; exit 0\n")

	const secret = "sk-live-abcdef1234567890"
	p, _ := testProvider(t)
	p.binPath = script
	p.SetCredential(secret)

	report, err := p.runDoctorPreflight(context.Background(), "tunnel_0123456789abcdef0123456789abcdef", "")
	if err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue, secret) {
			t.Fatalf("the secret leaked into the doctor output: %q", issue)
		}
	}
	_ = redactSecretLike // referenced for clarity; redaction happens in runDoctorPreflight
}

// writeExecutable creates an executable shell stub standing in for the real
// tunnel-client binary.
func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

var _ = errors.New // placeholder to keep errors imported if assertions change
