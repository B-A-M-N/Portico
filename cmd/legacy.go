package cmd

import (
	"github.com/spf13/cobra"
)

// LegacyCmd returns the legacy flare-cli command subtree.
// These commands are preserved for backwards compatibility.
func LegacyCmd() *cobra.Command {
	legacy := &cobra.Command{
		Use:    "legacy",
		Short:  "Preserved flare-cli commands for backwards compatibility",
		Long:   "Access the original flare-cli functionality. This is temporary and will be removed once the new Portico commands reach feature parity.",
		Hidden: true,
	}

	legacy.AddCommand(
		authCmd,
		closeCmd,
		configCmd,
		doctorCmd,
		initCmd,
		listCmd,
		logsCmd,
		serveCmd,
		statusCmd,
		updateCmd,
		versionCmd,
	)

	return legacy
}
