//go:build linux

package origin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
)

// procStat holds parsed fields from /proc/[pid]/stat.
type procStat struct {
	state     byte
	ppid      int
	pgrp      int
	session   int
	startTime uint64
}

// parseProcStat reads /proc/[pid]/stat and returns parsed fields.
// After the comm field (which may contain spaces and parentheses), the
// remaining fields are indexed 0-based as:
//
//	0=state, 1=ppid, 2=pgrp, 3=session, 4=tty_nr, 5=tpgid,
//	6=flags, 7=minflt, 8=cminflt, 9=majflt, 10=cmajflt,
//	11=utime, 12=stime, ... 19=starttime (field 22 in kernel)
func parseProcStat(pid int) (*procStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	s := string(data)
	// /proc/[pid]/stat is "pid (comm) state ..."; comm may contain spaces or
	// parentheses, so find the LAST ')' and parse the fields after it.
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 >= len(s) {
		return nil, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 20 {
		return nil, fmt.Errorf("not enough fields in /proc/%d/stat", pid)
	}
	parsedPpid, err := parseIntField(fields[1])
	if err != nil {
		return nil, err
	}
	parsedPgrp, err := parseIntField(fields[2])
	if err != nil {
		return nil, err
	}
	parsedSession, err := parseIntField(fields[3])
	if err != nil {
		return nil, err
	}
	parsedStartTime, err := parseUintField(fields[19])
	if err != nil {
		return nil, err
	}
	return &procStat{
		state:     fields[0][0],
		ppid:      parsedPpid,
		pgrp:      parsedPgrp,
		session:   parsedSession,
		startTime: parsedStartTime,
	}, nil
}

func parseIntField(s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse int field %q: %w", s, err)
	}
	return v, nil
}

func parseUintField(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse uint field %q: %w", s, err)
	}
	return v, nil
}

// ProcessGroupOf returns the process group ID of a PID by reading /proc.
func ProcessGroupOf(pid int) (int, error) {
	ps, err := parseProcStat(pid)
	if err != nil {
		return 0, err
	}
	return ps.pgrp, nil
}

// StartTimeOf returns the start time field of /proc/[pid]/stat.
func StartTimeOf(pid int) (uint64, error) {
	ps, err := parseProcStat(pid)
	if err != nil {
		return 0, err
	}
	return ps.startTime, nil
}

// readCommandHash returns a SHA-256 hash of the process's /proc/[pid]/cmdline.
func readCommandHash(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty cmdline for pid %d", pid)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// allowlistedEnv returns a controlled environment for an owned origin. The
// supervisor's full environment must never be inherited because it may contain
// provider credentials, keyring variables, or unrelated developer secrets.
// `extra` is the origin's environment specification (literal values resolved
// by the supervisor). Explicit command values always override inherited ones.
func allowlistedEnv(extra map[string]string) []string {
	// Only inherit a narrow, well-known set of safe runtime variables.
	// Proxy variables are intentionally excluded — proxy URLs commonly
	// embed credentials that must never leak into child processes.
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"TZ": true, "TMPDIR": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
	}
	// Start with a map of allowed inherited values
	result := make(map[string]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if allowed[name] {
			result[name] = kv
		}
	}
	// Explicit command values override inherited values
	for k, v := range extra {
		if allowed[k] {
			result[k] = k + "=" + v
		}
	}
	// Emit in deterministic order
	out := make([]string, 0, len(result))
	for _, k := range sortedKeys(result) {
		out = append(out, result[k])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ErrIdentityMismatch is returned when a process identity cannot be verified.
var ErrIdentityMismatch = fmt.Errorf("origin: identity mismatch")

// verifyOriginIdentity checks that the live process still matches the stored
// identity: PID, start time, executable, command hash, and process group.
// It must be called before any signal is sent.
func verifyOriginIdentity(stored core.ProcessIdentity, storedPgid int) error {
	if stored.PID <= 0 {
		return fmt.Errorf("%w: zero PID in stored identity", ErrIdentityMismatch)
	}
	if stored.StartTime == 0 || stored.ExecutablePath == "" || stored.CommandHash == "" {
		return fmt.Errorf("%w: incomplete identity", ErrIdentityMismatch)
	}

	// Check start time to detect PID reuse.
	currentStartTime, err := StartTimeOf(stored.PID)
	if err != nil {
		return fmt.Errorf("%w: process %d stat failed: %v", ErrIdentityMismatch, stored.PID, err)
	}
	if currentStartTime != stored.StartTime {
		return fmt.Errorf("%w: PID %d reused: start time mismatch", ErrIdentityMismatch, stored.PID)
	}

	// Check executable path.
	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", stored.PID))
	if err != nil {
		return fmt.Errorf("%w: process %d executable read failed: %v", ErrIdentityMismatch, stored.PID, err)
	}
	storedExe := strings.TrimSuffix(stored.ExecutablePath, " (deleted)")
	currentExe := strings.TrimSuffix(exePath, " (deleted)")
	if currentExe != storedExe {
		return fmt.Errorf("%w: PID %d executable mismatch: stored %s, current %s", ErrIdentityMismatch, stored.PID, storedExe, currentExe)
	}

	// Check command hash.
	currentHash, err := readCommandHash(stored.PID)
	if err != nil {
		return fmt.Errorf("%w: cannot verify command hash for PID %d: %v", ErrIdentityMismatch, stored.PID, err)
	}
	if currentHash != stored.CommandHash {
		return fmt.Errorf("%w: PID %d command hash mismatch", ErrIdentityMismatch, stored.PID)
	}

	// Check process group.
	currentPgid, err := ProcessGroupOf(stored.PID)
	if err != nil {
		return fmt.Errorf("%w: cannot read pgrp for PID %d: %v", ErrIdentityMismatch, stored.PID, err)
	}
	if currentPgid != storedPgid {
		return fmt.Errorf("%w: PID %d pgrp mismatch: stored %d, current %d", ErrIdentityMismatch, stored.PID, storedPgid, currentPgid)
	}

	return nil
}
