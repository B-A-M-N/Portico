package clienttunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// doctorTimeout bounds a single tunnel-client invocation. The client's own
// network checks can hang on a broken control plane; the preflight must fail
// fast enough that setup feels responsive and reconciliation is not blocked.
const doctorTimeout = 30 * time.Second

// requiredDoctorFlags are the command-line surface Portico's launch depends
// on. A client missing any of them cannot be driven correctly, so the
// preflight fails with the specific gap rather than letting a wrong process
// start.
var requiredRunFlags = []string{
	"--control-plane.tunnel-id",
	"--control-plane.api-key",
	"--mcp.server-url",
	"--health.listen-addr",
	"--health.url-file",
}

// clientVersion runs `tunnel-client --version` and returns the trimmed output.
func (p *Provider) clientVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.binPath, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type doctorReport struct {
	Version string   `json:"version,omitempty"`
	Issues  []string `json:"issues,omitempty"`
}

// runDoctorPreflight invokes `tunnel-client doctor --json --explain` with the
// configuration Portico intends to run. The upstream doctor knows the real
// contract — credential acceptance, MCP reachability, health listener — which
// Portico can only approximate from outside. Structured output is parsed when
// present; text output is still accepted because older clients printed only
// prose. Output is returned redacted: it is shown to users and may be
// persisted, and the doctor can echo configuration values.
func (p *Provider) runDoctorPreflight(ctx context.Context, tunnelID, serverURL string) (doctorReport, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()

	args := []string{"doctor", "--json", "--explain",
		"--control-plane.tunnel-id", tunnelID,
	}
	if serverURL != "" {
		args = append(args, "--mcp.server-url", "url="+serverURL)
	}
	out, err := exec.CommandContext(ctx, p.binPath, args...).CombinedOutput()
	text := string(out)
	if err != nil && !doctorProducedOutput(text) {
		return doctorReport{}, fmt.Errorf("%s doctor failed: %v", p.binPath, err)
	}

	report := doctorReport{}
	if jsonErr := json.Unmarshal([]byte(text), &report); jsonErr != nil {
		// Not JSON: fall back to scanning the explanation for the flags we
		// require, which also covers clients whose --json is partial.
		for _, flag := range requiredRunFlags {
			if !strings.Contains(text, flag) && !strings.Contains(text, strings.TrimPrefix(flag, "--")) {
				report.Issues = append(report.Issues, "client does not advertise "+flag)
			}
		}
	} else if report.Version == "" {
		if version, vErr := p.clientVersion(ctx); vErr == nil {
			report.Version = version
		}
	}

	// Redact anything that looks like a credential value before it reaches
	// callers who display or persist the report.
	for i, issue := range report.Issues {
		report.Issues[i] = redactSecretLike(issue, p.credential())
	}
	return report, nil
}

// doctorProducedOutput distinguishes "the client ran and said something" from
// "the binary could not be executed at all".
func doctorProducedOutput(out string) bool {
	return strings.TrimSpace(out) != ""
}

// redactSecretLike replaces occurrences of the secret (and sk- prefixed
// values) in free-form tool output.
func redactSecretLike(text, secret string) string {
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	if idx := strings.Index(text, "sk-"); idx >= 0 {
		end := idx
		for end < len(text) && !isBoundary(text[end]) {
			end++
		}
		text = text[:idx] + "[redacted]" + text[end:]
	}
	return text
}

func isBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', ',', ')', ']':
		return true
	}
	return false
}
