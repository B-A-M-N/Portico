package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ListeningSocket describes a discovered listening socket.
type ListeningSocket struct {
	Protocol string
	Address  string
	Port     int
	PID      int
	Process  string
	Command  string
}

// ListenerEnumerator discovers listening sockets on the local machine.
type ListenerEnumerator interface {
	List(ctx context.Context) ([]ListeningSocket, error)
}

// SSEnumerator uses `ss -H -lntup` to discover listening sockets.
type SSEnumerator struct{}

// NewSSEnumerator creates a new ss-based enumerator.
func NewSSEnumerator() *SSEnumerator {
	return &SSEnumerator{}
}

// List runs `ss -H -lntup` and parses the output.
func (e *SSEnumerator) List(ctx context.Context) ([]ListeningSocket, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ss", "-H", "-lntup")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ss enumeration: %w", err)
	}

	return parseSSOutput(string(output))
}

func parseSSOutput(output string) ([]ListeningSocket, error) {
	var sockets []ListeningSocket
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		sock, err := parseSSLine(line)
		if err != nil {
			continue
		}
		sockets = append(sockets, sock)
	}

	return sockets, scanner.Err()
}

func parseSSLine(line string) (ListeningSocket, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return ListeningSocket{}, fmt.Errorf("malformed ss line: %s", line)
	}

	protocol := fields[0]
	localAddr := fields[4]

	addr, portStr, err := net.SplitHostPort(localAddr)
	if err != nil {
		lastColon := strings.LastIndex(localAddr, ":")
		if lastColon < 0 {
			return ListeningSocket{}, fmt.Errorf("cannot parse address: %s", localAddr)
		}
		addr = localAddr[:lastColon]
		portStr = localAddr[lastColon+1:]
	}

	port, _ := strconv.Atoi(strings.TrimSpace(portStr))

	sock := ListeningSocket{
		Protocol: protocol,
		Address:  addr,
		Port:     port,
	}

	if len(fields) >= 6 {
		procInfo := fields[len(fields)-1]
		procInfo = strings.TrimPrefix(procInfo, "users:((")
		procInfo = strings.TrimSuffix(procInfo, "))")

		parts := strings.SplitN(procInfo, "\",", 2)
		if len(parts) == 2 {
			sock.Process = strings.TrimPrefix(parts[0], "\"")
			pidPart := parts[1]
			pidPart = strings.TrimPrefix(pidPart, "pid=")
			pidPart = strings.Split(pidPart, ",")[0]
			if pid, err := strconv.Atoi(strings.TrimSpace(pidPart)); err == nil {
				sock.PID = pid
			}
		}
	}

	return sock, nil
}

// Confidence describes how confident we are about a discovery.
type Confidence string

const (
	ConfidenceVeryLikely Confidence = "very_likely"
	ConfidenceLikely     Confidence = "likely"
	ConfidencePossible   Confidence = "possible"
)

// MaxCandidates is the max number of discovery candidates.
const MaxCandidates = 128
