package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// ProbeResult is the result of probing a discovered service.
type ProbeResult struct {
	Address    string
	Protocol   string
	Framework  string
	Confidence Confidence
	Evidence   []string
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

// Discover probes discovered listening sockets and classifies them.
func Discover(ctx context.Context, maxConcurrent int) ([]ProbeResult, error) {
	enum := NewSSEnumerator()
	sockets, err := enum.List(ctx)
	if err != nil {
		return nil, err
	}

	if len(sockets) > MaxCandidates {
		sockets = sockets[:MaxCandidates]
	}

	if maxConcurrent <= 0 {
		maxConcurrent = 16
	}

	var results []ProbeResult
	var mu sync.Mutex
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup

	for _, sock := range sockets {
		if sock.Port == 0 {
			continue
		}
		if sock.Address != "127.0.0.1" && sock.Address != "::1" &&
			sock.Address != "0.0.0.0" && sock.Address != "*" && sock.Address != "::" {
			continue
		}

		sock := sock
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			result := ProbeResult{
				Address:  fmt.Sprintf("localhost:%d", sock.Port),
				Protocol: sock.Protocol,
			}

			if sock.Process != "" {
				result.Confidence = ConfidenceLikely
				result.Evidence = append(result.Evidence,
					fmt.Sprintf("Process: %s (PID %d)", sock.Process, sock.PID))
			} else {
				result.Confidence = ConfidencePossible
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}

	wg.Wait()
	return results, nil
}
