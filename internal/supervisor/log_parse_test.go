package supervisor

import "testing"

// Reading structure out of a connector's own log lines.
//
// Portico tails files a connector wrote, in that connector's format. A timestamp
// and a severity exist only when the connector put them there — and the time
// Portico read a line is not the time the event happened, so stamping lines with
// it would fabricate an ordering the user would then reason from.

func TestParseLogLineRecognisesConnectorFormats(t *testing.T) {
	cases := []struct {
		name          string
		line          string
		wantTimestamp string
		wantSeverity  string
		wantText      string
	}{
		{
			name:          "cloudflared: RFC3339 then level",
			line:          "2026-01-02T15:04:05Z INF Connection registered connIndex=0",
			wantTimestamp: "2026-01-02T15:04:05Z",
			wantSeverity:  "",
			wantText:      "INF Connection registered connIndex=0",
		},
		{
			name:          "RFC3339 then a spelled level",
			line:          "2026-01-02T15:04:05Z error failed to connect to edge",
			wantTimestamp: "2026-01-02T15:04:05Z",
			wantSeverity:  "error",
			wantText:      "failed to connect to edge",
		},
		{
			name:          "ngrok logfmt",
			line:          `t=2026-01-02T15:04:05Z lvl=warn msg="failed to reconnect" err=timeout`,
			wantTimestamp: "2026-01-02T15:04:05Z",
			wantSeverity:  "warn",
			wantText:      "failed to reconnect",
		},
		{
			name:         "a bare bracketed level",
			line:         "[ERROR] the upstream refused the connection",
			wantSeverity: "error",
			wantText:     "the upstream refused the connection",
		},
		{
			name: "a plain line is left alone",
			//nolint:lll // the point is that nothing is extracted from it
			line:     "Server listening on 127.0.0.1:8080",
			wantText: "Server listening on 127.0.0.1:8080",
		},
		{
			name:     "an empty line",
			line:     "",
			wantText: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timestamp, severity, text := parseLogLine(tc.line)
			if timestamp != tc.wantTimestamp {
				t.Errorf("timestamp = %q, want %q", timestamp, tc.wantTimestamp)
			}
			if severity != tc.wantSeverity {
				t.Errorf("severity = %q, want %q", severity, tc.wantSeverity)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

// TestParseLogLineInventsNothing pins that a line carrying no time gets no time.
//
// This is the whole reason the fields are optional. A blank timestamp renders as
// an unadorned line; a fabricated one renders as evidence.
func TestParseLogLineInventsNothing(t *testing.T) {
	for _, line := range []string{
		"Server listening on 127.0.0.1:8080",
		"a line with an= sign but no known keys",
		"12345 not a timestamp",
	} {
		timestamp, severity, _ := parseLogLine(line)
		if timestamp != "" {
			t.Errorf("%q was given the timestamp %q", line, timestamp)
		}
		if severity != "" {
			t.Errorf("%q was given the severity %q", line, severity)
		}
	}
}

// TestParseLogLineNormalisesTheTimestamp pins that a client does not have to know
// which format the connector used.
func TestParseLogLineNormalisesTheTimestamp(t *testing.T) {
	for _, line := range []string{
		"2026-01-02T15:04:05Z something happened",
		"2026-01-02T15:04:05.123456Z something happened",
		"2026-01-02 15:04:05 something happened",
	} {
		timestamp, _, _ := parseLogLine(line)
		if timestamp != "2026-01-02T15:04:05Z" {
			t.Errorf("%q normalised to %q", line, timestamp)
		}
	}
}
