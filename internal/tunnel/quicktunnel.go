// Package tunnel provides utilities for working with tunnel subprocesses.
package tunnel

import (
	"fmt"
	"net/url"
	"regexp"
)

// quickTunnelURLRe matches the trycloudflare.com URL that cloudflared prints.
var quickTunnelURLRe = regexp.MustCompile(`trycloudflare\.com/[a-zA-Z0-9_-]+`)

// ExtractQuickTunnelURL parses the trycloudflare.com URL from cloudflared output.
// Returns a validated absolute HTTPS URL, or an error if the output does not
// contain a valid Quick Tunnel address.
func ExtractQuickTunnelURL(output string) (string, error) {
	matches := quickTunnelURLRe.FindStringSubmatch(output)
	if len(matches) == 0 {
		return "", fmt.Errorf("no Quick Tunnel URL found in output")
	}
	// The regex returns a bare hostname. Construct a validated absolute URL.
	raw := "https://" + matches[0]
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid Quick Tunnel URL %q: %w", raw, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Quick Tunnel URL %q", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("Quick Tunnel URL must not contain userinfo")
	}
	return parsed.String(), nil
}
