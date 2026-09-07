package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/config"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/keys"
)

const doctorResultVersion = 1

// DoctorResult is the versioned result emitted by the doctor command.
//
// Text and JSON are projections of this value. In particular, exit status is
// derived from Blockers rather than from whichever renderer happened to run.
type DoctorResult struct {
	Version  int             `json:"version"`
	Summary  string          `json:"summary,omitempty"`
	Checks   []DoctorCheck   `json:"checks"`
	Blockers []DoctorBlocker `json:"blockers"`
	Overall  string          `json:"overall"`
	Exit     DoctorExit      `json:"exit"`
}

// DoctorCheck is one observed doctor check. State is one of ok, attention,
// problem, or unknown. Unknown is deliberately not a pass.
type DoctorCheck struct {
	ID             string `json:"id"`
	Title          string `json:"title,omitempty"`
	State          string `json:"state"`
	Classification string `json:"classification,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Detail         string `json:"detail,omitempty"`
	NextAction     string `json:"next_action,omitempty"`
	Technical      string `json:"technical,omitempty"`
}

// DoctorBlocker is the actionable subset of DoctorCheck values, or a blocked
// connection reported by readiness. It is kept separate so clients do not
// have to infer process failure from all informational checks.
type DoctorBlocker struct {
	ID             string `json:"id"`
	Title          string `json:"title,omitempty"`
	State          string `json:"state"`
	Classification string `json:"classification"`
	Summary        string `json:"summary,omitempty"`
	Detail         string `json:"detail,omitempty"`
	NextAction     string `json:"next_action,omitempty"`
	Technical      string `json:"technical,omitempty"`
}

// DoctorExit is the stable process-result classification accompanying a
// DoctorResult. The main command maps the returned DoctorError to a nonzero
// process exit; Code is the doctor validation code represented here.
type DoctorExit struct {
	Code           int    `json:"code"`
	Classification string `json:"classification"`
}

// DoctorError means doctor produced a result with one or more blockers.
// Cause is retained for transport and supervisor failures so callers still
// receive the useful underlying error while the rendered result stays clean.
type DoctorError struct {
	Result DoctorResult
	Cause  error
}

func (e *DoctorError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("doctor failed: %v", e.Cause)
	}
	return fmt.Sprintf("doctor found %d blocker(s)", len(e.Result.Blockers))
}

func (e *DoctorError) Unwrap() error { return e.Cause }

func newDoctorResult() DoctorResult {
	return DoctorResult{
		Version:  doctorResultVersion,
		Checks:   make([]DoctorCheck, 0),
		Blockers: make([]DoctorBlocker, 0),
		Overall:  "ok",
		Exit: DoctorExit{
			Code:           0,
			Classification: "success",
		},
	}
}

func (r *DoctorResult) addCheck(check DoctorCheck) {
	check.State = doctorState(check.State)
	if check.ID == "" {
		check.ID = "unknown"
	}
	if check.Classification == "" {
		check.Classification = doctorCheckClassification(check)
	}
	r.Checks = append(r.Checks, check)
}

func (r *DoctorResult) addReadiness(readiness *ipc.ReadinessDTO) {
	if readiness == nil {
		r.addCheck(DoctorCheck{
			ID:      "readiness",
			Title:   "Readiness",
			State:   "unknown",
			Summary: "The supervisor returned no readiness result.",
		})
		return
	}

	r.Summary = readiness.Summary
	for _, check := range readiness.Checks {
		r.addCheck(doctorCheckFromHealth(check))
	}
	for _, conn := range readiness.Connections {
		if conn.Ready {
			continue
		}
		blockers := strings.Join(conn.Blockers, "; ")
		if blockers == "" {
			blockers = "The supervisor reported that this connection cannot open, but gave no reason."
		}
		title := conn.Name
		if title == "" {
			title = conn.ID
		}
		detail := strings.Join(conn.Blockers, "\n")
		r.addCheck(DoctorCheck{
			ID:             "connection:" + conn.ID,
			Title:          title,
			State:          "problem",
			Classification: "readiness",
			Summary:        blockers,
			Detail:         detail,
		})
	}
	if len(readiness.Checks) == 0 {
		r.addCheck(DoctorCheck{
			ID:      "readiness",
			Title:   "Readiness",
			State:   "unknown",
			Summary: "The supervisor returned no readiness checks.",
		})
	}
}

func (r *DoctorResult) finalize() {
	r.Blockers = r.Blockers[:0]
	attention := false
	for _, check := range r.Checks {
		if check.State == "attention" {
			attention = true
		}
		if !doctorCheckIsBlocker(check) {
			continue
		}
		r.Blockers = append(r.Blockers, DoctorBlocker{
			ID:             check.ID,
			Title:          doctorCheckTitle(check),
			State:          check.State,
			Classification: check.Classification,
			Summary:        check.Summary,
			Detail:         check.Detail,
			NextAction:     check.NextAction,
			Technical:      check.Technical,
		})
	}

	switch {
	case len(r.Blockers) > 0:
		r.Overall = "blocked"
		r.Exit = DoctorExit{Code: 1, Classification: "failure"}
	case attention:
		// Attention is useful context but does not stop Portico working.
		r.Overall = "attention"
		r.Exit = DoctorExit{Code: 0, Classification: "success"}
	default:
		r.Overall = "ok"
		r.Exit = DoctorExit{Code: 0, Classification: "success"}
	}
}

func doctorCheckIsBlocker(check DoctorCheck) bool {
	switch check.State {
	case "ok":
		return false
	case "problem", "unknown":
		return true
	case "attention":
		// The aggregate provider readiness check uses attention when no provider
		// is usable. That is a readiness failure even though attention is
		// nonfatal for an otherwise usable installation.
		return check.ID == "providers"
	default:
		return true
	}
}

func doctorState(state string) string {
	switch state {
	case "ok", "attention", "problem", "unknown":
		return state
	default:
		return "unknown"
	}
}

func doctorCheckClassification(check DoctorCheck) string {
	id := strings.ToLower(check.ID)
	switch {
	case check.State == "unknown":
		return "unknown"
	case strings.Contains(id, "credential"):
		return "credential_mismatch"
	case id == "providers" || strings.HasPrefix(id, "provider:") || id == "readiness" || strings.HasPrefix(id, "connection:"):
		return "readiness"
	case id == "database":
		return "database"
	case strings.Contains(id, "key"):
		if strings.Contains(strings.ToLower(check.Summary), "missing") || strings.Contains(strings.ToLower(check.Summary), "wrong") || strings.Contains(strings.ToLower(check.Summary), "replaced") {
			return "key_mismatch"
		}
		return "encryption_key"
	case strings.Contains(id, "socket"):
		return "socket"
	default:
		return "health"
	}
}

func doctorCheckFromHealth(check ipc.HealthCheckDTO) DoctorCheck {
	return DoctorCheck{
		ID:         check.ID,
		Title:      check.Title,
		State:      check.State,
		Summary:    check.Summary,
		Detail:     check.Detail,
		NextAction: check.NextAction,
		Technical:  check.Technical,
	}
}

func collectDoctorResult(ctx context.Context, launcher *app.Launcher) (DoctorResult, error) {
	paths := launcher.GetPaths()
	result := newDoctorResult()
	result.addCheck(doctorFileCheck("database", "Database", paths.DatabasePath, 0600))
	if check := doctorLegacyCredentialCheck(); check != nil {
		result.addCheck(*check)
	}
	for _, check := range doctorSecretKeyChecks(filepath.Dir(paths.DatabasePath)) {
		result.addCheck(check)
	}

	client := launcher.ConnectToSupervisor()
	if err := client.Health(ctx); err != nil {
		result.addCheck(DoctorCheck{
			ID:         "supervisor",
			Title:      "Supervisor",
			State:      "problem",
			Summary:    "The supervisor is not running.",
			Detail:     err.Error(),
			NextAction: "Start one with: portico supervisor run",
			Technical:  err.Error(),
		})
		result.addCheck(doctorSocketCheck(paths.SocketPath))
		result.finalize()
		return result, fmt.Errorf("supervisor is not running: %w", err)
	}

	snap, err := client.Snapshot(ctx)
	if err != nil {
		result.addCheck(DoctorCheck{
			ID:         "snapshot",
			Title:      "Supervisor snapshot",
			State:      "unknown",
			Summary:    "The supervisor is reachable, but its state could not be read.",
			NextAction: "Inspect the supervisor log, then retry doctor.",
			Technical:  err.Error(),
		})
		result.finalize()
		return result, fmt.Errorf("read supervisor snapshot: %w", err)
	}
	open := 0
	for _, conn := range snap.Connections {
		if conn.RuntimeState == "open" {
			open++
		}
	}
	result.addCheck(DoctorCheck{
		ID:      "supervisor",
		Title:   "Supervisor",
		State:   "ok",
		Summary: fmt.Sprintf("Supervisor reachable (seq: %d).", snap.LastSeq),
	})
	result.addCheck(DoctorCheck{
		ID:      "connections",
		Title:   "Connections",
		State:   "ok",
		Summary: fmt.Sprintf("%d connection(s), of which %d open.", len(snap.Connections), open),
	})

	readiness, err := client.Readiness(ctx)
	if err != nil {
		result.addCheck(DoctorCheck{
			ID:         "readiness",
			Title:      "Readiness",
			State:      "unknown",
			Summary:    "The supervisor's readiness checks could not be read.",
			NextAction: "Inspect the supervisor log, then retry doctor.",
			Technical:  err.Error(),
		})
		result.finalize()
		return result, fmt.Errorf("read supervisor readiness: %w", err)
	}
	result.addReadiness(readiness)
	result.finalize()
	return result, nil
}

// doctorLegacyCredentialCheck keeps a failed one-time plaintext cleanup
// visible. LoadCredential retries cleanup on every initialization, but doctor
// must still tell the operator why a valid encrypted credential is blocked
// from being considered fully migrated.
func doctorLegacyCredentialCheck() *DoctorCheck {
	path := config.LegacyCredentialPath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	check := &DoctorCheck{
		ID:             "legacy_credential",
		Title:          "Legacy plaintext credential",
		State:          "problem",
		Classification: "credential_mismatch",
		Summary:        "A legacy plaintext credential remains on disk.",
		Detail:         "Portico encrypted the credential but could not remove the old plaintext file.",
		NextAction:     "Restrict the legacy directory and rerun doctor so Portico can remove the file.",
		Technical:      path,
	}
	if err != nil {
		check.State = "unknown"
		check.Summary = "Portico could not inspect the legacy credential path."
		check.Detail = "The legacy credential may still contain plaintext secret material."
		check.NextAction = "Restore access to the legacy configuration directory, then rerun doctor."
		check.Technical = err.Error()
		return check
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		check.Summary = "A legacy credential path remains and is not a regular file."
		check.NextAction = "Inspect and remove the unexpected legacy credential path, then rerun doctor."
	}
	return check
}

func handleDoctor(cmd *cobra.Command) error {
	result, cause := collectDoctorResult(cmd.Context(), app.NewLauncher())

	var renderErr error
	if jsonOut(cmd) {
		enc := json.NewEncoder(cmd.OutOrStdout())
		renderErr = enc.Encode(result)
	} else {
		renderErr = renderDoctorText(cmd.OutOrStdout(), result)
	}
	if renderErr != nil {
		return renderErr
	}
	if cause != nil || result.Exit.Code != 0 {
		return &DoctorError{Result: result, Cause: cause}
	}
	return nil
}

func renderDoctorText(w io.Writer, result DoctorResult) error {
	fmt.Fprintln(w, "Portico Doctor")
	fmt.Fprintln(w, "==============")
	for _, check := range result.Checks {
		if strings.HasPrefix(check.ID, "connection:") {
			continue
		}
		if err := printDoctorCheck(w, check); err != nil {
			return err
		}
	}
	if result.Summary != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, result.Summary)
	}

	var blockedConnections []DoctorCheck
	for _, check := range result.Checks {
		if strings.HasPrefix(check.ID, "connection:") {
			blockedConnections = append(blockedConnections, check)
		}
	}
	if len(blockedConnections) > 0 {
		fmt.Fprintf(w, "\n%d connection(s) cannot open:\n", len(blockedConnections))
		for _, check := range blockedConnections {
			fmt.Fprintf(w, "  ✗ %s\n", doctorCheckTitle(check))
			for _, blocker := range strings.Split(check.Detail, "\n") {
				if blocker != "" {
					fmt.Fprintf(w, "      %s\n", blocker)
				}
			}
			if check.Detail == "" {
				fmt.Fprintf(w, "      %s\n", check.Summary)
			}
		}
	}

	if len(result.Blockers) > 0 {
		fmt.Fprintf(w, "\n%d blocker(s) prevent Portico from being ready.\n", len(result.Blockers))
	} else {
		fmt.Fprintln(w, "\nNothing is wrong that Portico can see.")
	}
	return nil
}

func printDoctorCheck(w io.Writer, check DoctorCheck) error {
	if _, err := fmt.Fprintf(w, "%s %s\n", doctorCheckMark(check.State), doctorCheckTitle(check)); err != nil {
		return err
	}
	if check.Summary != "" {
		if _, err := fmt.Fprintf(w, "    %s\n", check.Summary); err != nil {
			return err
		}
	}
	if check.Detail != "" {
		if _, err := fmt.Fprintf(w, "    %s\n", check.Detail); err != nil {
			return err
		}
	}
	if check.NextAction != "" {
		if _, err := fmt.Fprintf(w, "    → %s\n", check.NextAction); err != nil {
			return err
		}
	}
	if check.Technical != "" {
		if _, err := fmt.Fprintf(w, "    (%s)\n", check.Technical); err != nil {
			return err
		}
	}
	return nil
}

func doctorCheckMark(state string) string {
	switch state {
	case "ok":
		return "✓"
	case "attention":
		return "~"
	case "problem":
		return "✗"
	default:
		return "?"
	}
}

func doctorCheckTitle(check DoctorCheck) string {
	if check.Title != "" {
		return check.Title
	}
	return check.ID
}

func doctorFileCheck(id, label, path string, expectedMode os.FileMode) DoctorCheck {
	check := DoctorCheck{ID: id, Title: label}
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		check.State = "attention"
		check.Summary = "Not created yet."
	case err != nil:
		check.State = "unknown"
		check.Summary = "Portico could not inspect this file."
		check.Technical = err.Error()
	case !info.Mode().IsRegular():
		check.State = "problem"
		check.Summary = "The path is not a regular file."
		check.NextAction = "Replace it with the expected Portico file."
	case info.Mode().Perm() != expectedMode:
		check.State = "problem"
		check.Summary = fmt.Sprintf("Permissions are %04o; expected %04o.", info.Mode().Perm(), expectedMode)
		check.Classification = "insecure_database"
		check.NextAction = "Restrict the file to the Portico user (0600)."
	default:
		check.State = "ok"
		check.Summary = path
	}
	return check
}

func doctorSecretKeyChecks(dataDir string) []DoctorCheck {
	entries, err := os.ReadDir(dataDir)
	if os.IsNotExist(err) {
		return []DoctorCheck{{
			ID:      "installation_key",
			Title:   "Installation key",
			State:   "attention",
			Summary: "Not created yet.",
		}}
	}
	if err != nil {
		return []DoctorCheck{{
			ID:         "installation_key",
			Title:      "Installation key",
			State:      "unknown",
			Summary:    "Portico could not read its data directory to find the key.",
			Technical:  err.Error(),
			NextAction: "Restore access to the Portico data directory, then retry doctor.",
		}}
	}

	var checks []DoctorCheck
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !keys.IsInstallationKeyFile(name) {
			continue
		}
		check := DoctorCheck{
			ID:    "installation_key:" + name,
			Title: "Installation key: " + name,
		}
		path := filepath.Join(dataDir, name)
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			check.State = "unknown"
			check.Summary = "Portico could not inspect this key file."
			check.Technical = statErr.Error()
		case !info.Mode().IsRegular():
			check.State = "problem"
			check.Summary = "The key path is not a regular file."
			check.Classification = "insecure_key"
		case info.Mode().Perm() != 0600:
			check.State = "problem"
			check.Summary = fmt.Sprintf("Permissions are %04o; expected 0600.", info.Mode().Perm())
			check.Classification = "insecure_key"
			check.NextAction = "Restrict the key file to the Portico user (0600)."
		default:
			check.State = "ok"
			check.Summary = path
		}
		checks = append(checks, check)
	}
	if len(checks) == 0 {
		checks = append(checks, DoctorCheck{
			ID:      "installation_key",
			Title:   "Installation key",
			State:   "attention",
			Summary: "Not created yet.",
		})
	}
	return checks
}
