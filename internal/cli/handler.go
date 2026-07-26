package cli

import (
	"encoding/json"
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
	"github.com/B-A-M-N/portico/internal/config"
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
	fmt.Printf("State:    %s\n", conn.UserState)
	fmt.Printf("Provider: %s\n", conn.ProviderID)
	if conn.PublicAddress != "" {
		fmt.Printf("Address:  %s\n", conn.PublicAddress)
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

	op, err := client.ApplyPlan(cmd.Context(), plan.ID)
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

	op, err := client.ApplyPlan(cmd.Context(), plan.ID)
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

	op, err := client.ApplyPlan(cmd.Context(), plan.ID)
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
	op, err := client.ApplyPlan(cmd.Context(), plan.ID)
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
	op, err := client.ApplyPlan(cmd.Context(), planID)
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
	}
	return nil
}

func handleProviderLogin(cmd *cobra.Command, id string) error {
	if id != "cloudflare" {
		return fmt.Errorf("provider login for %q is not supported", id)
	}
	accountID, _ := cmd.Flags().GetString("account-id")
	zoneID, _ := cmd.Flags().GetString("zone-id")
	if accountID == "" || zoneID == "" {
		return fmt.Errorf("cloudflare login requires --account-id and --zone-id")
	}
	if err := config.Init(); err != nil {
		return fmt.Errorf("initialize config: %w", err)
	}
	token := config.APIToken()
	if token == "" {
		return fmt.Errorf("set CLOUDFLARE_API_TOKEN in the environment before logging in; Portico never accepts provider tokens on the command line")
	}
	if err := config.SaveCloudflareSetup(accountID, zoneID, token); err != nil {
		return fmt.Errorf("save Cloudflare setup: %w", err)
	}
	fmt.Println("Cloudflare credentials saved securely. Restart Portico's supervisor to activate full Cloudflare connections.")
	return nil
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

func handleDoctor(cmd *cobra.Command) error {
	fmt.Println("Portico Doctor")
	fmt.Println("==============")
	launcher := app.NewLauncher()
	paths := launcher.GetPaths()
	doctorFileStatus("Database", paths.DatabasePath, 0600)
	doctorFileStatus("Installation key", filepath.Join(filepath.Dir(paths.DatabasePath), "portico-key.bin"), 0600)

	// Doctor is observational by default. In particular, it must not call
	// getClient because that helper starts a supervisor when none is running.
	client := launcher.ConnectToSupervisor()
	if err := client.Health(cmd.Context()); err != nil {
		fmt.Println("~ Supervisor: not running (no changes made)")
		return nil
	}
	snap, err := client.Snapshot(cmd.Context())
	if err != nil {
		return fmt.Errorf("read supervisor snapshot: %w", err)
	}
	fmt.Printf("✓ Supervisor reachable (seq: %d)\n", snap.LastSeq)
	fmt.Printf("✓ Connections: %d\n", len(snap.Connections))
	// Check providers
	providers, err := client.ListProviders(cmd.Context())
	if err != nil {
		fmt.Printf("⚠ Provider list: %v\n", err)
	} else {
		fmt.Printf("✓ Providers: %d\n", len(providers))
		for _, p := range providers {
			status := "✓"
			if !p.Authenticated {
				status = "✗"
			}
			fmt.Printf("  %s %s (%s)\n", status, p.DisplayName, p.ID)
		}
	}
	// Check connections
	running := 0
	for _, c := range snap.Connections {
		if c.RuntimeState == "open" {
			running++
		}
	}
	fmt.Printf("✓ Running connections: %d/%d\n", running, len(snap.Connections))
	// Discovery check
	if result, err := client.Discovery(cmd.Context()); err == nil && len(result.Services) > 0 {
		fmt.Printf("✓ Discovery: %d services available\n", len(result.Services))
	} else {
		fmt.Printf("~ Discovery: not available or no services\n")
	}
	return nil
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

	op, err := client.ApplyPlan(cmd.Context(), plan.ID)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}

	if jsonFlag {
		return json.NewEncoder(os.Stdout).Encode(op)
	}

	fmt.Printf("Operation: %s (%s)\n", op.ID, op.State)
	return nil
}
