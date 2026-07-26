package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/B-A-M-N/portico/cmd"
	"github.com/B-A-M-N/portico/internal/cli"
	"github.com/B-A-M-N/portico/internal/ipc"
)

const exitOK = 0
const exitError = 1
const exitInvalidInput = 2
const exitNotFound = 3
const exitConflict = 4
const exitStalePlan = 5
const exitProviderUnavailable = 6
const exitAuthRequired = 7
const exitSupervisorUnavailable = 8

func main() {
	// Create one signal-aware root context.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	// Create root command with signal-aware context.
	rootCmd := cli.NewCLI()

	// Add legacy command group that wraps old Flare commands.
	rootCmd.AddCommand(cmd.LegacyCmd())

	// Execute with the signal-aware context.
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps errors to stable exit codes for scripting and agents.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return exitError
	}

	// Typed supervisor API errors carry the HTTP status.
	var apiErr *ipc.APIStatusError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return exitInvalidInput
		case http.StatusNotFound:
			return exitNotFound
		case http.StatusConflict:
			return exitConflict
		case http.StatusPreconditionFailed, http.StatusGone:
			return exitStalePlan
		case http.StatusUnauthorized, http.StatusForbidden:
			return exitAuthRequired
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return exitProviderUnavailable
		default:
			return exitError
		}
	}

	// Supervisor unreachable: socket missing or connection refused.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return exitSupervisorUnavailable
	}

	return exitError
}
