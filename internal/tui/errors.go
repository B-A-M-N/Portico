package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// UserFacingError is an error presented as an intervention rather than a
// failure report: what happened, why, and what the user can do next. The
// original error text is retained under Technical rather than discarded.
type UserFacingError struct {
	Code        string
	Summary     string
	Explanation string
	NextActions []string
	Technical   string
	Retryable   bool
}

// Error implements the error interface so a UserFacingError can be returned
// where an error is expected.
func (e UserFacingError) Error() string { return e.Summary }

// String renders the error for display: a summary, an explanation, and the
// concrete next actions. Technical detail is deliberately excluded; it belongs
// under a technical-details view, not in the primary message.
func (e UserFacingError) String() string {
	var b strings.Builder
	b.WriteString(e.Summary)
	if e.Explanation != "" {
		b.WriteString("\n\n")
		b.WriteString(e.Explanation)
	}
	if len(e.NextActions) > 0 {
		b.WriteString("\n")
		for _, action := range e.NextActions {
			b.WriteString("\n  • ")
			b.WriteString(action)
		}
	}
	return b.String()
}

// describeError maps any error into a user-facing intervention.
//
// The TUI previously rendered raw err.Error() text, so a user saw strings like
// "provider account unavailable" or "connection refused" with no indication of
// what to do. The supervisor already returns structured errors carrying an
// explanation and recovery actions; this uses them when present and falls back
// to recognising the common transport failures when not.
func describeError(err error) UserFacingError {
	if err == nil {
		return UserFacingError{}
	}

	// An error the supervisor described structurally is already actionable.
	var apiErr *ipc.APIStatusError
	if errors.As(err, &apiErr) {
		ufe := UserFacingError{
			Code:        apiErr.Code,
			Summary:     apiErr.Message,
			Explanation: apiErr.Explanation,
			Technical:   apiErr.TechnicalDetails,
			Retryable:   apiErr.Retryable,
		}
		if ufe.Summary == "" {
			ufe.Summary = "The supervisor rejected the request."
		}
		if ufe.Technical == "" {
			ufe.Technical = err.Error()
		}
		for _, action := range apiErr.RecoveryActions {
			if action.Label != "" {
				ufe.NextActions = append(ufe.NextActions, action.Label)
			}
		}
		if len(ufe.NextActions) == 0 && ufe.Retryable {
			ufe.NextActions = append(ufe.NextActions, "Try the operation again")
		}
		return ufe
	}

	technical := err.Error()

	switch {
	case errors.Is(err, context.Canceled):
		return UserFacingError{
			Code:        "PTO-UI-CANCELLED",
			Summary:     "The operation was cancelled.",
			Explanation: "Portico stopped waiting for this request. Nothing was changed by the cancellation itself.",
			Technical:   technical,
		}
	case errors.Is(err, context.DeadlineExceeded):
		return UserFacingError{
			Code:        "PTO-UI-TIMEOUT",
			Summary:     "The supervisor did not respond in time.",
			Explanation: "The request was sent but no reply arrived before the timeout. The work may still be in progress.",
			NextActions: []string{"Check the operations screen to see whether it completed", "Try again"},
			Technical:   technical,
			Retryable:   true,
		}
	}

	// A dial failure to the supervisor socket is the most common startup
	// problem and has a specific remedy.
	var opErr *net.OpError
	if errors.As(err, &opErr) || strings.Contains(technical, "no such file or directory") ||
		strings.Contains(technical, "connect: connection refused") {
		if strings.Contains(technical, ".sock") || strings.Contains(technical, "supervisor") {
			return UserFacingError{
				Code:        "PTO-UI-NO-SUPERVISOR",
				Summary:     "Portico could not reach the supervisor.",
				Explanation: "The supervisor process is not running, or its socket is not accessible to this user.",
				NextActions: []string{"Start it with: portico supervisor run", "Check that the socket path is readable"},
				Technical:   technical,
				Retryable:   true,
			}
		}
		return UserFacingError{
			Code:        "PTO-UI-UNREACHABLE",
			Summary:     "Portico could not reach the service.",
			Explanation: "The address refused the connection or does not exist.",
			NextActions: []string{"Start the service", "Check the address", "Run the connection test again"},
			Technical:   technical,
			Retryable:   true,
		}
	}

	return UserFacingError{
		Code:      "PTO-UI-UNEXPECTED",
		Summary:   firstLine(technical),
		Technical: technical,
	}
}

// statusLine renders an error as a single line, for the status bar.
func statusLine(prefix string, err error) string {
	ufe := describeError(err)
	if prefix == "" {
		return ufe.Summary
	}
	return fmt.Sprintf("%s: %s", prefix, ufe.Summary)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
