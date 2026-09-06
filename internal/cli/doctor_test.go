package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestDoctorResultFeedsTextAndJSONFromOneModel(t *testing.T) {
	result := newDoctorResult()
	result.Summary = "A provider is ready."
	result.addCheck(DoctorCheck{
		ID:      "credential_health",
		Title:   "Stored credentials",
		State:   "problem",
		Summary: "A stored credential cannot be decrypted.",
	})
	result.finalize()

	var textOutput bytes.Buffer
	if err := renderDoctorText(&textOutput, result); err != nil {
		t.Fatalf("render text: %v", err)
	}
	if !strings.Contains(textOutput.String(), "Stored credentials") ||
		!strings.Contains(textOutput.String(), "1 blocker(s)") {
		t.Fatalf("text output does not reflect the result: %q", textOutput.String())
	}

	jsonOutput, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var decoded struct {
		Version  int             `json:"version"`
		Overall  string          `json:"overall"`
		Blockers []DoctorBlocker `json:"blockers"`
		Exit     DoctorExit      `json:"exit"`
	}
	if err := json.Unmarshal(jsonOutput, &decoded); err != nil {
		t.Fatalf("decode result: %v; json=%s", err, jsonOutput)
	}
	if decoded.Version != doctorResultVersion {
		t.Fatalf("version = %d, want %d", decoded.Version, doctorResultVersion)
	}
	if decoded.Overall != "blocked" {
		t.Fatalf("overall = %q, want blocked", decoded.Overall)
	}
	if len(decoded.Blockers) != 1 || decoded.Blockers[0].Classification != "credential_mismatch" {
		t.Fatalf("blockers = %#v, want one credential_mismatch blocker", decoded.Blockers)
	}
	if decoded.Exit.Code == 0 || decoded.Exit.Classification != "failure" {
		t.Fatalf("exit = %#v, want nonzero failure", decoded.Exit)
	}
}

func TestDoctorFailureClassesReturnNonzeroResult(t *testing.T) {
	tests := []struct {
		name           string
		check          DoctorCheck
		classification string
	}{
		{
			name:           "readiness problem",
			check:          DoctorCheck{ID: "providers", State: "problem", Summary: "No provider is usable."},
			classification: "readiness",
		},
		{
			name:           "blocked readiness attention",
			check:          DoctorCheck{ID: "providers", State: "attention", Summary: "No provider is ready."},
			classification: "readiness",
		},
		{
			name:           "unknown",
			check:          DoctorCheck{ID: "events", State: "unknown", Summary: "The check could not run."},
			classification: "unknown",
		},
		{
			name:           "insecure database",
			check:          DoctorCheck{ID: "database", State: "problem", Classification: "insecure_database"},
			classification: "insecure_database",
		},
		{
			name:           "insecure key",
			check:          DoctorCheck{ID: "installation_key:portico-key.bin", State: "problem", Classification: "insecure_key"},
			classification: "insecure_key",
		},
		{
			name:           "stale socket",
			check:          DoctorCheck{ID: "socket", State: "problem", Classification: "stale_socket"},
			classification: "stale_socket",
		},
		{
			name:           "credential mismatch",
			check:          DoctorCheck{ID: "credential_health", State: "problem"},
			classification: "credential_mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newDoctorResult()
			result.addCheck(tt.check)
			result.finalize()
			if result.Exit.Code == 0 || result.Exit.Classification != "failure" {
				t.Fatalf("exit = %#v, want nonzero failure", result.Exit)
			}
			if result.Overall != "blocked" {
				t.Fatalf("overall = %q, want blocked", result.Overall)
			}
			if len(result.Blockers) != 1 || result.Blockers[0].Classification != tt.classification {
				t.Fatalf("blockers = %#v, want classification %q", result.Blockers, tt.classification)
			}
		})
	}
}

func TestDoctorReadinessCarriesCredentialUnknownAndConnectionBlockers(t *testing.T) {
	result := newDoctorResult()
	result.addReadiness(&ipc.ReadinessDTO{
		Summary: "No provider is ready, so no connection can open.",
		Checks: []ipc.HealthCheckDTO{
			{ID: "credential_health", Title: "Stored credentials", State: "problem", Summary: "A credential cannot be decrypted."},
			{ID: "events", Title: "Event history", State: "unknown", Summary: "Event history could not be read."},
		},
		Connections: []ipc.ConnectionReadinessDTO{
			{ID: "conn-1", Name: "demo", Ready: false, Blockers: []string{"provider credential is unavailable"}},
		},
	})
	result.finalize()

	if result.Exit.Code == 0 {
		t.Fatal("readiness blockers produced a successful exit")
	}
	classes := make(map[string]bool)
	for _, blocker := range result.Blockers {
		classes[blocker.Classification] = true
	}
	for _, want := range []string{"credential_mismatch", "unknown", "readiness"} {
		if !classes[want] {
			t.Errorf("missing %q blocker in %#v", want, result.Blockers)
		}
	}
}

func TestDoctorLocalDatabaseAndKeySecurityAreBlockers(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "portico.db")
	key := filepath.Join(dir, "portico-key.bin")
	if err := os.WriteFile(database, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(database, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}

	result := newDoctorResult()
	result.addCheck(doctorFileCheck("database", "Database", database, 0600))
	for _, check := range doctorSecretKeyChecks(dir) {
		result.addCheck(check)
	}
	result.finalize()

	if result.Exit.Code == 0 {
		t.Fatal("insecure database/key files produced a successful exit")
	}
	classes := make(map[string]bool)
	for _, blocker := range result.Blockers {
		classes[blocker.Classification] = true
	}
	for _, want := range []string{"insecure_database", "insecure_key"} {
		if !classes[want] {
			t.Errorf("missing %q blocker in %#v", want, result.Blockers)
		}
	}
}

func TestDoctorReportsStaleLegacyPlaintextCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "flare-cli", "credentials")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("plaintext-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	check := doctorLegacyCredentialCheck()
	if check == nil || check.State != "problem" || check.Classification != "credential_mismatch" {
		t.Fatalf("legacy credential check = %#v, want credential mismatch problem", check)
	}
	if strings.Contains(check.Summary+check.Detail, "plaintext-secret") {
		t.Fatal("doctor included the plaintext credential in its finding")
	}
}

func TestDoctorStaleSocketIsAHighSignalBlocker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portico.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}

	result := newDoctorResult()
	result.addCheck(doctorSocketCheck(path))
	result.finalize()
	if result.Exit.Code == 0 {
		t.Fatal("stale socket produced a successful exit")
	}
	if len(result.Blockers) != 1 || result.Blockers[0].Classification != "stale_socket" {
		t.Fatalf("blockers = %#v, want one stale_socket blocker", result.Blockers)
	}
}

func TestDoctorCommandRendersAndReturnsErrorOnSupervisorFailure(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonMode], func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("HOME", base)
			t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "runtime"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))

			root := NewCLI()
			args := []string{"doctor"}
			if jsonMode {
				args = []string{"--json", "doctor"}
			}
			root.SetArgs(args)
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			err := root.Execute()
			if err == nil {
				t.Fatal("doctor returned success while supervisor was unavailable")
			}
			var doctorErr *DoctorError
			if !errors.As(err, &doctorErr) {
				t.Fatalf("error = %T %v, want DoctorError", err, err)
			}
			if doctorErr.Result.Exit.Code == 0 {
				t.Fatal("DoctorError carried a successful exit classification")
			}

			if jsonMode {
				var decoded DoctorResult
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
					t.Fatalf("doctor JSON: %v; output=%q", err, output.String())
				}
				if decoded.Version != doctorResultVersion || decoded.Overall != "blocked" || len(decoded.Blockers) == 0 {
					t.Fatalf("doctor JSON = %#v, want versioned blocked result", decoded)
				}
			} else if !strings.Contains(output.String(), "Portico Doctor") || !strings.Contains(output.String(), "Supervisor") {
				t.Fatalf("doctor text = %q", output.String())
			}
		})
	}
}
