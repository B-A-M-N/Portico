package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is the IPC client connecting to the supervisor via Unix socket.
type Client struct {
	socketPath string
	httpClient http.Client
}

// NewClient creates a new IPC client.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", socketPath, 5*time.Second)
				},
			},
		},
	}
}

// doRequest performs an HTTP request against the supervisor.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	return c.httpClient.Do(req)
}

// APIStatusError is a typed error carrying the HTTP status and the decoded
// APIError body returned by the supervisor.
type APIStatusError struct {
	Status  int
	Code    string
	Message string
	// The supervisor already returns a structured error carrying an
	// explanation, concrete recovery actions and technical detail. Flattening
	// it into a single string discarded everything a caller needs to present
	// an intervention rather than a raw failure.
	Explanation      string
	RecoveryActions  []RecoveryAction
	TechnicalDetails string
	Retryable        bool
}

// Error implements the error interface.
func (e *APIStatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// checkResponse checks that the response status is successful. On failure it
// decodes the structured APIError body into an APIStatusError, falling back
// to the raw body if decoding fails.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	statusErr := &APIStatusError{Status: resp.StatusCode}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr == nil && len(body) > 0 {
		var apiErr APIError
		if err := json.Unmarshal(body, &apiErr); err == nil && (apiErr.Code != "" || apiErr.Summary != "") {
			statusErr.Code = apiErr.Code
			statusErr.Message = apiErr.Summary
			statusErr.Explanation = apiErr.Explanation
			statusErr.RecoveryActions = apiErr.RecoveryActions
			statusErr.TechnicalDetails = apiErr.TechnicalDetails
			statusErr.Retryable = apiErr.Retryable
		} else {
			statusErr.Message = strings.TrimSpace(string(body))
		}
	}
	if statusErr.Message == "" {
		statusErr.Message = resp.Status
	}
	return statusErr
}

// Health checks if the supervisor is available.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.doRequest(ctx, "GET", "/v1/health", nil)
	if err != nil {
		return fmt.Errorf("supervisor unavailable: %w", err)
	}
	defer resp.Body.Close()

	return checkResponse(resp)
}

// GetSnapshot fetches the full state snapshot.
func (c *Client) GetSnapshot(ctx context.Context) (*SnapshotDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/snapshot", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var snap SnapshotDTO
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ListConnections lists all connections.
func (c *Client) ListConnections(ctx context.Context) ([]ConnectionDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/connections", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var conns []ConnectionDTO
	if err := json.NewDecoder(resp.Body).Decode(&conns); err != nil {
		return nil, err
	}
	return conns, nil
}

// GetConnection gets a single connection.
func (c *Client) GetConnection(ctx context.Context, id string) (*ConnectionDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/connections/"+id, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var conn ConnectionDTO
	if err := json.NewDecoder(resp.Body).Decode(&conn); err != nil {
		return nil, err
	}
	return &conn, nil
}

// GetConnectionDetail fetches the authoritative detail view of a connection:
// its desired spec, observed endpoints, route segments, managed provider
// resources with their external IDs, connector processes and open findings.
//
// The inspect screen must render from this rather than from the list summary,
// which carries only enough state to draw a row.
func (c *Client) GetConnectionDetail(ctx context.Context, id string) (*ConnectionDetailDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/connections/"+id+"/detail", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var detail ConnectionDetailDTO
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// CloneConnection copies a connection's desired state into a new connection.
func (c *Client) CloneConnection(ctx context.Context, id string, req CloneConnectionRequest) (*ConnectionDTO, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+id+"/clone", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var conn ConnectionDTO
	if err := json.NewDecoder(resp.Body).Decode(&conn); err != nil {
		return nil, err
	}
	return &conn, nil
}

// ConnectionLogs fetches a bounded, redacted tail of a connection's connector
// output.
func (c *Client) ConnectionLogs(ctx context.Context, id string, lines int) (*ConnectionLogsDTO, error) {
	path := "/v1/connections/" + id + "/logs"
	if lines > 0 {
		path += "?lines=" + strconv.Itoa(lines)
	}
	resp, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var logs ConnectionLogsDTO
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		return nil, err
	}
	return &logs, nil
}

// CreateConnection creates a new connection.
func (c *Client) CreateConnection(ctx context.Context, req CreateConnectionRequest) (*ConnectionDTO, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/v1/connections", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var conn ConnectionDTO
	if err := json.NewDecoder(resp.Body).Decode(&conn); err != nil {
		return nil, err
	}
	return &conn, nil
}

// PlanOpen creates an open plan.
func (c *Client) PlanOpen(ctx context.Context, connID string) (*PlanDTO, error) {
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+connID+"/plan/open", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var plan PlanDTO
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// PlanClose creates a close plan.
func (c *Client) PlanClose(ctx context.Context, connID string) (*PlanDTO, error) {
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+connID+"/plan/close", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var plan PlanDTO
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// PlanEdit previews an edit as a change plan without applying it.
func (c *Client) PlanEdit(ctx context.Context, connID string, req UpdateConnectionRequest) (*PlanDTO, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+connID+"/plan/edit", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var plan PlanDTO
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// PlanRepair creates a repair plan.
func (c *Client) PlanRepair(ctx context.Context, connID string) (*PlanDTO, error) {
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+connID+"/plan/repair", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var plan PlanDTO
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// PlanDelete creates a delete plan.
func (c *Client) PlanDelete(ctx context.Context, connID string) (*PlanDTO, error) {
	resp, err := c.doRequest(ctx, "POST", "/v1/connections/"+connID+"/plan/delete", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var plan PlanDTO
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// StopSupervisor requests the supervisor to stop gracefully.
func (c *Client) StopSupervisor(ctx context.Context) error {
	resp, err := c.doRequest(ctx, "POST", "/v1/supervisor/stop", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkResponse(resp)
}

// ApplyPlan applies a plan and returns the operation.
func (c *Client) ApplyPlan(ctx context.Context, planID string) (*OperationDTO, error) {
	return c.ApplyPlanWithIdempotency(ctx, planID, "")
}

// ApplyPlanWithIdempotency applies a plan with an optional idempotency key.
// If the same key was used for a previous apply, the cached operation is returned.
func (c *Client) ApplyPlanWithIdempotency(ctx context.Context, planID string, idempotencyKey string) (*OperationDTO, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "http://unix/v1/plans/"+planID+"/apply", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var op OperationDTO
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return nil, err
	}
	return &op, nil
}

// GetOperation returns the current state of an asynchronous operation.
func (c *Client) GetOperation(ctx context.Context, operationID string) (*OperationDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/operations/"+operationID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	var op OperationDTO
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return nil, err
	}
	return &op, nil
}

// GetOperationHistory returns the operation history.
func (c *Client) GetOperationHistory(ctx context.Context) (*OperationHistoryDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/operations", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	var history OperationHistoryDTO
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return nil, err
	}
	return &history, nil
}

// ListProviders lists providers.
func (c *Client) ListProviders(ctx context.Context) ([]ProviderDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/providers", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var providers []ProviderDTO
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		return nil, err
	}
	return providers, nil
}

// ConfigureProviderAccount persists a provider account through the local
// supervisor. The response never contains the supplied credential.
func (c *Client) ConfigureProviderAccount(ctx context.Context, providerID string, req ConfigureProviderAccountRequest) (*ConfigureProviderAccountResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal provider account configuration: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/v1/providers/"+url.PathEscape(providerID)+"/accounts", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	var result ConfigureProviderAccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --------------- Event stream ---------------

// EventStream connects to the SSE event stream.
type EventStream struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

// ConnectEventStream connects to the SSE event stream.
func (c *Client) ConnectEventStream(ctx context.Context, lastSeq int64) (*EventStream, error) {
	// Check context before dialing
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{
		Timeout: 5 * time.Second,
	}).DialContext(dialCtx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("event stream dial: %w", err)
	}

	// Build proper HTTP request
	path := "/v1/events"
	if lastSeq > 0 {
		path += fmt.Sprintf("?last_seq=%d", lastSeq)
	}

	// Write proper HTTP request
	reqLine := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: unix\r\n", path)
	if lastSeq > 0 {
		reqLine += fmt.Sprintf("Last-Event-ID: %d\r\n", lastSeq)
	}
	reqLine += "\r\n"

	if _, err := conn.Write([]byte(reqLine)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("event stream request: %w", err)
	}

	reader := bufio.NewReader(conn)

	// Read HTTP response status line
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("event stream status: %w", err)
	}
	statusLine = strings.TrimSpace(statusLine)
	if !strings.HasPrefix(statusLine, "HTTP/1.1 2") {
		conn.Close()
		return nil, fmt.Errorf("event stream rejected: %s", statusLine)
	}

	// Read headers
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("event stream header: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break // End of headers
		}
	}

	return &EventStream{
		conn:   conn,
		reader: reader,
	}, nil
}

// Next blocks until the next event is received.
func (es *EventStream) Next() (*EventDTO, error) {
	es.mu.Lock()
	defer es.mu.Unlock()

	var evt EventDTO
	var dataBuf strings.Builder

	for {
		line, err := es.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("event read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "id: "):
			evtID := strings.TrimPrefix(line, "id: ")
			if id, err := strconv.ParseInt(evtID, 10, 64); err == nil {
				evt.Sequence = id
			}

		case strings.HasPrefix(line, "event: "):
			evt.Type = strings.TrimPrefix(line, "event: ")

		case strings.HasPrefix(line, "data: "):
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))

		case line == "":
			if dataBuf.Len() > 0 {
				if err := json.Unmarshal([]byte(dataBuf.String()), &evt); err != nil {
					return nil, fmt.Errorf("event parse: %w", err)
				}
				return &evt, nil
			}
			dataBuf.Reset()

		case line == ":":
			// Heartbeat comment, continue
		}
	}
}

// Close closes the event stream connection.
func (es *EventStream) Close() error {
	return es.conn.Close()
}

// CloseWithContext closes the event stream connection and cancels the context.
func (es *EventStream) CloseWithContext() error {
	return es.conn.Close()
}

// Discovery performs service discovery.
func (c *Client) Discovery(ctx context.Context) (*DiscoveryDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/discovery", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var result DiscoveryDTO
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RefreshDiscovery forces a fresh service discovery scan.
func (c *Client) RefreshDiscovery(ctx context.Context) (*DiscoveryDTO, error) {
	resp, err := c.doRequest(ctx, "POST", "/v1/discovery", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var result DiscoveryDTO
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Diagnostics returns diagnostic findings for a connection.
func (c *Client) Diagnostics(ctx context.Context, connID string) ([]DiagnosticDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/diagnostics/"+connID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var result []DiagnosticDTO
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// Snapshot returns the current snapshot of all connections.
func (c *Client) Snapshot(ctx context.Context) (*SnapshotDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/snapshot", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var result SnapshotDTO
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CheckAvailability checks if the supervisor socket exists and responds.
func CheckAvailability(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

// SupportExport fetches a redacted diagnostic report suitable for a bug report.
func (c *Client) SupportExport(ctx context.Context) (*SupportExportDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/support/export", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var export SupportExportDTO
	if err := json.NewDecoder(resp.Body).Decode(&export); err != nil {
		return nil, err
	}
	return &export, nil
}

// Readiness fetches the aggregated setup view.
func (c *Client) Readiness(ctx context.Context) (*ReadinessDTO, error) {
	resp, err := c.doRequest(ctx, "GET", "/v1/readiness", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var readiness ReadinessDTO
	if err := json.NewDecoder(resp.Body).Decode(&readiness); err != nil {
		return nil, err
	}
	return &readiness, nil
}
