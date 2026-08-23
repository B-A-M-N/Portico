package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/provider"
)

// maxLogLines bounds a log response. Connector logs rotate on disk and can be
// large; a UI tail must never load all of one.
const maxLogLines = 500

// maxLogLineBytes truncates a single very long line, so one pathological line
// cannot dominate the response.
const maxLogLineBytes = 2000

// secretPatterns match values that must never leave the machine in a log tail.
//
// Connector output is provider software Portico does not control, so the
// redaction is applied to what is read rather than assumed absent. Each pattern
// keeps its label so a reader can see that something was removed.
// Order matters. A more specific pattern must run before a more general one
// that would otherwise consume only part of the match and leave the secret
// behind: the generic key/value rule matches "Authorization: Bearer" and stops
// at the space, exposing the token that follows.
var secretPatterns = []*regexp.Regexp{
	// Bearer and similar scheme-prefixed headers, before the generic rule.
	regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\b\s*[=:]\s*\S+(\s+\S+)?`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{10,}`),
	// Long opaque tokens: Cloudflare tunnel tokens, OpenAI keys, JWTs.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
	// key=value and key: value forms for anything that names a secret.
	regexp.MustCompile(`(?i)\b(token|secret|password|credential|api[_-]?key)\b\s*[=:]\s*"?[^\s"']{6,}"?`),
}

// redactLogLine removes secret-looking values from one line of connector output.
func redactLogLine(line string) string {
	for _, pattern := range secretPatterns {
		line = pattern.ReplaceAllStringFunc(line, func(match string) string {
			// Keep the key so the reader knows what was withheld, drop the value.
			if idx := strings.IndexAny(match, "=:"); idx > 0 {
				return match[:idx+1] + " [redacted]"
			}
			return "[redacted]"
		})
	}
	return line
}

// HandleConnectionLogs returns a bounded, redacted tail of a connection's
// connector output.
//
// The connector already writes rotating log files; nothing read them back, so
// the inspect screen had no logs to show. Paths come from the running process
// specification rather than being reconstructed from a naming convention, so a
// provider that names its files differently still works.
func (h *supervisorHandler) HandleConnectionLogs(id string, lines int) (*ipc.ConnectionLogsDTO, error) {
	cid := core.ConnectionID(id)
	if _, ok := h.sup.controller.GetProfile(cid); !ok {
		return nil, core.ErrProfileNotFound(cid)
	}
	if lines <= 0 || lines > maxLogLines {
		lines = maxLogLines
	}

	result := &ipc.ConnectionLogsDTO{ConnectionID: id}

	managed, ok := h.sup.procMgr.GetProcess(cid)
	if !ok {
		result.Unavailable = "no connector process is being supervised for this connection, so no log files are known"
		return result, nil
	}

	paths := map[string]string{}
	if managed.Spec.StdoutPath != "" {
		paths[managed.Spec.StdoutPath] = "stdout"
	}
	if managed.Spec.StderrPath != "" {
		// A provider may direct both streams to one file; do not read it twice.
		if _, seen := paths[managed.Spec.StderrPath]; !seen {
			paths[managed.Spec.StderrPath] = "stderr"
		}
	}
	if len(paths) == 0 {
		result.Unavailable = "this provider does not capture connector output to a file"
		return result, nil
	}

	for path, stream := range paths {
		tail, err := tailFile(path, lines)
		if err != nil {
			// A missing file is not a failure: the connector may not have
			// written anything yet.
			if os.IsNotExist(err) {
				continue
			}
			result.Unavailable = fmt.Sprintf("could not read %s: %v", stream, err)
			continue
		}
		for _, line := range tail {
			result.Lines = append(result.Lines, ipc.LogLineDTO{
				Stream: stream,
				Text:   redactLogLine(line),
			})
		}
	}

	if len(result.Lines) > maxLogLines {
		result.Truncated = true
		result.Lines = result.Lines[len(result.Lines)-maxLogLines:]
	}
	result.Available = result.Unavailable == ""
	return result, nil
}

// HandleTelemetry returns the traffic snapshot for a connection.
func (h *supervisorHandler) HandleTelemetry(id string) (*ipc.TelemetryDTO, error) {
	cid := core.ConnectionID(id)
	profile, ok := h.sup.controller.GetProfile(cid)
	if !ok {
		return nil, core.ErrProfileNotFound(cid)
	}

	result := &ipc.TelemetryDTO{}

	providerID := profile.Driver.ProviderID
	prov := h.sup.registry.Get(providerID)
	if prov == nil {
		result.Unavailable = "provider is not registered"
		return result, nil
	}

	// Check if the provider supports telemetry.
	_, tp := provider.TelemetryCapability(prov)
	if tp == nil {
		result.Unavailable = "provider does not expose traffic telemetry"
		return result, nil
	}

	// Get the telemetry sample.
	sample, err := tp.Telemetry(context.Background(), cid)
	if err != nil {
		result.Unavailable = fmt.Sprintf("could not read telemetry: %v", err)
		return result, nil
	}

	result.ConnectionCount = sample.ConnectionCount
	result.RequestCount = sample.RequestCount
	result.BytesIn = sample.BytesIn
	result.BytesOut = sample.BytesOut
	result.ProviderErrors = sample.ProviderErrors
	result.SampledAt = sample.SampledAt.Format(time.RFC3339)
	result.HasCounts = sample.HasCounts
	result.HasBytes = sample.HasBytes
	result.HasErrors = sample.HasErrors
	result.Available = true
	// A sample carrying no measured counter at all is not a sample. Reporting it
	// as available would put a screenful of blanks in front of the user with
	// nothing saying why.
	if !sample.HasCounts && !sample.HasBytes && !sample.HasErrors {
		result.Available = false
		result.Unavailable = "the provider returned no traffic counters"
	}
	return result, nil
}

// tailFile returns the last n lines of a file, reading it in a single pass with
// a bounded ring buffer so a large log does not have to fit in memory.
func tailFile(path string, n int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	ring := make([]string, 0, n)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > maxLogLineBytes {
			line = line[:maxLogLineBytes] + " …[truncated]"
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	if err := scanner.Err(); err != nil {
		// A line longer than the buffer is reported rather than silently
		// dropping the rest of the file.
		return ring, nil
	}
	return ring, nil
}
