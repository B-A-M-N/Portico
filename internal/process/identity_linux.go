package process

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// VerifyIdentity checks that the process identity stored in a ManagedProcess
// still matches the actual OS process (SPEC §11.1).
// Never trust a PID alone.
func VerifyIdentity(mp *ManagedProcess) error {
	if mp == nil {
		return fmt.Errorf("no managed process")
	}
	return verifyIdentityFields(mp.Identity)
}

// verifyIdentityFields checks every identity field (start time, executable
// path, command hash) against the live process via /proc. Adopting a process
// requires all four fields to be populated — partial identities are rejected
// as ErrIdentityPartial so they cannot be confused with a verified match.
// All other failures — including an unreadable or missing process — are
// wrapped in ErrIdentityMismatch so callers must NOT signal the PID.
func verifyIdentityFields(ident core.ProcessIdentity) error {
	if !ident.Complete() {
		return fmt.Errorf("%w: incomplete identity (pid=%d, start_time=%d, exe=%q, hash=%q)",
			ErrIdentityPartial, ident.PID, ident.StartTime, ident.ExecutablePath, ident.CommandHash)
	}
	pid := ident.PID
	// Verify PID exists and check start time to detect PID reuse
	procStat, err := parseProcStat(pid)
	if err != nil {
		return fmt.Errorf("%w: process %d stat failed: %v", ErrIdentityMismatch, pid, err)
	}
	if procStat.startTime != ident.StartTime {
		return fmt.Errorf("%w: PID %d reused: start time mismatch (stored %d, current %d)",
			ErrIdentityMismatch, pid, ident.StartTime, procStat.startTime)
	}

	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return fmt.Errorf("%w: process %d executable read failed: %v", ErrIdentityMismatch, pid, err)
	}
	// Normalize paths for comparison (strip " (deleted)" suffix)
	storedExe := strings.TrimSuffix(ident.ExecutablePath, " (deleted)")
	currentExe := strings.TrimSuffix(exePath, " (deleted)")
	if currentExe != storedExe {
		return fmt.Errorf("%w: PID %d executable mismatch: stored %s, current %s",
			ErrIdentityMismatch, pid, ident.ExecutablePath, exePath)
	}

	currentHash, err := computeCommandHash(pid)
	if err != nil {
		return fmt.Errorf("%w: cannot verify command hash: %v", ErrIdentityMismatch, err)
	}
	if currentHash != ident.CommandHash {
		return fmt.Errorf("%w: PID %d command hash mismatch", ErrIdentityMismatch, pid)
	}
	return nil
}

// procStat holds parsed /proc/[pid]/stat fields
type procStat struct {
	comm      string
	startTime uint64
}

// parseProcStat reads and parses /proc/[pid]/stat to get process start time.
func parseProcStat(pid int) (*procStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}

	// Find the last ')' which ends the comm field (field 1)
	// The comm field is enclosed in parentheses and may contain spaces
	idx := strings.LastIndex(string(data), ")")
	if idx < 0 {
		return nil, fmt.Errorf("invalid stat format")
	}

	comm := string(data[1:idx]) // without parentheses

	// Fields after comm: pid (2), state (3), ppid (4), pgrp (5),
	// session (6), tty_nr (7), tpgid (8), flags (9), minflt (10),
	// cminflt (11), majflt (12), cmajflt (13), utime (14), stime (15),
	// ...
	// Field 22 (index 21) is starttime (clock ticks since boot)
	fields := strings.Fields(string(data[idx+2:]))
	if len(fields) < 21 {
		return nil, fmt.Errorf("not enough fields in stat")
	}

	startTime, err := strconv.ParseUint(fields[19], 10, 64) // field 22 (0-indexed: 21)
	if err != nil {
		return nil, fmt.Errorf("parse start time: %w", err)
	}

	return &procStat{
		comm:      comm,
		startTime: startTime,
	}, nil
}

// computeCommandHash computes a SHA-256 hash of the process command line.
func computeCommandHash(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}

	// cmdline is null-separated
	args := strings.Split(string(data), "\x00")
	// Remove trailing empty string
	if len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}

	// Compute hash
	h := sha256.New()
	for _, arg := range args {
		io.WriteString(h, arg)
		io.WriteString(h, "\x00")
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// IsProcessRunning checks if a PID is running on Linux.
func IsProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// ProcessIdentityFromProc reads process identity from /proc/[pid] for validation.
func ProcessIdentityFromProc(spec core.ProcessSpec) (core.ProcessIdentity, error) {
	// Start with the spec values as a base
	ident := core.ProcessIdentity{
		ExecutablePath: spec.Executable,
	}

	// For a process we're about to start, we don't have a PID yet
	// The caller must set PID after start
	// Start time will be captured after the process starts

	// Compute command hash from spec
	ident.CommandHash = computeSpecHash(spec)

	return ident, nil
}

// computeSpecHash computes a hash from the process spec.
func computeSpecHash(spec core.ProcessSpec) string {
	h := sha256.New()
	io.WriteString(h, spec.Executable)
	for _, arg := range spec.Args {
		io.WriteString(h, "\x00")
		io.WriteString(h, arg)
	}
	if spec.Dir != "" {
		io.WriteString(h, "\x00")
		io.WriteString(h, spec.Dir)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// captureProcessIdentity reads the full identity of a running process once.
// Callers that have just launched a process should use captureIdentityStable.
func captureProcessIdentity(pid int) (core.ProcessIdentity, error) {
	procStat, err := parseProcStat(pid)
	if err != nil {
		return core.ProcessIdentity{}, err
	}

	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		exePath = ""
	}

	cmdHash, _ := computeCommandHash(pid)

	return core.ProcessIdentity{
		PID:            pid,
		StartTime:      procStat.startTime,
		ExecutablePath: exePath,
		CommandHash:    cmdHash,
	}, nil
}

// CaptureProcessIdentity captures the full identity of a running process.
// It waits for a stable post-exec /proc view so callers adopting a freshly
// launched process cannot accidentally record the parent/pre-exec command.
func CaptureProcessIdentity(pid int) (core.ProcessIdentity, error) {
	return captureIdentityStable(pid, 500*time.Millisecond)
}

// captureIdentityStable captures the identity of a just-started process.
// Immediately after fork, /proc/<pid>/cmdline can transiently be empty (or
// reflect the pre-exec image) until execve fully completes. Capturing at
// that moment would poison the stored CommandHash/ExecutablePath and cause
// false identity mismatches on every later verification. Retry briefly
// until the cmdline is populated; fall back to a plain capture when the
// deadline expires (e.g. the process already exited and became a zombie,
// whose cmdline is permanently empty).
func captureIdentityStable(pid int, deadline time.Duration) (core.ProcessIdentity, error) {
	end := time.Now().Add(deadline)
	// execve happens in the child after Start returns. Give it a short window
	// before inspecting /proc; merely seeing a non-empty cmdline is not enough
	// because it can still describe the inherited pre-exec image.
	notBefore := time.Now().Add(10 * time.Millisecond)
	for {
		if now := time.Now(); now.Before(notBefore) {
			time.Sleep(notBefore.Sub(now))
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && len(data) > 0 {
			return captureProcessIdentity(pid)
		}
		if err != nil || time.Now().After(end) {
			return captureProcessIdentity(pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
