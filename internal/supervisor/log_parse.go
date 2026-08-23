package supervisor

import (
	"strings"
	"time"
)

// Reading structure out of a connector's own log lines.
//
// Portico tails log files a connector wrote. The lines are that connector's
// format, not Portico's, so a timestamp and a severity exist only when the
// connector put them there.
//
// This recognises the two shapes the connectors Portico runs actually emit —
// cloudflared's RFC3339 prefix with a level, and ngrok's logfmt key-value pairs —
// and leaves everything else alone. It does not stamp lines with the time they
// were read: that is when Portico looked, not when the event happened, and
// presenting it as an event time would fabricate an ordering the user would
// then reason from.

// logLevels are the severities a connector line can carry, lowercased.
var logLevels = map[string]string{
	"trace": "trace", "debug": "debug", "info": "info", "warn": "warn",
	"warning": "warn", "error": "error", "err": "error", "fatal": "fatal",
	"panic": "fatal", "crit": "fatal", "critical": "fatal",
}

// parseLogLine extracts a timestamp and severity from a connector's line, and
// returns the text with a recognised prefix removed so it is not printed twice.
//
// Everything is optional. A line that carries neither is returned unchanged,
// which is the common case for a program that simply writes to stdout.
func parseLogLine(line string) (timestamp, severity, text string) {
	text = line

	// logfmt: ngrok writes t=2026-01-02T15:04:05Z lvl=info msg="..." — so the
	// fields are looked for by name rather than by position.
	if ts, lvl, msg, ok := parseLogfmt(line); ok {
		if msg != "" {
			text = msg
		}
		return ts, lvl, text
	}

	// A leading RFC3339 timestamp, as cloudflared writes.
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", "", text
	}

	// A space-separated timestamp is two fields, so the pair is tried before the
	// single field. Trying only the first field found the date and rejected it,
	// which silently dropped every timestamp in this format.
	if len(fields) > 1 {
		if ts, ok := parseTimestampField(fields[0] + " " + fields[1]); ok {
			rest := strings.TrimSpace(strings.TrimPrefix(
				strings.TrimSpace(strings.TrimPrefix(line, fields[0])), fields[1]))
			severity, rest = takeLeadingLevel(rest)
			return ts, severity, rest
		}
	}

	if ts, ok := parseTimestampField(fields[0]); ok {
		timestamp = ts
		rest := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		// The level, when the next field is one.
		severity, rest = takeLeadingLevel(rest)
		return timestamp, severity, rest
	}

	// A bare bracketed level, with no timestamp: [ERROR] something failed.
	if lvl, ok := logLevels[strings.ToLower(strings.Trim(fields[0], "[]:"))]; ok {
		return "", lvl, strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	}
	return "", "", text
}

// takeLeadingLevel removes a severity from the front of a line, if there is one.
func takeLeadingLevel(rest string) (severity, remainder string) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", rest
	}
	lvl, ok := logLevels[strings.ToLower(strings.Trim(fields[0], "[]:"))]
	if !ok {
		return "", rest
	}
	return lvl, strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
}

// parseLogfmt reads t=, lvl=/level= and msg= out of a logfmt line.
func parseLogfmt(line string) (timestamp, severity, message string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", "", "", false
	}
	var found bool
	for _, field := range fields {
		key, value, isPair := strings.Cut(field, "=")
		if !isPair {
			continue
		}
		value = strings.Trim(value, `"`)
		switch strings.ToLower(key) {
		case "t", "time", "ts":
			if parsed, valid := parseTimestampField(value); valid {
				timestamp = parsed
				found = true
			}
		case "lvl", "level", "severity":
			if lvl, valid := logLevels[strings.ToLower(value)]; valid {
				severity = lvl
				found = true
			}
		case "msg", "message":
			// The message is taken from the raw line rather than the split
			// fields, so a quoted message containing spaces survives.
			message = logfmtValue(line, key)
			found = true
		}
	}
	return timestamp, severity, message, found
}

// logfmtValue pulls one quoted or bare value out of a logfmt line.
func logfmtValue(line, key string) string {
	idx := strings.Index(line, key+"=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+1:]
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			return rest[1 : end+1]
		}
		return strings.Trim(rest, `"`)
	}
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		return rest[:end]
	}
	return rest
}

// parseTimestampField reports whether a field is a timestamp, and normalises it.
//
// The normalised form is RFC3339, so a client does not have to know which format
// the connector used.
func parseTimestampField(field string) (string, bool) {
	field = strings.Trim(field, `"[]`)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, field); err == nil {
			return parsed.UTC().Format(time.RFC3339), true
		}
	}
	return "", false
}
