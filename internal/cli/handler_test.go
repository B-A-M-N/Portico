package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
