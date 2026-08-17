// Package tunnel provides utilities for working with tunnel subprocesses.
package tunnel

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// quickTunnelHostRe matches a hostname ending in .trycloudflare.com.
// It rejects lookalike domains such as eviltrycloudflare.com by anchoring
// the suffix to a label boundary.
var quickTunnelHostRe = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.trycloudflare\.com$`)

// urlFinderRe finds absolute URLs within arbitrary log output.
var urlFinderRe = regexp.MustCompile(`https?://[^\s]+`)

// ExtractQuickTunnelURL parses a Cloudflare Quick Tunnel URL from cloudflared
// log output. Cloudflare assigns a random subdomain, e.g.
// https://random-words-here.trycloudflare.com, not a path under
// trycloudflare.com. The function extracts absolute URLs from the output,
// validates that one is an HTTPS URL with no userinfo and a hostname ending
// exactly in .trycloudflare.com, and returns it.
//
// Cloudflare documents Quick Tunnels as development/testing only; callers
// must not treat them as production endpoints.
func ExtractQuickTunnelURL(output string) (string, error) {
	candidates := urlFinderRe.FindAllString(output, -1)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no Quick Tunnel URL found in output")
	}

	for _, raw := range candidates {
		// Strip trailing punctuation that may have been captured from
		// surrounding log formatting (quotes, commas, periods, etc.).
		raw = strings.TrimRightFunc(raw, func(r rune) bool {
			return unicode.IsPunct(r) && r != '/' && r != ':' && r != '-' && r != '.'
		})
		// But preserve a single trailing slash as a valid path.
		if strings.HasSuffix(raw, "//") {
			raw = strings.TrimRight(raw, "/") + "/"
		}

		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if !parsed.IsAbs() {
			continue
		}
		if parsed.Scheme != "https" {
			continue
		}
		if parsed.User != nil {
			continue
		}
		host := parsed.Hostname()
		if !quickTunnelHostRe.MatchString(host) {
			continue
		}
		// Reject path tricks: the URL must have no path beyond "/".
		if parsed.Path != "" && parsed.Path != "/" {
			continue
		}
		return parsed.String(), nil
	}

	return "", fmt.Errorf("no Quick Tunnel URL found in output")
}

// ExtractQuickTunnelURLFromLines is a convenience for tests that want to
// validate line-by-line extraction behavior.
func ExtractQuickTunnelURLFromLines(lines []string) (string, error) {
	return ExtractQuickTunnelURL(strings.Join(lines, "\n"))
}
