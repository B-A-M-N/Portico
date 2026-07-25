package cli

import (
	"github.com/spf13/cobra"

	"github.com/paoloanzn/portico/internal/app"
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
	root.AddCommand(newInspectCmd())
	root.AddCommand(newOpenCmd())
	root.AddCommand(newCloseCmd())
	root.AddCommand(newDeleteCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newApplyCmd())
	root.AddCommand(newProviderCmd())
	root.AddCommand(newDiscoverCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newRepairCmd())
	root.AddCommand(newLogsCmd("Show supervisor logs (alias for 'supervisor logs')"))

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
	cmd.Flags().String("source", "localhost:8080", "Local service address to expose")
	cmd.Flags().String("provider", "cloudflare", "Provider to use")
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
	cmd.Flags().String("source", "localhost:8080", "Local service address to expose")
	cmd.Flags().String("provider", "cloudflare", "Provider to use")
	cmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	return cmd
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
	cmd.AddCommand(&cobra.Command{
		Use:   "login <provider>",
		Short: "Authenticate with a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleProviderLogin(cmd, args[0])
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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate prerequisites and runtime environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleDoctor(cmd)
		},
	}
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
