package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// The history, for a person rather than for a debugger.
//
// The operations list led with operation IDs and plan fingerprints — the two
// things a user has least use for — and left the connection's name, what was
// being attempted, and why it failed to be inferred from them. Nothing could be
// filtered, so finding the failure that brought you here meant reading every
// successful open above it.
//
// Identifiers are not removed. They move to the technical detail of the selected
// operation, which is where they are useful: attaching to a bug report.

// operationFilter narrows the history to what the user is looking for.
type operationFilter int

const (
	// filterAll is everything, most recent first.
	filterAll operationFilter = iota
	// filterRunning is work the supervisor has not finished.
	filterRunning
	// filterFailed is what went wrong.
	filterFailed
	// filterConnection is everything Portico did to the selected connection.
	filterConnection
)

// label names a filter in the words a user would use.
func (f operationFilter) label() string {
	switch f {
	case filterRunning:
		return "still running"
	case filterFailed:
		return "failed"
	case filterConnection:
		return "this connection"
	default:
		return "everything"
	}
}

// next cycles to the following filter.
func (f operationFilter) next() operationFilter {
	if f >= filterConnection {
		return filterAll
	}
	return f + 1
}

// cycleFilter advances whichever filter the current screen offers.
func (m Model) cycleFilter() (Model, tea.Cmd, bool) {
	switch m.screen {
	case ScreenOperations:
		m.operationsFilter = m.operationsFilter.next()
		// The cursor is clamped against the newly filtered list, so a narrower
		// filter cannot leave the selection past the end.
		m.opsSelectedIdx = clampIndex(m.opsSelectedIdx, len(m.visibleOperations()))
		return m, m.operationEventsForSelection(), true
	case ScreenInspect:
		m.logsStream = nextLogStream(m.logsStream)
		if conn := m.SelectedConnection(); conn != nil {
			return m, m.connectionLogsCmd(conn.ID), true
		}
		return m, nil, true
	}
	return m, nil, true
}

// nextLogStream cycles the log source filter.
func nextLogStream(current string) string {
	switch current {
	case "":
		return "stdout"
	case "stdout":
		return "stderr"
	default:
		return ""
	}
}

// visibleOperations is the history after the current filter.
func (m *Model) visibleOperations() []ipc.OperationDTO {
	if m.operationsFilter == filterAll {
		return m.operations
	}
	selected := ""
	if conn := m.SelectedConnection(); conn != nil {
		selected = conn.ID
	}
	out := make([]ipc.OperationDTO, 0, len(m.operations))
	for _, op := range m.operations {
		switch m.operationsFilter {
		case filterRunning:
			if !operationFinished(op.State) {
				out = append(out, op)
			}
		case filterFailed:
			if operationFailed(op) {
				out = append(out, op)
			}
		case filterConnection:
			if selected != "" && op.ConnectionID == selected {
				out = append(out, op)
			}
		}
	}
	return out
}

// selectedOperation is the operation under the cursor, after filtering.
func (m *Model) selectedOperation() *ipc.OperationDTO {
	visible := m.visibleOperations()
	if m.opsSelectedIdx < 0 || m.opsSelectedIdx >= len(visible) {
		return nil
	}
	return &visible[m.opsSelectedIdx]
}

// operationFinished reports whether the supervisor is done with an operation.
func operationFinished(state string) bool {
	switch state {
	case "succeeded", "failed", "compensated", "recovery_required", "cancelled":
		return true
	default:
		return false
	}
}

// operationFailed reports whether an operation did not achieve what it was for.
func operationFailed(op ipc.OperationDTO) bool {
	switch op.State {
	case "failed", "compensated", "recovery_required":
		return true
	default:
		return op.Error != ""
	}
}

// operationHeadline names an operation the way a user would describe it: what
// was done, to what, and how it ended.
func (m *Model) operationHeadline(op ipc.OperationDTO) string {
	name := m.connectionName(op.ConnectionID)
	action := operationActionLabel(op.Intent)
	return action + " " + name
}

// operationActionLabel names an intent as an action.
func operationActionLabel(intent string) string {
	switch intent {
	case "open":
		return "Opened"
	case "close":
		return "Closed"
	case "edit":
		return "Changed"
	case "repair":
		return "Repaired"
	case "delete":
		return "Deleted"
	case "create":
		return "Created"
	default:
		if intent == "" {
			return "Worked on"
		}
		return strings.ToUpper(intent[:1]) + intent[1:]
	}
}

// operationOutcome states how an operation ended, in one clause.
func operationOutcome(op ipc.OperationDTO) string {
	switch op.State {
	case "succeeded":
		return "done"
	case "failed":
		return "failed"
	case "compensated":
		return "failed and was rolled back"
	case "recovery_required":
		return "needs attention"
	case "cancelled":
		return "cancelled"
	case "":
		return "unknown"
	default:
		return "in progress"
	}
}

// connectionName maps a connection ID to its name, falling back to a short form
// of the ID when the connection is gone — a deleted connection still has history.
func (m *Model) connectionName(id string) string {
	if id == "" {
		return "Portico"
	}
	for _, conn := range m.snapshot.Connections {
		if conn.ID == id {
			return conn.Name
		}
	}
	return "a deleted connection (" + shortID(id) + ")"
}

// shortID is the leading portion of an identifier, enough to tell two apart.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// operationAge says how long ago an operation started, or how long it has been
// running, in the coarsest useful unit.
func operationAge(op ipc.OperationDTO, now time.Time) string {
	started, err := time.Parse(time.RFC3339, op.StartedAt)
	if err != nil {
		return ""
	}
	end := now
	if op.CompletedAt != "" {
		if completed, err := time.Parse(time.RFC3339, op.CompletedAt); err == nil {
			end = completed
		}
	}
	d := end.Sub(started)
	if d < 0 {
		return ""
	}
	if !operationFinished(op.State) {
		return "running for " + humanDuration(now.Sub(started))
	}
	return "took " + humanDuration(d)
}

// humanDuration renders a duration in one unit.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
