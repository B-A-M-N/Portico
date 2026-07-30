package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// TestStructuredSupervisorErrorsBecomeInterventions pins audit item 19. The
// supervisor already returned an explanation and recovery actions; the client
// flattened them into a single string and the TUI rendered raw err.Error().
func TestStructuredSupervisorErrorsBecomeInterventions(t *testing.T) {
	err := &ipc.APIStatusError{
		Status:      409,
		Code:        "PTO-CORE-010",
		Message:     "This connection is assigned to a Cloudflare account that is not currently loaded.",
		Explanation: "The supervisor loaded no credentials for that account at startup.",
		RecoveryActions: []ipc.RecoveryAction{
			{Label: "Restart the Portico supervisor"},
			{Label: "Choose another account"},
		},
		TechnicalDetails: "provider account unavailable: provider=cloudflare account=acct-1",
	}

	ufe := describeError(err)
	if ufe.Code != "PTO-CORE-010" {
		t.Fatalf("code = %q", ufe.Code)
	}
	if !strings.Contains(ufe.Summary, "not currently loaded") {
		t.Fatalf("summary = %q", ufe.Summary)
	}
	if ufe.Explanation == "" {
		t.Fatal("explanation was discarded")
	}
	if len(ufe.NextActions) != 2 {
		t.Fatalf("next actions = %#v, want 2", ufe.NextActions)
	}
	// The raw text must remain available, but must not be the headline.
	if !strings.Contains(ufe.Technical, "provider account unavailable") {
		t.Fatalf("technical detail was lost: %q", ufe.Technical)
	}
	rendered := ufe.String()
	if strings.Contains(rendered, "provider account unavailable") {
		t.Fatalf("technical text leaked into the primary message:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Restart the Portico supervisor") {
		t.Fatalf("rendered message omits the next actions:\n%s", rendered)
	}
}

// TestCommonTransportFailuresGetSpecificRemedies ensures the bare errors a user
// is most likely to hit are explained rather than echoed.
func TestCommonTransportFailuresGetSpecificRemedies(t *testing.T) {
	t.Run("supervisor socket missing", func(t *testing.T) {
		ufe := describeError(errors.New("dial unix /run/portico/portico.sock: connect: no such file or directory"))
		if !strings.Contains(ufe.Summary, "supervisor") {
			t.Fatalf("summary = %q", ufe.Summary)
		}
		joined := strings.Join(ufe.NextActions, " | ")
		if !strings.Contains(joined, "portico supervisor run") {
			t.Fatalf("no actionable remedy offered: %q", joined)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		ufe := describeError(context.DeadlineExceeded)
		if !ufe.Retryable {
			t.Fatal("a timeout should be retryable")
		}
		if len(ufe.NextActions) == 0 {
			t.Fatal("a timeout offers no next action")
		}
	})

	t.Run("cancellation is not an error to act on", func(t *testing.T) {
		ufe := describeError(context.Canceled)
		if !strings.Contains(ufe.Explanation, "Nothing was changed") {
			t.Fatalf("cancellation explanation = %q", ufe.Explanation)
		}
	})
}

// TestUnrecognisedErrorsStillCarryTheirText ensures the fallback never loses
// information.
func TestUnrecognisedErrorsStillCarryTheirText(t *testing.T) {
	ufe := describeError(errors.New("something specific went wrong"))
	if ufe.Summary != "something specific went wrong" {
		t.Fatalf("summary = %q", ufe.Summary)
	}
	if ufe.Technical != "something specific went wrong" {
		t.Fatalf("technical = %q", ufe.Technical)
	}
}

// TestStatusLineIsSingleLine ensures the status bar never receives a multi-line
// intervention, which would break the layout.
func TestStatusLineIsSingleLine(t *testing.T) {
	err := &ipc.APIStatusError{
		Status:      500,
		Message:     "Something failed.",
		Explanation: "A long explanation\nspanning lines.",
		RecoveryActions: []ipc.RecoveryAction{
			{Label: "Do the thing"},
		},
	}
	line := statusLine("apply failed", err)
	if strings.Contains(line, "\n") {
		t.Fatalf("status line contains a newline: %q", line)
	}
	if !strings.Contains(line, "apply failed") {
		t.Fatalf("status line lost its prefix: %q", line)
	}
}

// TestDescribeNilError is a guard against formatting a nil error.
func TestDescribeNilError(t *testing.T) {
	if got := describeError(nil); got.Summary != "" {
		t.Fatalf("describeError(nil) = %#v", got)
	}
}
