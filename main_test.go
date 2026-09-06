package main

import (
	"errors"
	"io/fs"
	"net/http"
	"syscall"
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

func TestExitCodeIsStableForTypedSupervisorErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   int
	}{
		{name: "invalid input", status: http.StatusBadRequest, want: exitInvalidInput},
		{name: "not found", status: http.StatusNotFound, want: exitNotFound},
		{name: "conflict", status: http.StatusConflict, want: exitConflict},
		{name: "stale plan", status: http.StatusPreconditionFailed, want: exitStalePlan},
		{name: "auth", status: http.StatusUnauthorized, want: exitAuthRequired},
		{name: "provider unavailable", status: http.StatusBadGateway, want: exitProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(&ipc.APIStatusError{Status: tt.status}); got != tt.want {
				t.Fatalf("exitCode(HTTP %d) = %d, want %d", tt.status, got, tt.want)
			}
		})
	}
}

func TestExitCodeIsStableForTransportErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{name: "connection refused", err: syscall.ECONNREFUSED, want: exitSupervisorUnavailable},
		{name: "missing socket", err: fs.ErrNotExist, want: exitSupervisorUnavailable},
		{name: "generic failure", err: errors.New("broken"), want: exitError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Fatalf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
