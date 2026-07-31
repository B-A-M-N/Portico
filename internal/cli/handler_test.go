package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestReopenRotatedLogSwitchesToPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.log")
	if err := os.WriteFile(path, []byte("old log\n"), 0600); err != nil {
		t.Fatalf("write original log: %v", err)
	}
	current, err := os.Open(path)
	if err != nil {
		t.Fatalf("open original log: %v", err)
	}
	defer current.Close()
	if err := os.Rename(path, filepath.Join(dir, "supervisor.log.1")); err != nil {
		t.Fatalf("rotate original log: %v", err)
	}
	if err := os.WriteFile(path, []byte("new log\n"), 0600); err != nil {
		t.Fatalf("write replacement log: %v", err)
	}

	replacement, offset, rotated, err := reopenRotatedLog(path, current, int64(len("old log\n")))
	if err != nil {
		t.Fatalf("reopenRotatedLog: %v", err)
	}
	if !rotated || offset != 0 {
		t.Fatalf("rotation result = rotated:%t offset:%d, want true/0", rotated, offset)
	}
	defer replacement.Close()
	data, err := io.ReadAll(replacement)
	if err != nil {
		t.Fatalf("read replacement log: %v", err)
	}
	if string(data) != "new log\n" {
		t.Fatalf("replacement contents = %q", data)
	}
}

func TestNormalizeExistingAddress(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		port int
		want string
	}{
		{name: "hostname", raw: "localhost", port: 3000, want: "localhost:3000"},
		{name: "existing port", raw: "127.0.0.1:8080", port: 3000, want: "127.0.0.1:8080"},
		{name: "ipv6 literal", raw: "::1", port: 3000, want: "[::1]:3000"},
		{name: "ipv6 port", raw: "[::1]:8080", port: 3000, want: "[::1]:8080"},
		{name: "url", raw: "http://localhost:8080", port: 3000, want: "localhost:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeExistingAddress(tt.raw, tt.port)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got != tt.want {
				t.Fatalf("address = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeExistingAddressRejectsURLPath(t *testing.T) {
	if _, err := normalizeExistingAddress("http://localhost:8080/api", 3000); err == nil {
		t.Fatal("expected URL path to be rejected")
	}
}

// TestSupportExportIsReachable pins audit finding 30.
//
// The export existed on the supervisor, was carefully redacted, and had no
// command. A user asked to attach diagnostics to a bug report had no way to
// produce them.
func TestSupportExportIsReachable(t *testing.T) {
	root := NewCLI()

	support, _, err := root.Find([]string{"support", "export"})
	if err != nil {
		t.Fatalf("the support export command is not registered: %v", err)
	}
	if support.Name() != "export" {
		t.Fatalf("found %q, want the export command", support.Name())
	}
	if support.Flags().Lookup("output") == nil {
		t.Fatal("the export cannot be written to a file")
	}
	// The description has to state what the report does not contain, since that
	// is what a user needs to know before attaching it to a public issue.
	if !strings.Contains(support.Long, "no credentials") {
		t.Fatalf("the command does not say what it redacts: %q", support.Long)
	}
}

// TestRemoveAccountIsReachable pins the account lifecycle command.
func TestRemoveAccountIsReachable(t *testing.T) {
	root := NewCLI()

	remove, _, err := root.Find([]string{"provider", "remove-account"})
	if err != nil {
		t.Fatalf("the remove-account command is not registered: %v", err)
	}
	if remove.Name() != "remove-account" {
		t.Fatalf("found %q, want remove-account", remove.Name())
	}
}

// TestProviderLoginDoesNotRequireAZone pins that the CLI enforces the contract
// the provider declares, not a stricter one of its own.
//
// Cloudflare's setup flow marks the zone optional — it is needed only for DNS
// and custom hostnames — and the TUI accepts a tunnel-only setup. The CLI
// required it, so the same account could be configured in one interface and
// refused in the other.
func TestProviderLoginDoesNotRequireAZone(t *testing.T) {
	flow := &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{
			{ID: "account_id", Label: "Account ID", Required: true,
				EnvVars: []string{"TEST_ACCOUNT_ID"}},
			{ID: "zone_id", Label: "Zone ID", EnvVars: []string{"TEST_ZONE_ID"}},
			{ID: "credential", Label: "API token", Secret: true, Required: true,
				EnvVars: []string{"TEST_TOKEN"}},
		},
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("account-id", "acct-1", "")
	cmd.Flags().String("zone-id", "", "")
	cmd.Flags().String("label", "", "")
	t.Setenv("TEST_TOKEN", "cf-token")

	values, err := collectSetupValues(cmd, flow)
	if err != nil {
		t.Fatalf("a tunnel-only setup was refused: %v", err)
	}
	if values["account_id"] != "acct-1" {
		t.Fatalf("account = %q", values["account_id"])
	}
	if _, present := values["zone_id"]; present {
		t.Fatalf("an unset optional field was submitted: %#v", values)
	}
	if values["credential"] != "cf-token" {
		t.Fatal("the credential was not read from the environment")
	}
}

// TestASecretIsNeverTakenFromAnArgument pins the rule the whole flow rests on:
// a secret passed as an argument is in the shell history and visible in the
// process list to every user on the machine.
func TestASecretIsNeverTakenFromAnArgument(t *testing.T) {
	flow := &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{
			{ID: "credential", Label: "API token", Secret: true, Required: true,
				EnvVars: []string{"TEST_TOKEN_UNSET"}},
		},
	}
	cmd := &cobra.Command{}
	// A flag with the field's name exists and holds a value. It must be ignored.
	cmd.Flags().String("credential", "cf-token-from-argv", "")

	_, err := collectSetupValues(cmd, flow)
	if err == nil {
		t.Fatal("a secret was accepted from a command argument")
	}
	if !strings.Contains(err.Error(), "shell history") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

// TestARequiredFieldIsRefusedByName pins that a missing value says which one
// and how to supply it.
func TestARequiredFieldIsRefusedByName(t *testing.T) {
	flow := &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{
			{ID: "account_id", Label: "Account ID", Required: true,
				EnvVars: []string{"TEST_ACCOUNT_UNSET"}},
		},
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("account-id", "", "")

	_, err := collectSetupValues(cmd, flow)
	if err == nil {
		t.Fatal("a missing required field was accepted")
	}
	if !strings.Contains(err.Error(), "Account ID") || !strings.Contains(err.Error(), "--account-id") {
		t.Fatalf("the refusal does not say what to supply: %v", err)
	}
}

// TestOverwritingAPermissiveFileStillEndsPrivate pins the defect a review
// found, which was verified empirically before fixing.
//
// os.WriteFile's mode applies only when it creates the file. Writing a report
// over an existing world-readable file left it world-readable, so the code
// claimed 0600 in a comment and produced 0644. A redacted report is still a
// description of this machine's services and addresses.
func TestOverwritingAPermissiveFileStillEndsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateFile(path, []byte("new\n"), true); err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the report is mode %v, want 0600: anyone on this machine can read it", perm)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new\n" {
		t.Fatalf("the file holds %q", content)
	}
}

// TestAnExistingReportIsNotSilentlyReplaced pins that a report is not
// overwritten without being asked.
func TestAnExistingReportIsNotSilentlyReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writePrivateFile(path, []byte("new\n"), false)
	if err == nil {
		t.Fatal("an existing report was replaced without being asked")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("the refusal does not say how to proceed: %v", err)
	}

	content, _ := os.ReadFile(path)
	if string(content) != "keep\n" {
		t.Fatal("the existing report was modified anyway")
	}
}

// TestANewReportIsCreatedPrivate pins the create path, where umask would
// otherwise have a say.
func TestANewReportIsCreatedPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.json")
	if err := writePrivateFile(path, []byte("x\n"), false); err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("a new report is mode %v, want 0600", perm)
	}
}

// TestAFailedWriteLeavesNoPartialReport pins that an interrupted write cannot
// leave a half-report that looks complete.
func TestAFailedWriteLeavesNoPartialReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent-subdir", "report.json")

	if err := writePrivateFile(path, []byte("x\n"), false); err == nil {
		t.Fatal("writing into a missing directory succeeded")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".portico-export-") {
			t.Fatalf("a temporary file was left behind: %s", entry.Name())
		}
	}
}
