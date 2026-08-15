package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build-time variables injected via -ldflags by the Makefile.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// newVersionCmd reports what is actually running. The equivalent command in
// the legacy cmd package registered itself on a root that main never
// executes, so "portico version" answered "unknown command".
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := map[string]string{
				"version": Version,
				"commit":  Commit,
				"built":   Date,
				"go":      runtime.Version(),
				"os/arch": runtime.GOOS + "/" + runtime.GOARCH,
			}
			if jsonOut(cmd) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				_ = enc.Encode(info)
				return nil
			}
			for _, key := range []string{"version", "commit", "built", "go", "os/arch"} {
				fmt.Fprintf(cmd.OutOrStdout(), "%-8s %s\n", key+":", info[key])
			}
			return nil
		},
	}
}

// jsonOut reports whether --json was passed. The persistent flag lives on the
// root command, so it must be read through the command chain.
func jsonOut(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}
