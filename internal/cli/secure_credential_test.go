package cli

import (
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// secretFlow returns a setup flow with one required secret field whose
// environment variable is guaranteed unset.
func secretFlow(t *testing.T) *ipc.SetupFlowDTO {
	t.Helper()
	t.Setenv("PORTICO_TEST_SECRET_UNSET", "")
	return &ipc.SetupFlowDTO{
		ProviderID: "cloudflare", Kind: "account",
		Fields: []ipc.SetupFieldDTO{
			{ID: "credential", Label: "API token", Secret: true, Required: true,
				EnvVars: []string{"PORTICO_TEST_SECRET_UNSET"}},
		},
	}
}

func newSetupCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("credential-stdin", false, "")
	cmd.Flags().Int("credential-fd", -1, "")
	return cmd
}

// TestASecretCanBeReadFromStdin covers the --credential-stdin path added so a
// non-interactive caller never needs an environment variable or argv.
func TestASecretCanBeReadFromStdin(t *testing.T) {
	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-stdin", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cmd.SetIn(strings.NewReader("  token-from-stdin\n"))

	values, err := collectSetupValues(cmd, secretFlow(t))
	if err != nil {
		t.Fatalf("stdin credential refused: %v", err)
	}
	if values["credential"] != "token-from-stdin" {
		t.Fatalf("credential = %q, want surrounding whitespace trimmed", values["credential"])
	}
}

// TestASecretCanBeReadFromAnInheritedFileDescriptor covers the
// --credential-fd path. A production caller inherits the descriptor from a
// parent process; in-process the pipe read end occupies its own descriptor,
// which the flag accepts by number.
func TestASecretCanBeReadFromAnInheritedFileDescriptor(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("token-from-fd"); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", strconv.Itoa(int(reader.Fd()))); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	values, err := collectSetupValues(cmd, secretFlow(t))
	if err != nil {
		t.Fatalf("descriptor credential refused: %v", err)
	}
	if values["credential"] != "token-from-fd" {
		t.Fatalf("credential = %q", values["credential"])
	}
}

// TestAnAbsentCredentialFDFlagDoesNotBorrowStdin protects callers that use
// collectSetupValues with a command which does not expose the optional
// credential source flags. An unknown pflag integer used to look like FD 0,
// causing this function to read and close the process stdin on every call.
func TestAnAbsentCredentialFDFlagDoesNotBorrowStdin(t *testing.T) {
	cmd := &cobra.Command{}

	for i := 0; i < 3; i++ {
		values, err := collectSetupValues(cmd, secretFlow(t))
		if err == nil {
			t.Fatalf("invocation %d accepted a credential without a source: %#v", i, values)
		}
		if !strings.Contains(err.Error(), "API token") {
			t.Fatalf("invocation %d returned the wrong error: %v", i, err)
		}
	}
}

// TestAnExplicitNegativeCredentialFDIsRejected keeps the sentinel internal:
// -1 means the flag was not supplied, never an explicit descriptor.
func TestAnExplicitNegativeCredentialFDIsRejected(t *testing.T) {
	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", "-1"); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	_, err := collectSetupValues(cmd, secretFlow(t))
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative descriptor was accepted: %v", err)
	}
}

// TestStdinAndDescriptorAreMutuallyExclusive pins that ambiguous sources are
// refused rather than silently prioritised.
func TestStdinAndDescriptorAreMutuallyExclusive(t *testing.T) {
	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-stdin", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := cmd.Flags().Set("credential-fd", "3"); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	_, err := collectSetupValues(cmd, secretFlow(t))
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("both sources accepted: %v", err)
	}
}

// TestAnEmptySecretStillFallsThroughToTheRequiredRefusal pins that stdin
// delivering only whitespace behaves as if no credential was supplied.
func TestAnEmptySecretStillFallsThroughToTheRequiredRefusal(t *testing.T) {
	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-stdin", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cmd.SetIn(strings.NewReader("   \n"))

	_, err := collectSetupValues(cmd, secretFlow(t))
	if err == nil || !strings.Contains(err.Error(), "API token") {
		t.Fatalf("empty stdin silently satisfied the field: %v", err)
	}
}

// replaceFD0ForTest supplies a controlled stdin descriptor without changing
// the os.Stdin pointer. The returned reader shares the pipe's open file
// description with descriptor 0, so callers can verify whether the handler
// consumed input from an absent --credential-fd.
func replaceFD0ForTest(t *testing.T, input string) *os.File {
	t.Helper()

	original, err := syscall.Dup(0)
	if err != nil {
		t.Fatalf("duplicate stdin: %v", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(original)
		t.Fatalf("pipe: %v", err)
	}
	if _, err := writer.WriteString(input); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = syscall.Close(original)
		t.Fatalf("write stdin fixture: %v", err)
	}
	if err := writer.Close(); err != nil {
		_ = reader.Close()
		_ = syscall.Close(original)
		t.Fatalf("close stdin fixture: %v", err)
	}
	if err := syscall.Dup2(int(reader.Fd()), 0); err != nil {
		_ = reader.Close()
		_ = syscall.Close(original)
		t.Fatalf("replace stdin: %v", err)
	}

	t.Cleanup(func() {
		if err := syscall.Dup2(original, 0); err != nil {
			t.Errorf("restore stdin: %v", err)
		}
		if err := syscall.Close(original); err != nil {
			t.Errorf("close saved stdin: %v", err)
		}
		_ = reader.Close()
	})
	return reader
}

func assertDescriptorClosed(t *testing.T, fd int) {
	t.Helper()
	dup, err := syscall.Dup(fd)
	if err == nil {
		_ = syscall.Close(dup)
		t.Fatalf("descriptor %d remained open", fd)
	}
}

// TestAbsentCredentialFDDoesNotClaimFD0AcrossCalls catches both the absent
// flag lookup bug and same-process descriptor leakage. An absent flag must not
// read or close descriptor 0, even when collectSetupValues is called repeatedly
// with the same command.
func TestAbsentCredentialFDDoesNotClaimFD0AcrossCalls(t *testing.T) {
	t.Setenv("PORTICO_TEST_SECRET_UNSET", "")
	reader := replaceFD0ForTest(t, "stdin-must-remain-untouched")
	cmd := &cobra.Command{}

	for i := 0; i < 3; i++ {
		_, err := collectSetupValues(cmd, secretFlow(t))
		if err == nil || !strings.Contains(err.Error(), "API token is required") {
			t.Fatalf("call %d: absent descriptor was treated as explicit: %v", i+1, err)
		}
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read untouched stdin fixture: %v", err)
	}
	if string(data) != "stdin-must-remain-untouched" {
		t.Fatalf("absent descriptor consumed stdin: %q", data)
	}
}

// TestExplicitCredentialFDZeroIsConsumedAndClosed pins ownership for the
// special descriptor number that the absent-flag bug accidentally selected.
func TestExplicitCredentialFDZeroIsConsumedAndClosed(t *testing.T) {
	replaceFD0ForTest(t, "  token-from-fd-zero\n")
	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", "0"); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	values, err := collectSetupValues(cmd, secretFlow(t))
	if err != nil {
		t.Fatalf("explicit descriptor 0 refused: %v", err)
	}
	if values["credential"] != "token-from-fd-zero" {
		t.Fatalf("credential = %q", values["credential"])
	}
	assertDescriptorClosed(t, 0)
}

// TestExplicitCredentialFDNonzeroIsConsumedAndClosed pins the same ownership
// rule for an ordinary inherited descriptor.
func TestExplicitCredentialFDNonzeroIsConsumedAndClosed(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("token-from-fd"); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	fd := int(reader.Fd())
	if fd == 0 {
		t.Fatal("pipe reader unexpectedly occupied descriptor 0")
	}

	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", strconv.Itoa(fd)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	values, err := collectSetupValues(cmd, secretFlow(t))
	if err != nil {
		t.Fatalf("explicit nonzero descriptor refused: %v", err)
	}
	if values["credential"] != "token-from-fd" {
		t.Fatalf("credential = %q", values["credential"])
	}
	assertDescriptorClosed(t, fd)
}

func TestExplicitCredentialFDClosedIsRejected(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	fd := int(reader.Fd())
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", strconv.Itoa(fd)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	_, err = collectSetupValues(cmd, secretFlow(t))
	if err == nil || !strings.Contains(err.Error(), "read API token securely") {
		t.Fatalf("closed descriptor was accepted: %v", err)
	}
}

func TestExplicitCredentialFDThatExceedsLimitIsRejectedAndClosed(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "credential")
	if err != nil {
		t.Fatalf("create credential fixture: %v", err)
	}
	fd := int(file.Fd())
	if _, err := file.WriteString(strings.Repeat("x", maxCredentialBytes+1)); err != nil {
		file.Close()
		t.Fatalf("write oversized credential: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		t.Fatalf("rewind credential fixture: %v", err)
	}

	cmd := newSetupCommand(t)
	if err := cmd.Flags().Set("credential-fd", strconv.Itoa(fd)); err != nil {
		file.Close()
		t.Fatalf("set flag: %v", err)
	}
	_, err = collectSetupValues(cmd, secretFlow(t))
	if err == nil || !strings.Contains(err.Error(), "credential exceeds") {
		t.Fatalf("oversized descriptor was accepted: %v", err)
	}
	assertDescriptorClosed(t, fd)
	_ = file.Close()
}

// TestTheProductionLoginCommandRegistersTheSecureCredentialFlags pins the
// command-tree contract. The handler accepted --credential-stdin and
// --credential-fd, and its error text told users those options existed — but
// the production login command never registered them, so the documented
// automation path failed with "unknown flag". This test walks the real
// NewCLI() tree rather than a substitute Cobra command, so a flag that is not
// registered cannot pass.
func TestTheProductionLoginCommandRegistersTheSecureCredentialFlags(t *testing.T) {
	root := NewCLI()
	login, _, err := root.Find([]string{"provider", "login", "cloudflare"})
	if err != nil {
		t.Fatalf("find provider login in the real command tree: %v", err)
	}
	if login == nil || login.Name() != "login" {
		t.Fatalf("provider login not found in the production command tree")
	}
	for _, flag := range []string{"credential-stdin", "credential-fd"} {
		if login.Flags().Lookup(flag) == nil {
			t.Errorf("production provider login does not register --%s; the handler's own error text advertises it", flag)
		}
	}
}

// TestProviderLoginIsProviderNeutral pins audit item 7: the login command's
// persistent vocabulary must not name one provider inside another provider's
// help. Cloudflare terms hardcoded on the shared command previously meant
// `portico provider login client_tunnel --help` presented Cloudflare
// configuration to someone configuring ChatGPT's tunnel transport.
func TestProviderLoginIsProviderNeutral(t *testing.T) {
	root := NewCLI()
	login, _, err := root.Find([]string{"provider", "login", "client_tunnel"})
	if err != nil {
		t.Fatalf("find provider login: %v", err)
	}
	if login == nil || login.Name() != "login" {
		t.Fatalf("provider login not found in the production command tree")
	}
	help := login.Long + "\n" + login.Short
	login.Flags().VisitAll(func(flag *pflag.Flag) {
		help += "\n" + flag.Name + " " + flag.Usage
	})
	for _, term := range []string{"cloudflare", "Cloudflare", "account ID", "zone ID", "account-id", "zone-id"} {
		if strings.Contains(help, term) {
			t.Errorf("provider login help mentions %q; provider-specific vocabulary must come from each provider's own setup flow", term)
		}
	}
	// The generic surface exists for every provider.
	if login.Flags().Lookup("set") == nil {
		t.Error("provider login does not register the generic --set flag")
	}
}

// TestCollectSetupValuesRejectsUndeclaredAndSecretSets pins --set's contract:
// keys must be declared by the provider, and a secret can never ride the
// command line where shell history and process listings would record it.
func TestCollectSetupValuesRejectsUndeclaredAndSecretSets(t *testing.T) {
	flow := &ipc.SetupFlowDTO{
		ProviderID: "acme",
		Fields: []ipc.SetupFieldDTO{
			{ID: "workspace", Label: "Workspace", Required: true},
			{ID: "token", Label: "Token", Secret: true, Required: true},
		},
	}
	cmd := &cobra.Command{Use: "login"}
	cmd.Flags().StringArray("set", nil, "")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	// An undeclared key is rejected with the known fields named.
	if err := cmd.Flags().Set("set", "region=eu-west"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	_, err := collectSetupValues(cmd, flow)
	if err == nil || !strings.Contains(err.Error(), `no field "region"`) {
		t.Fatalf("undeclared --set = %v, want a named-fields error", err)
	}

	// A declared, non-secret key is accepted (fresh command: --set appends).
	cmd2 := &cobra.Command{Use: "login"}
	cmd2.Flags().StringArray("set", nil, "")
	cmd2.SetOut(io.Discard)
	cmd2.SetErr(io.Discard)
	_ = cmd2.Flags().Set("set", "workspace=ws-1")
	values, err := collectSetupValues(cmd2, flow)
	if err == nil {
		if values["workspace"] != "ws-1" {
			t.Fatalf("values = %#v, want workspace carried", values)
		}
	} else if !strings.Contains(err.Error(), "Token is required") {
		t.Fatalf("collect with declared sets failed unexpectedly: %v", err)
	}

	// A secret key is refused outright.
	cmd3 := &cobra.Command{Use: "login"}
	cmd3.Flags().StringArray("set", nil, "")
	cmd3.SetOut(io.Discard)
	cmd3.SetErr(io.Discard)
	_ = cmd3.Flags().Set("set", "token=secret-value")
	if _, err := collectSetupValues(cmd3, flow); err == nil ||
		!strings.Contains(err.Error(), "--set cannot carry the secret") {
		t.Fatalf("secret --set = %v, want refusal", err)
	}
}
