package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/ipc"
	"github.com/B-A-M-N/portico/internal/supervisor"
)

// stdinIsTTY reports whether stdin is attached to a terminal.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// confirm prompts on stderr and reads a response from stdin.
// It returns an error if stdin is not a terminal, so noninteractive
// callers must pass --yes instead of hanging on a prompt.
func confirm(prompt string) (bool, error) {
	if !stdinIsTTY() {
		return false, fmt.Errorf("confirmation required but stdin is not a terminal; re-run with --yes")
	}
	fmt.Fprint(os.Stderr, prompt)
	var response string
	fmt.Scanln(&response)
	return response == "y" || response == "Y", nil
}

// getClient returns an IPC client to the supervisor.
// It ensures the supervisor is running before returning the client.
func getClient(cmd *cobra.Command) (*ipc.Client, error) {
	launcher := app.NewLauncher()

	// Ensure supervisor is running (this handles starting it if needed)
	if err := launcher.EnsureSupervisor(cmd.Context()); err != nil {
		return nil, fmt.Errorf("supervisor unavailable: %w", err)
	}

	// Create the client
	client := launcher.ConnectToSupervisor()

	// Verify supervisor is reachable
	if err := client.Health(cmd.Context()); err != nil {
		return nil, fmt.Errorf("supervisor not available at %s: %w", launcher.GetSocketPath(), err)
	}

	return client, nil
}

func runSupervisor(cmd *cobra.Command) error {
	return supervisor.RunSupervisor(cmd.Context())
}

func supervisorStart(cmd *cobra.Command) error {
	launcher := app.NewLauncher()

	// Use the detached launcher to start supervisor
	if err := launcher.StartSupervisor(cmd.Context()); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}

	fmt.Println("Supervisor started in background")
	fmt.Printf("Socket: %s\n", launcher.GetSocketPath())
	fmt.Printf("Database: %s\n", launcher.GetPaths().DatabasePath)
	return nil
}

func supervisorStatus(cmd *cobra.Command) error {
	launcher := app.NewLauncher()
	client := launcher.ConnectToSupervisor()

	// Don't start the supervisor — just check if it's running.
	if err := client.Health(cmd.Context()); err != nil {
		fmt.Println("Supervisor: not running")
		return nil
	}

	fmt.Println("Supervisor: running")
	return nil
}

func supervisorStop(cmd *cobra.Command) error {
	launcher := app.NewLauncher()
	force, _ := cmd.Flags().GetBool("force")

	// Do NOT use getClient() — that would start the supervisor if not running.
	// Just create a client and check if it responds.
	client := launcher.ConnectToSupervisor()

	// Check if supervisor is actually running before trying to stop it.
	if err := client.Health(cmd.Context()); err != nil {
		if force {
			fmt.Println("Cleaning up stale supervisor state...")
			return app.CleanupLock(launcher.GetPaths())
		}
		return fmt.Errorf("supervisor is not running (use --force to clean stale state): %w", err)
	}

	// Request graceful shutdown via IPC.
	if err := client.StopSupervisor(cmd.Context()); err != nil {
		if force {
			slog.Warn("stop failed, cleaning up stale state", "err", err)
			return app.CleanupLock(launcher.GetPaths())
		}
		return fmt.Errorf("stop supervisor: %w", err)
	}

	fmt.Println("Shutdown requested, waiting for supervisor to stop...")

	// Poll health until the socket is gone or deadline expires.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		default:
		}

		err := client.Health(cmd.Context())
		if err != nil {
			// Supervisor is down — connection refused or socket gone.
			fmt.Println("Supervisor stopped")
			return nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("timed out waiting for supervisor to stop after 10s")
}

func handleLogs(cmd *cobra.Command) error {
	paths := app.DefaultPaths()
	logPath := filepath.Join(paths.LogDir, "supervisor.log")
	lines, _ := cmd.Flags().GetInt("lines")
	follow, _ := cmd.Flags().GetBool("follow")

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no supervisor log found at %s", logPath)
		}
		return fmt.Errorf("open supervisor log: %w", err)
	}
	defer func() { _ = f.Close() }()

	offset, err := printLastLines(f, os.Stdout, lines)
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// Follow by polling for new content.
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}

		// Rename-based rotation leaves the original descriptor readable but no
		// longer connected to the path users asked to follow. Reopen the path
		// before checking size so `portico logs -f` follows the new inode.
		reopened, next, rotated, err := reopenRotatedLog(logPath, f, offset)
		if err != nil {
			return fmt.Errorf("check supervisor log rotation: %w", err)
		}
		if rotated {
			if err := f.Close(); err != nil {
				return fmt.Errorf("close rotated supervisor log: %w", err)
			}
			f, offset = reopened, next
		}

		fi, err := f.Stat()
		if err != nil {
			return fmt.Errorf("stat supervisor log: %w", err)
		}
		size := fi.Size()
		if size < offset {
			// File was truncated or rotated; start from the beginning.
			offset = 0
		}
		if size > offset {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return err
			}
			n, err := io.Copy(os.Stdout, f)
			offset += n
			if err != nil {
				return err
			}
		}
	}
}

// reopenRotatedLog detects rename-based rotation by comparing the current
// descriptor with the path's file identity. When the path names a new file it
// returns an opened replacement and a zero offset. A temporarily absent path
// is normal during rotation and leaves the existing descriptor untouched.
func reopenRotatedLog(path string, current *os.File, offset int64) (*os.File, int64, bool, error) {
	pathInfo, err := os.Stat(path)
	if os.IsNotExist(err) {
		return current, offset, false, nil
	}
	if err != nil {
		return nil, offset, false, err
	}
	currentInfo, err := current.Stat()
	if err != nil {
		return nil, offset, false, err
	}
	if os.SameFile(pathInfo, currentInfo) {
		return current, offset, false, nil
	}
	replacement, err := os.Open(path)
	if err != nil {
		return nil, offset, false, err
	}
	return replacement, 0, true, nil
}

// printLastLines writes the last n lines of f to w and returns the
// file size at the time of reading (for follow mode).
func printLastLines(f *os.File, w io.Writer, n int) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()

	// Only read a bounded tail of the file.
	const maxTail = 256 * 1024
	start := int64(0)
	if size > maxTail {
		start = size - maxTail
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}

	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return size, nil
	}
	lines := strings.Split(text, "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return size, nil
}

func handleList(cmd *cobra.Command) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	conns, err := client.ListConnections(cmd.Context())
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(conns)
	}

	if len(conns) == 0 {
		fmt.Println("No connections")
		return nil
	}
	for _, c := range conns {
		state := c.UserState
		if state == "" {
			state = c.RuntimeState
		}
		fmt.Printf("%-36s %-20s %s\n", c.ID, c.Name, state)
	}
	return nil
}

func handleInspect(cmd *cobra.Command, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	conn, err := client.GetConnection(cmd.Context(), id)
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(conn)
	}

	fmt.Printf("ID:       %s\n", conn.ID)
	fmt.Printf("Name:     %s\n", conn.Name)
	fmt.Printf("Kind:     %s\n", connectionKindLabel(conn.Kind))
	fmt.Printf("State:    %s\n", conn.UserState)
	fmt.Printf("Provider: %s\n", conn.ProviderID)
	if conn.ProviderAccountID != "" {
		fmt.Printf("Account:  %s\n", conn.ProviderAccountID)
	}
	// A port forward and a client tunnel have no public address, so printing
	// only the public one reported them as having no address at all.
	if conn.PublicAddress != "" {
		fmt.Printf("Address:  %s\n", conn.PublicAddress)
	}
	if conn.PrivateAddress != "" {
		fmt.Printf("%-9s %s\n", privateAddressLabel(conn.Kind)+":", conn.PrivateAddress)
	}
	return nil
}

func handleOpen(cmd *cobra.Command, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	plan, err := client.PlanOpen(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if !jsonFlag {
		fmt.Printf("Plan: %d steps\n", len(plan.Steps))
		for _, step := range plan.Steps {
			fmt.Printf("  - %s\n", step.Summary)
		}
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		ok, err := confirm("Apply this plan? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Cancelled")
			return nil
		}
	}

	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), plan.ID, applyKeyFor(plan.ID))
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}

	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Operation: %s (state: %s)\n", op.ID, op.State)
	return nil
}

func handleClose(cmd *cobra.Command, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	plan, err := client.PlanClose(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("plan close: %w", err)
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		ok, err := confirm("Apply close plan? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Cancelled")
			return nil
		}
	}

	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), plan.ID, applyKeyFor(plan.ID))
	if err != nil {
		return fmt.Errorf("apply close: %w", err)
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Close operation: %s (%s)\n", op.ID, op.State)
	return nil
}

func handleDelete(cmd *cobra.Command, id string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes && !stdinIsTTY() {
		return fmt.Errorf("delete is destructive and requires --yes when running noninteractively")
	}

	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	plan, err := client.PlanDelete(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("plan delete: %w", err)
	}

	if !yes {
		fmt.Fprintf(os.Stderr, "Plan: %d steps\n", len(plan.Steps))
		for _, step := range plan.Steps {
			mark := " "
			if step.Destructive {
				mark = "!"
			}
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", mark, step.Summary)
		}
		ok, err := confirm("Apply delete plan? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Cancelled")
			return nil
		}
	}

	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), plan.ID, applyKeyFor(plan.ID))
	if err != nil {
		return fmt.Errorf("apply delete: %w", err)
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Delete operation: %s (%s)\n", op.ID, op.State)
	return nil
}

func handlePlan(cmd *cobra.Command, action, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	var plan *ipc.PlanDTO
	switch action {
	case "open":
		plan, err = client.PlanOpen(cmd.Context(), id)
	case "close":
		plan, err = client.PlanClose(cmd.Context(), id)
	case "repair":
		plan, err = client.PlanRepair(cmd.Context(), id)
	case "delete":
		plan, err = client.PlanDelete(cmd.Context(), id)
	default:
		return fmt.Errorf("unknown plan action: %s (use: open, close, repair, delete)", action)
	}
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(plan)
	}

	fmt.Printf("Plan: %s (intent: %s, fingerprint: %s)\n", plan.ID, plan.Intent, plan.Fingerprint)
	for _, step := range plan.Steps {
		mark := " "
		if step.Destructive {
			mark = "!"
		}
		fmt.Printf("  [%s] %s\n", mark, step.Summary)
	}
	return nil
}

func handleRepair(cmd *cobra.Command, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	// First, get repair plan
	plan, err := client.PlanRepair(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("plan repair: %w", err)
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if plan.Noop {
		if jsonFlag {
			return json.NewEncoder(os.Stdout).Encode(plan)
		}
		fmt.Fprintln(os.Stdout, "No repair needed")
		return nil
	}
	if !jsonFlag {
		fmt.Printf("Repair Plan: %s (fingerprint: %s)\n", plan.ID, plan.Fingerprint)
		fmt.Println("Proposed changes:")
		for _, step := range plan.Steps {
			mark := " "
			if step.Destructive {
				mark = "!"
			}
			fmt.Printf("  [%s] %s\n", mark, step.Summary)
		}
	}

	// Ask for confirmation unless --yes
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		ok, err := confirm("Apply this repair plan? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Cancelled")
			return nil
		}
	}

	// Apply the repair plan
	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), plan.ID, applyKeyFor(plan.ID))
	if err != nil {
		return fmt.Errorf("apply repair: %w", err)
	}

	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Repair operation: %s (%s)\n", op.ID, op.State)
	return nil
}

func handleApply(cmd *cobra.Command, planID string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), planID, applyKeyFor(planID))
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Operation: %s (%s)\n", op.ID, op.State)
	return nil
}

func handleProviderList(cmd *cobra.Command) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	providers, err := client.ListProviders(cmd.Context())
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(providers)
	}

	for _, p := range providers {
		status := "✓"
		if !p.Authenticated {
			status = "✗"
		}
		fmt.Printf("%s %s (%s)\n", status, p.DisplayName, p.ID)
		for _, account := range p.Accounts {
			label := account.Label
			if label == "" {
				label = account.ID
			}
			state := account.Status
			if state == "" {
				state = "configured"
			}
			fmt.Printf("  - %s (%s)\n", label, state)
		}
		// An account that exists but cannot be used is the thing most likely to
		// be causing a problem. Listing only usable accounts made a provider
		// that says "needs setup" after setup look inexplicable.
		for _, account := range p.PendingAccounts {
			label := account.Label
			if label == "" {
				label = account.ID
			}
			state := account.Status
			if state == "" {
				state = "unverified"
			}
			fmt.Printf("  ! %s (%s — not usable)\n", label, state)
		}
	}
	return nil
}

// handleProviderLogin configures any provider that declares a setup flow.
//
// It used to be Cloudflare and nothing else, and it required a zone that the
// provider's own declaration marks optional — so the CLI enforced a stricter
// contract than the supervisor, and refused a tunnel-only setup the TUI accepts.
// One provider contract, declared by the provider, read by both interfaces.
func handleProviderLogin(cmd *cobra.Command, id string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	flow, err := client.ProviderSetupFlow(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("ask %s what it needs: %w", id, err)
	}
	if !flow.StoresAccount() {
		// A guidance flow describes what to do elsewhere. Collecting values
		// here would store something nothing reads.
		fmt.Printf("%s cannot be configured through Portico.\n\n", id)
		if flow.GuidanceReason != "" {
			fmt.Println(flow.GuidanceReason)
			fmt.Println()
		}
		fmt.Println("What this provider needs:")
		for _, field := range flow.Fields {
			fmt.Printf("  • %s\n", field.Label)
			if field.Description != "" {
				fmt.Printf("    %s\n", field.Description)
			}
		}
		return nil
	}

	values, err := collectSetupValues(cmd, flow)
	if err != nil {
		return err
	}

	req := ipc.ConfigureProviderAccountRequest{Fields: values}
	// The identity and secret fields have dedicated request fields, so they are
	// lifted out of the generic map rather than duplicated in it.
	req.AccountID = values["account_id"]
	req.ZoneID = values["zone_id"]
	req.Label = values["label"]
	req.Credential = values["credential"]

	response, err := client.ConfigureProviderAccount(cmd.Context(), id, req)
	if err != nil {
		return describeProviderSetupFailure(id, err)
	}

	fmt.Printf("%s account saved.\n", id)
	// What the account can actually do. A zone is needed only for DNS and
	// custom hostnames, so an account saved without one is useful and limited,
	// and saying which is the difference between a working setup and a puzzle.
	if response.CapabilityLevel != "" {
		fmt.Printf("  Capability: %s\n", response.CapabilityLevel)
	}
	if !response.Validated {
		fmt.Println("  The credential could not be confirmed with the provider; " +
			"the account is saved as unverified.")
	}
	if response.RestartRequired {
		fmt.Println("Run 'portico supervisor stop' then 'portico supervisor start' to activate it.")
	}
	return nil
}

// collectSetupValues gathers the fields a provider declared.
//
// Non-secret values come from flags or the environment. Secrets come only from
// the environment: a secret passed as an argument is in the shell history and
// visible in the process list to every user on the machine.
func collectSetupValues(cmd *cobra.Command, flow *ipc.SetupFlowDTO) (map[string]string, error) {
	values := map[string]string{}

	for _, field := range flow.Fields {
		var value string

		if !field.Secret {
			// A flag named after the field, so --account-id still works.
			flagName := strings.ReplaceAll(field.ID, "_", "-")
			if f := cmd.Flags().Lookup(flagName); f != nil {
				value = strings.TrimSpace(f.Value.String())
			}
		}
		if value == "" {
			for _, name := range field.EnvVars {
				if fromEnv := strings.TrimSpace(os.Getenv(name)); fromEnv != "" {
					value = fromEnv
					break
				}
			}
		}

		if value == "" && field.Required {
			if field.Secret {
				return nil, fmt.Errorf(
					"%s is required; set %s in the environment — Portico never accepts a secret "+
						"as a command argument, because arguments are recorded in shell history "+
						"and visible in the process list",
					field.Label, strings.Join(field.EnvVars, " or "))
			}
			return nil, fmt.Errorf("%s is required; pass --%s or set %s",
				field.Label, strings.ReplaceAll(field.ID, "_", "-"),
				strings.Join(field.EnvVars, " or "))
		}
		if value != "" {
			values[field.ID] = value
		}
	}
	return values, nil
}

// describeProviderSetupFailure reports why a credential was refused, using the
// typed detail the supervisor sends rather than only its summary.
func describeProviderSetupFailure(id string, err error) error {
	var status *ipc.APIStatusError
	if !errors.As(err, &status) || status.ProviderValidation == nil {
		return fmt.Errorf("save %s account: %w", id, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "save %s account: %s", id, status.Message)
	if len(status.ProviderValidation.MissingPermissions) > 0 {
		b.WriteString("\n\nThe token is missing:")
		for _, permission := range status.ProviderValidation.MissingPermissions {
			b.WriteString("\n  • " + permission)
		}
	}
	return errors.New(b.String())
}

func handleDiscover(cmd *cobra.Command) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}
	result, err := client.Discovery(cmd.Context())
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if len(result.Services) == 0 {
		fmt.Println("No local services discovered")
		return nil
	}
	fmt.Printf("Discovered %d local services:\n", len(result.Services))
	for _, svc := range result.Services {
		label := svc.Process
		if label == "" {
			label = "unknown process"
		}
		fmt.Printf("  %s (%s) - %s\n", svc.Address, svc.Protocol, label)
	}
	return nil
}

// handleDoctor reports what Portico can see about this machine.
//
// It used to make its own health judgements: a switch over provider availability
// strings, reaching verdicts the readiness screen reached differently from the same
// data. Two interpretations of one field is one too many, and the one that drifted
// was whichever nobody was reading.
//
// So doctor now prints the supervisor's checks. It adds only what the supervisor
// cannot answer — the state of files on this machine, which doctor can see without
// a supervisor running — and stays read-only: everything below observes and nothing
// changes state.
func handleDoctor(cmd *cobra.Command) error {
	fmt.Println("Portico Doctor")
	fmt.Println("==============")
	launcher := app.NewLauncher()
	paths := launcher.GetPaths()

	// Local files first, because these are answerable whether or not a supervisor
	// is running — and when one is not, they are all a user gets.
	doctorFileStatus("Database", paths.DatabasePath, 0600)
	doctorSecretKeyStatus(filepath.Dir(paths.DatabasePath))

	// Doctor is observational by default. In particular, it must not call
	// getClient because that helper starts a supervisor when none is running.
	client := launcher.ConnectToSupervisor()
	if err := client.Health(cmd.Context()); err != nil {
		fmt.Printf("\n✗ Supervisor: not running (%v)\n", err)
		fmt.Println("  Everything below needs a running supervisor. Start one with:")
		fmt.Println("    portico supervisor run")
		return fmt.Errorf("supervisor is not running: %w", err)
	}

	snap, err := client.Snapshot(cmd.Context())
	if err != nil {
		return fmt.Errorf("read supervisor snapshot: %w", err)
	}
	running := 0
	for _, c := range snap.Connections {
		if c.RuntimeState == "open" {
			running++
		}
	}
	fmt.Printf("\n✓ Supervisor reachable (seq: %d)\n", snap.LastSeq)
	fmt.Printf("✓ Connections: %d, of which %d open\n", len(snap.Connections), running)

	// The supervisor's own checks. Readiness carries them, so this is the same
	// interpretation the setup screen shows rather than a second one.
	readiness, err := client.Readiness(cmd.Context())
	if err != nil {
		fmt.Printf("\n⚠ Could not read the supervisor's health checks: %v\n", err)
		return nil
	}

	fmt.Println()
	if readiness.Summary != "" {
		fmt.Println(readiness.Summary)
		fmt.Println()
	}

	problems := 0
	for _, check := range readiness.Checks {
		fmt.Printf("%s %s\n", doctorCheckMark(check.State), doctorCheckTitle(check))
		if check.Summary != "" {
			fmt.Printf("    %s\n", check.Summary)
		}
		if check.Detail != "" {
			fmt.Printf("    %s\n", check.Detail)
		}
		if check.NextAction != "" {
			fmt.Printf("    → %s\n", check.NextAction)
		}
		// Technical detail is secondary and printed last, so a reader who does not
		// need it does not have to read past it to find what to do.
		if check.Technical != "" {
			fmt.Printf("    (%s)\n", check.Technical)
		}
		if check.State == "problem" {
			problems++
		}
	}

	// Connections that cannot open, with the reason. The supervisor computed these
	// blockers; doctor reports them rather than working them out again.
	var blocked []ipc.ConnectionReadinessDTO
	for _, conn := range readiness.Connections {
		if !conn.Ready {
			blocked = append(blocked, conn)
		}
	}
	if len(blocked) > 0 {
		fmt.Printf("\n%d connection(s) cannot open:\n", len(blocked))
		for _, conn := range blocked {
			fmt.Printf("  ✗ %s\n", conn.Name)
			for _, blocker := range conn.Blockers {
				fmt.Printf("      %s\n", blocker)
			}
		}
	}

	if problems > 0 {
		// A non-zero exit is how a script learns something is wrong. The message
		// says what it means, because "exit 1" on its own does not.
		fmt.Printf("\n%d problem(s) will stop Portico working.\n", problems)
		return fmt.Errorf("%d health problem(s) found", problems)
	}
	fmt.Println("\nNothing is wrong that Portico can see.")
	return nil
}

// doctorCheckMark is the leading glyph for a check's state.
func doctorCheckMark(state string) string {
	switch state {
	case "ok":
		return "✓"
	case "attention":
		return "~"
	case "problem":
		return "✗"
	default:
		// Unknown is not a pass. Printing a tick for a check that could not run is
		// how a broken machine reads as a healthy one.
		return "?"
	}
}

// doctorCheckTitle names the check, falling back to its ID.
func doctorCheckTitle(check ipc.HealthCheckDTO) string {
	if check.Title != "" {
		return check.Title
	}
	return check.ID
}

// doctorSecretKeyStatus reports the status of the secret store installation key.
// It lists all versioned key files rather than hardcoding "portico-key.bin".
func doctorSecretKeyStatus(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		fmt.Printf("~ Secret store: cannot read %s: %v\n", dataDir, err)
		return
	}
	var keyFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "portico-key.bin" || strings.HasPrefix(name, "portico-key-v") && strings.HasSuffix(name, ".bin") {
			keyFiles = append(keyFiles, name)
		}
	}
	if len(keyFiles) == 0 {
		fmt.Printf("~ Installation key: not created yet\n")
		return
	}
	for _, name := range keyFiles {
		path := filepath.Join(dataDir, name)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Printf("✗ Installation key: %s: %v\n", name, err)
			continue
		}
		if !info.Mode().IsRegular() {
			fmt.Printf("✗ Installation key: %s: not a regular file\n", name)
			continue
		}
		if info.Mode().Perm() != 0600 {
			fmt.Printf("✗ Installation key: %s: permissions %o, expected 600\n", name, info.Mode().Perm())
			continue
		}
		fmt.Printf("✓ Installation key: %s\n", path)
	}
}

func doctorFileStatus(label, path string, expectedMode os.FileMode) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		fmt.Printf("~ %s: not created yet\n", label)
		return
	}
	if err != nil {
		fmt.Printf("✗ %s: %v\n", label, err)
		return
	}
	if !info.Mode().IsRegular() {
		fmt.Printf("✗ %s: not a regular file\n", label)
		return
	}
	if info.Mode().Perm() != expectedMode {
		fmt.Printf("✗ %s: permissions %o, expected %o\n", label, info.Mode().Perm(), expectedMode)
		return
	}
	fmt.Printf("✓ %s: %s\n", label, path)
}

// createConnection creates a connection from the command flags.
func createConnection(cmd *cobra.Command, name string) (*ipc.ConnectionDTO, error) {
	client, err := getClient(cmd)
	if err != nil {
		return nil, err
	}
	source, _ := cmd.Flags().GetString("source")
	sourceType, _ := cmd.Flags().GetString("source-type")
	protocol, _ := cmd.Flags().GetString("source-protocol")
	port, _ := cmd.Flags().GetInt("source-port")
	args, _ := cmd.Flags().GetStringArray("source-arg")
	workingDir, _ := cmd.Flags().GetString("source-working-dir")
	env, _ := cmd.Flags().GetStringToString("source-env")
	useShell, _ := cmd.Flags().GetBool("source-shell")
	directoryMode, _ := cmd.Flags().GetString("directory-mode")
	directorySPA, _ := cmd.Flags().GetBool("directory-spa")
	directoryUpload, _ := cmd.Flags().GetBool("directory-allow-upload")
	directoryDelete, _ := cmd.Flags().GetBool("directory-allow-delete")
	mcpCommand, _ := cmd.Flags().GetBool("mcp-command")
	providerID, _ := cmd.Flags().GetString("provider")
	sourceDTO := ipc.SourceDTO{Kind: sourceType}
	switch sourceType {
	case "existing_service":
		var addressErr error
		source, addressErr = normalizeExistingAddress(source, port)
		if addressErr != nil {
			return nil, addressErr
		}
		sourceDTO.Existing = &ipc.ExistingSourceDTO{Address: source, Protocol: protocol}
	case "directory":
		sourceDTO.Directory = &ipc.DirectorySourceDTO{Path: source, Mode: directoryMode, SPAFallback: directorySPA, AllowUpload: directoryUpload, AllowDelete: directoryDelete}
	case "command":
		sourceDTO.Command = &ipc.CommandSourceDTO{Executable: source, Args: args, WorkingDir: workingDir, Env: env, Port: port, Protocol: protocol, UseShell: useShell}
	case "mcp_server":
		if mcpCommand {
			sourceDTO.MCP = &ipc.MCPSourceDTO{Transport: "http", Command: &ipc.CommandSourceDTO{Executable: source, Args: args, WorkingDir: workingDir, Env: env, Port: port, Protocol: protocol, UseShell: useShell}}
		} else {
			sourceDTO.MCP = &ipc.MCPSourceDTO{Transport: "http", Endpoint: source}
		}
	default:
		return nil, fmt.Errorf("unsupported source type %q", sourceType)
	}

	conn, err := client.CreateConnection(cmd.Context(), ipc.CreateConnectionRequest{
		Version: 1,
		Name:    name,
		Source:  sourceDTO,
		Exposure: ipc.ExposureDTO{
			Mode: "temporary_public",
		},
		Provider: ipc.ProviderSelectionDTO{
			ProviderID: providerID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	return conn, nil
}

// normalizeExistingAddress accepts a host, host:port, IPv6 literal, or HTTP
// URL and produces the host:port form required by core.ExistingService.
func normalizeExistingAddress(raw string, port int) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("source address is required")
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("source URL must contain only scheme and host[:port]")
		}
		value = parsed.Host
	}
	if _, _, err := net.SplitHostPort(value); err == nil {
		return value, nil
	}
	if port <= 0 {
		return value, nil
	}
	host := strings.Trim(value, "[]")
	if net.ParseIP(host) != nil {
		return net.JoinHostPort(host, fmt.Sprintf("%d", port)), nil
	}
	if strings.Contains(value, ":") {
		return "", fmt.Errorf("source address %q is not a valid host or host:port", raw)
	}
	return net.JoinHostPort(value, fmt.Sprintf("%d", port)), nil
}

func handleCreate(cmd *cobra.Command, name string) error {
	conn, err := createConnection(cmd, name)
	if err != nil {
		return err
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(conn)
	}

	fmt.Printf("Connection created: %s (%s)\n", conn.Name, conn.ID)
	return nil
}

func handleServe(cmd *cobra.Command, name string) error {
	jsonFlag, _ := cmd.Flags().GetBool("json")

	// Step 1: Create
	conn, err := createConnection(cmd, name)
	if err != nil {
		return err
	}
	if !jsonFlag {
		fmt.Printf("Connection created: %s (%s)\n", conn.Name, conn.ID)
	}

	// Step 2: Plan open
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	plan, err := client.PlanOpen(cmd.Context(), conn.ID)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	if !jsonFlag {
		fmt.Printf("Plan: %d steps\n", len(plan.Steps))
		for _, step := range plan.Steps {
			fmt.Printf("  - %s\n", step.Summary)
		}
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		ok, err := confirm("Apply? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Cancelled")
			return nil
		}
	}

	op, err := client.ApplyPlanWithIdempotency(cmd.Context(), plan.ID, applyKeyFor(plan.ID))
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}

	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Operation: %s (%s)\n", op.ID, op.State)
	return nil
}

// connectionKindLabel names the kind in the words a user would use.
func connectionKindLabel(kind string) string {
	switch kind {
	case "port_forward":
		return "Port forward"
	case "private_network":
		return "Private network"
	case "client_tunnel":
		return "Client tunnel (no public address)"
	case "service_exposure", "":
		return "Published service"
	default:
		return kind
	}
}

// privateAddressLabel says what the non-public address is for this kind.
func privateAddressLabel(kind string) string {
	switch kind {
	case "port_forward":
		return "Listening"
	case "private_network":
		return "Network"
	default:
		return "Private"
	}
}

// handleProviderRemoveAccount forgets a stored provider account.
//
// It previews first. The scriptable surface had no preview at all, which made
// "what the preview describes is exactly what applying does" vacuous here: there
// was nothing to compare against.
func handleProviderRemoveAccount(cmd *cobra.Command, providerID, accountID string) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	preview, err := client.PreviewProviderAccountRemoval(cmd.Context(), providerID, accountID)
	if err != nil {
		return err
	}

	name := preview.Label
	if name == "" {
		name = preview.AccountID
	}
	fmt.Printf("Remove %s from %s?\n\n", name, providerID)
	for _, line := range preview.Consequences {
		fmt.Printf("  %s\n", line)
	}

	if !preview.Removable {
		fmt.Println("\nThis account cannot be removed yet:")
		for _, dep := range preview.Dependencies {
			depName := dep.Name
			if depName == "" {
				depName = dep.ID
			}
			fmt.Printf("  • %s — %s\n", depName, dep.Explanation)
		}
		return fmt.Errorf("nothing was removed")
	}

	assumeYes, _ := cmd.Flags().GetBool("yes")
	if !assumeYes {
		fmt.Print("\nType the account ID to confirm: ")
		var typed string
		if _, err := fmt.Fscanln(cmd.InOrStdin(), &typed); err != nil {
			return fmt.Errorf("nothing was removed")
		}
		if strings.TrimSpace(typed) != accountID {
			return fmt.Errorf("that did not match %q; nothing was removed", accountID)
		}
	}

	// The fingerprint of the preview just shown. If the account changed in
	// between, the supervisor refuses rather than removing something else.
	response, err := client.RemoveProviderAccount(cmd.Context(), providerID, accountID, preview.Fingerprint)
	if err != nil {
		return err
	}
	if !response.Removed {
		return fmt.Errorf("the account was not removed")
	}

	jsonFlag, _ := cmd.Flags().GetBool("json")
	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(response)
	}

	fmt.Printf("Removed account %s from %s.\n", accountID, providerID)
	if response.RestartRequired {
		fmt.Println("Restart the supervisor to finish applying it.")
	}
	return nil
}

// handleSupportExport writes the redacted diagnostic report.
func handleSupportExport(cmd *cobra.Command) error {
	client, err := getClient(cmd)
	if err != nil {
		return err
	}

	export, err := client.SupportExport(cmd.Context())
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	encoded = append(encoded, '\n')

	output, _ := cmd.Flags().GetString("output")
	if output == "" {
		_, err = os.Stdout.Write(encoded)
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	if err := writePrivateFile(output, encoded, force); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
	fmt.Fprintln(os.Stderr, "Read it before sharing: it describes this machine's services and addresses.")
	return nil
}

// writePrivateFile writes a file only this user can read, atomically.
//
// os.WriteFile's mode applies only when it creates the file. Writing over an
// existing world-readable file left it world-readable, so the previous version
// of this claimed 0600 in a comment while producing 0644 — a redacted report is
// still a description of this machine's services and addresses.
//
// It is also written through a temporary file and renamed, so an interrupted
// write cannot leave a half-report that looks complete.
func writePrivateFile(path string, data []byte, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf(
				"%s already exists; pass --force to replace it", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s: %w", path, err)
		}
	}

	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".portico-export-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write report: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("flush report: %w", err)
	}
	// Set explicitly rather than relying on the creation mode, which umask
	// modifies.
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("restrict report permissions: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}

	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("move report into place: %w", err)
	}
	// The rename preserves the temporary file's mode, but an existing target
	// replaced by rename does not carry its own mode over — assert it anyway,
	// because the whole point is that this file is not readable by others.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict report permissions: %w", err)
	}
	return nil
}

// applyKeyFor derives a stable idempotency key for applying a plan.
//
// A plan is applied once. If the response is lost — a timeout, a dropped
// connection — the supervisor may already have started the operation, and a
// retry carrying the same key returns that operation rather than starting a
// second one. The plan ID is the natural key: it identifies exactly the
// approved change.
func applyKeyFor(planID string) string {
	return "apply-" + planID
}
