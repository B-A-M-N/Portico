package cli

import (
	"github.com/spf13/cobra"

	"github.com/B-A-M-N/portico/internal/app"
)

// NewCLI creates the Portico CLI with supervisor-aware commands.
func NewCLI() *cobra.Command {
	root := &cobra.Command{
		Use:   "portico",
		Short: "Connection manager for local services",
		Long: `portico is a terminal-native connection manager that discovers local services,
creates and manages provider-backed connections, and keeps them alive.

It supports Cloudflare Tunnel, local file serving, and provides diagnostics
for connection failures.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd)
		},
	}

	root.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")
	root.PersistentFlags().Bool("json", false, "Output in JSON format")

	// Supervisor commands
	root.AddCommand(newSupervisorCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newCreateCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newForwardCmd())
	root.AddCommand(newInspectCmd())
	root.AddCommand(newOpenCmd())
	root.AddCommand(newCloseCmd())
	root.AddCommand(newDeleteCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newApplyCmd())
	root.AddCommand(newProviderCmd())
	root.AddCommand(newSupportCmd())
	root.AddCommand(newDiscoverCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newRepairCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newLogsCmd("Show supervisor logs (alias for 'supervisor logs')"))

	// Setting Version makes 'portico --version' work and keeps the version
	// template consistent with the 'version' subcommand.
	root.Version = Version

	return root
}

func runTUI(cmd *cobra.Command) error {
	// Use the shared launcher to ensure supervisor and start TUI
	launcher := app.NewLauncher()
	return launcher.RunTUI(cmd.Context())
}

func newSupervisorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "supervisor",
		Short: "Manage the Portico supervisor daemon",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the supervisor in the foreground",
		Long:  "Start the Portico supervisor daemon in the current terminal. For background use 'supervisor start'.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSupervisor(cmd)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the supervisor in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			return supervisorStart(cmd)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show supervisor status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return supervisorStatus(cmd)
		},
	})

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the supervisor",
		RunE: func(cmd *cobra.Command, args []string) error {
			return supervisorStop(cmd)
		},
	}
	stopCmd.Flags().Bool("force", false, "Clean up stale lock state if supervisor is not running")
	cmd.AddCommand(stopCmd)

	cmd.AddCommand(newLogsCmd("Show supervisor logs"))

	return cmd
}

// newLogsCmd builds a logs command. It is used both as 'portico supervisor logs'
// (the canonical location) and as the top-level 'portico logs' alias.
func newLogsCmd(short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleLogs(cmd)
		},
	}
	cmd.Flags().IntP("lines", "n", 50, "Number of trailing lines to show")
	cmd.Flags().BoolP("follow", "f", false, "Follow the log for new output")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all connections",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleList(cmd)
		},
	}
}

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a connection without opening it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleCreate(cmd, args[0])
		},
	}
	addSourceFlags(cmd)
	cmd.Flags().String("provider", "cloudflare", "Provider to use")
	// Binds the connection to one stored account. The wizard asks with human
	// labels; this is the scriptable equivalent, and it is what makes the
	// account-removal refusal (which names dependent connections) reachable
	// from automation.
	cmd.Flags().String("account-id", "", "Provider account to bind (optional)")
	return cmd
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve <name>",
		Short: "Create and open a connection in one step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleServe(cmd, args[0])
		},
	}
	addSourceFlags(cmd)
	cmd.Flags().String("provider", "cloudflare", "Provider to use")
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
}

func newForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forward",
		Short: "Manage local port forwards",
		Long: "Create and manage local port forward connections. A forward relays traffic\n" +
			"between a local listening port and a remote host:port without publishing\nanything publicly.",
	}
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a local port forward connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleForwardCreate(cmd, args[0])
		},
	}
	createCmd.Flags().Int("local-port", 0, "Local port to listen on (0 picks a free port)")
	createCmd.Flags().String("remote-host", "127.0.0.1", "Remote host to forward to")
	createCmd.Flags().Int("remote-port", 0, "Remote port to forward to (required)")
	createCmd.Flags().String("protocol", "tcp", "Forward protocol: tcp or udp is refused; only tcp is supported")
	createCmd.Flags().String("direction", "local", "Forward direction: only local is supported; remote is refused")
	_ = createCmd.Flags().MarkHidden("direction")
	createCmd.Flags().Bool("json", false, "Output JSON")
	cmd.AddCommand(createCmd)
	return cmd
}

func addSourceFlags(cmd *cobra.Command) {
	cmd.Flags().String("source", "localhost:8080", "Service address, directory path, command executable, or MCP endpoint")
	cmd.Flags().String("source-type", "existing_service", "Source type: existing_service, directory, command, or mcp_server")
	cmd.Flags().String("source-protocol", "http", "Protocol for existing or command sources")
	cmd.Flags().Int("source-port", 0, "HTTP port for command sources (or existing service when --source omits a port)")
	cmd.Flags().StringArray("source-arg", nil, "Argument for a command source (repeatable)")
	cmd.Flags().String("source-working-dir", "", "Working directory for a command source")
	cmd.Flags().StringToString("source-env", nil, "Environment variable for a command source (KEY=VALUE; repeatable)")
	cmd.Flags().Bool("source-shell", false, "Run command source through sh -c (explicit opt-in)")
	cmd.Flags().String("directory-mode", "read", "Directory mode: read or writes")
	cmd.Flags().Bool("directory-spa", false, "Serve missing directory paths from index.html")
	cmd.Flags().Bool("directory-allow-upload", false, "Allow uploads for a writable directory")
	cmd.Flags().Bool("directory-allow-delete", false, "Allow deletion for a writable directory")
	cmd.Flags().Bool("mcp-command", false, "Treat an MCP source as a Portico-owned command instead of an endpoint")
	// createConnection reads these for existing-service sources. The removal of
	// the legacy cmd/ subtree dropped their registration while the reader
	// survived, so every documented invocation failed with "unknown flag".
	// Defaults preserve the historical behavior: probing on, server-chosen
	// path and timing.
	cmd.Flags().Bool("health-enabled", true, "Probe an existing service's HTTP health")
	cmd.Flags().String("health-path", "", "HTTP health check path (server default when empty)")
	cmd.Flags().String("health-timeout", "", "HTTP health check timeout, e.g. 5s (server default when empty)")
	cmd.Flags().String("health-interval", "", "HTTP health check interval, e.g. 30s (server default when empty)")
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <connection>",
		Short: "Show connection details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleInspect(cmd, args[0])
		},
	}
}

func newOpenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open <connection>",
		Short: "Open a connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleOpen(cmd, args[0])
		},
	}
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
}

func newCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close <connection>",
		Short: "Close a connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleClose(cmd, args[0])
		},
	}
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <connection>",
		Short: "Delete a connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleDelete(cmd, args[0])
		},
	}
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
}

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview and manage plans for connections",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "open <connection>",
		Short: "Preview an open plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handlePlan(cmd, "open", args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "close <connection>",
		Short: "Preview a close plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handlePlan(cmd, "close", args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "repair <connection>",
		Short: "Preview a repair plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handlePlan(cmd, "repair", args[0])
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "delete <connection>",
		Short: "Preview a delete plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handlePlan(cmd, "delete", args[0])
		},
	})

	return cmd
}

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <plan-id>",
		Short: "Apply a plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleApply(cmd, args[0])
		},
	}
}

// newSupportCmd exposes the redacted diagnostic report.
//
// The export existed, was carefully redacted, and had no command. A user asked
// to "attach diagnostics" had no way to produce them.
func newSupportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "support",
		Short: "Produce a diagnostic report for a bug report",
	}
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Write a redacted diagnostic report",
		Long: "Writes a report describing this installation's connections, providers and recent " +
			"operations. It carries no credentials, authorization headers, cookies, private keys " +
			"or command environments.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleSupportExport(cmd)
		},
	}
	exportCmd.Flags().String("output", "", "Write to a file instead of standard output")
	exportCmd.Flags().Bool("force", false, "Replace the output file if it already exists")
	cmd.AddCommand(exportCmd)
	return cmd
}

func newProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage providers",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleProviderList(cmd)
		},
	})
	// Key rotation existed end to end on the supervisor and in the TUI, with
	// no command-line path. A headless host that needs to rotate must not have
	// to open a terminal UI to do it.
	rotateCmd := &cobra.Command{
		Use:   "rotate-key",
		Short: "Rotate the installation encryption key",
		Long: "Re-encrypts every stored provider credential under a new installation-key " +
			"version. The old key files are kept until every secret has been migrated.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleRotateSecretKey(cmd)
		},
	}
	rotateCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
	cmd.AddCommand(rotateCmd)
	loginCmd := &cobra.Command{
		Use:   "login <provider>",
		Short: "Securely configure a provider",
		Long: "Configure any provider that declares a setup flow. The credential is read from a\n" +
			"hidden TTY prompt, stdin (--credential-stdin) or a file descriptor (--credential-fd) —\n" +
			"never a command argument. Non-interactive field values come from --set key=value\n" +
			"pairs named after the provider's declared fields, or from the provider's declared\n" +
			"environment variables.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleProviderLogin(cmd, args[0])
		},
	}
	// The generic, provider-neutral surface. Field values arrive as key=value
	// pairs the supervisor resolves against the provider's own declaration, so
	// no provider's vocabulary leaks into another provider's help.
	loginCmd.Flags().StringArray("set", nil,
		"Set a non-secret field value as key=value (repeatable; keys are the provider's declared field IDs)")
	loginCmd.Flags().String("label", "", "Friendly account label")
	// Secure credential acquisition. The handler already refused a secret on
	// the command line and its error text pointed at these flags; not
	// registering them made the documented automation path an unknown flag.
	// The human default stays the hidden TTY prompt.
	loginCmd.Flags().Bool("credential-stdin", false,
		"Read the credential from stdin (for scripts; the value must not be an argument)")
	loginCmd.Flags().Int("credential-fd", -1,
		"Read the credential from this file descriptor (for scripts)")
	cmd.AddCommand(loginCmd)

	// Removal existed on the supervisor with no way to ask for it. An account
	// added by mistake, or whose token has been revoked, had to be lived with.
	removeCmd := &cobra.Command{
		Use:   "remove-account <provider> <account-id>",
		Short: "Forget a stored provider account",
		Long: "Removes the credential Portico stored for an account. Nothing is deleted at " +
			"the provider. Refused while connections still use the account, naming them.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleProviderRemoveAccount(cmd, args[0], args[1])
		},
	}
	removeCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
	cmd.AddCommand(removeCmd)

	// Reverification existed end to end (supervisor, IPC route, TUI action)
	// with no command-line path. Headless hosts rotate credentials from cron;
	// after doing so they must be able to prove the new secret works without
	// opening a TUI.
	cmd.AddCommand(&cobra.Command{
		Use:   "verify <provider> <account-id>",
		Short: "Re-verify a stored account's credential",
		Long: "Checks the stored credential against the provider without changing it. " +
			"Accounts configured with only a local shape check stay provisional until " +
			"the provider itself confirms the credential.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleProviderVerify(cmd, args[0], args[1])
		},
	})
	return cmd
}

func newDiscoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover",
		Short: "Discover local services",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleDiscover(cmd)
		},
	}
}

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate prerequisites and runtime environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleDoctor(cmd)
		},
	}
	// The socket diagnostic's NextAction names this command exactly. A
	// recovery instruction that does not parse when typed is worse than none:
	// the user is already debugging something broken.
	repair := &cobra.Command{
		Use:   "repair",
		Short: "Run an explicit repair action for a diagnosed problem",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleRepairStaleSocket(cmd)
		},
	}
	repair.Flags().Bool("stale-socket", false,
		"Remove a supervisor socket nothing is listening on")
	repair.Flags().Bool("yes", false, "Skip confirmation prompt")
	cmd.AddCommand(repair)
	return cmd
}

func newRepairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair <connection>",
		Short: "Repair a connection using diagnostics and repair plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleRepair(cmd, args[0])
		},
	}
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
}
