package supervisor

import (
	"os"
	"strings"
	"testing"
)

// TestLogRedactionRemovesSecretsFromConnectorOutput pins the redaction rule.
// Connector output is provider software Portico does not control, so secrets
// are removed from what is read rather than assumed absent.
func TestLogRedactionRemovesSecretsFromConnectorOutput(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		secret string
	}{
		{
			name:   "openai style key",
			line:   "starting with key sk-abcdefghijklmnopqrstuvwxyz012345",
			secret: "sk-abcdefghijklmnopqrstuvwxyz012345",
		},
		{
			name:   "jwt",
			line:   "tunnel token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			secret: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:   "key equals value",
			line:   `cloudflared --token=aVeryLongSecretTokenValue123`,
			secret: "aVeryLongSecretTokenValue123",
		},
		{
			name:   "authorization header",
			line:   "sending Authorization: Bearer abcdef1234567890xyz",
			secret: "abcdef1234567890xyz",
		},
		{
			name:   "api key colon form",
			line:   "api_key: 9f8e7d6c5b4a39281706",
			secret: "9f8e7d6c5b4a39281706",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactLogLine(tc.line)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("secret survived redaction:\n in: %s\nout: %s", tc.line, got)
			}
			if !strings.Contains(got, "redacted") {
				t.Fatalf("redaction left no marker that something was removed: %s", got)
			}
		})
	}
}

// TestLogRedactionKeepsOrdinaryOutput ensures redaction does not destroy the
// diagnostic value of the log.
func TestLogRedactionKeepsOrdinaryOutput(t *testing.T) {
	for _, line := range []string{
		"INF Connection established connIndex=0 location=lhr01",
		"registered tunnel connection tunnelID=8f3a-1234",
		"failed to connect to origin http://127.0.0.1:3000: connection refused",
	} {
		if got := redactLogLine(line); got != line {
			t.Fatalf("ordinary output was altered:\n in: %s\nout: %s", line, got)
		}
	}
}

// TestTailFileReturnsOnlyTheLastLines pins the bound: a UI tail must never load
// an entire rotating log.
func TestTailFileReturnsOnlyTheLastLines(t *testing.T) {
	path := t.TempDir() + "/connector.out"
	var b strings.Builder
	for i := range 1000 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 3))
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(strings.Repeat(" ", 0)))
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	if err := writeFile(path, b.String()); err != nil {
		t.Fatalf("write: %v", err)
	}

	lines, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(lines))
	}
	if !strings.HasSuffix(lines[len(lines)-1], "999") {
		t.Fatalf("tail did not return the end of the file: %q", lines[len(lines)-1])
	}
}

// TestTailFileTruncatesPathologicalLines ensures one enormous line cannot
// dominate the response.
func TestTailFileTruncatesPathologicalLines(t *testing.T) {
	path := t.TempDir() + "/huge.out"
	if err := writeFile(path, strings.Repeat("a", maxLogLineBytes*3)+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	for _, line := range lines {
		if len(line) > maxLogLineBytes+32 {
			t.Fatalf("line of %d bytes was not truncated", len(line))
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}
