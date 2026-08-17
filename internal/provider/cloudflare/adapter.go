package cloudflare

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go"

	"github.com/B-A-M-N/portico/internal/access"
	"github.com/B-A-M-N/portico/internal/core"
	cfdns "github.com/B-A-M-N/portico/internal/dns"
	"github.com/B-A-M-N/portico/internal/tunnel"
)

// Provider implements core.Provider for Cloudflare.
type Provider struct {
	mu        sync.RWMutex
	apiClient *cf.API
	accountID string
	zoneID    string
	tunnels   tunnel.Manager
	dns       cfdns.Manager
	access    access.Manager
	mode      ProviderMode

	cloudflaredBin string
	logDir         string
	connectorProc  core.ConnectorProcessService
	credStore      CredentialStore

	connections map[core.ConnectionID]*cfConnection
}

// ProviderMode distinguishes the operational mode of a Cloudflare provider.
type ProviderMode string

const (
	ProviderModeFull      ProviderMode = "full"
	ProviderModeQuickOnly ProviderMode = "quick_only"
)

type cfConnection struct {
	tunnel    *tunnel.Info
	dnsID     string
	accessID  string
	policyID  string
	hostname  string
	publicURL string
	// connectorPID tracks the PID of the running connector process.
	connectorPID int
	// connectorStarted indicates whether a connector process is active.
	connectorStarted bool
	ownership        core.ResourceOwnership
}

// safeShortID returns a safe short identifier for logging and file naming.
// If the input is shorter than the requested length, it returns the full string.
func safeShortID(id string, length int) string {
	if len(id) <= length {
		return id
	}
	return id[:length]
}

// New creates a new Cloudflare provider.
func New(apiToken, accountID, zoneID, cloudflaredBin, logDir string, connectorProc core.ConnectorProcessService) (*Provider, error) {
	return newWithOptions(apiToken, accountID, zoneID, cloudflaredBin, logDir, connectorProc)
}

// newWithOptions builds the provider with extra client options.
//
// The options exist so a contract test can point the client at a local server
// and assert what Portico actually sends. Without a seam the adapter could only
// be tested through its own helpers, which asserts that the code agrees with
// itself rather than that the requests are the ones Cloudflare documents.
func newWithOptions(apiToken, accountID, zoneID, cloudflaredBin, logDir string,
	connectorProc core.ConnectorProcessService, opts ...cf.Option) (*Provider, error) {
	if connectorProc == nil {
		return nil, fmt.Errorf("cloudflare: connector process service is required")
	}
	api, err := cf.NewWithAPIToken(apiToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("cloudflare client: %w", err)
	}

	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}

	p := &Provider{
		apiClient:      api,
		accountID:      accountID,
		zoneID:         zoneID,
		tunnels:        tunnel.NewAPIManager(api),
		dns:            cfdns.NewAPIManager(api),
		access:         access.NewAPIManager(api, accountID),
		mode:           ProviderModeFull,
		cloudflaredBin: cloudflaredBin,
		logDir:         logDir,
		connectorProc:  connectorProc,
		connections:    make(map[core.ConnectionID]*cfConnection),
	}

	// Remove credential files left behind by a previous supervisor crash.
	p.sweepStaleCredentials()

	return p, nil
}

// sweepStaleCredentials removes credential files left by a previous supervisor crash.
// Called once during provider initialization.
func (p *Provider) sweepStaleCredentials() {
	if err := prepareCredentialDir(p.credentialDir()); err != nil {
		slog.Warn("cloudflare: failed to prepare credential directory", "err", err)
		return
	}
	if err := sweepStaleCredentialFiles(p.credentialDir(), time.Now()); err != nil {
		slog.Warn("cloudflare: failed to sweep stale credential files", "err", err)
	}
}

// credentialDir returns the directory used for scoped token credential files.
func (p *Provider) credentialDir() string {
	// Credential files are account-scoped so two provider instances cannot
	// sweep or write one another's temporary tokens. Hashing avoids treating a
	// provider-supplied account identifier as a filesystem path.
	account := sha256.Sum256([]byte(p.accountID))
	return filepath.Join(p.logDir, "..", "credentials", fmt.Sprintf("%x", account[:8]))
}

// NewQuickTunnel creates a Cloudflare provider that supports Quick Tunnels only.
// No API token is required — only cloudflared binary access.
func NewQuickTunnel(cloudflaredBin, logDir string, connectorProc core.ConnectorProcessService) (*Provider, error) {
	if connectorProc == nil {
		return nil, fmt.Errorf("cloudflare: connector process service is required for quick tunnels")
	}
	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}

	return &Provider{
		apiClient:      nil,
		accountID:      "",
		zoneID:         "",
		tunnels:        nil,
		dns:            nil,
		access:         nil,
		mode:           ProviderModeQuickOnly,
		cloudflaredBin: cloudflaredBin,
		logDir:         logDir,
		connectorProc:  connectorProc,
		connections:    make(map[core.ConnectionID]*cfConnection),
	}, nil
}

// SetConnectorProcessService allows setting or overriding the connector
// process service after construction. Used for testing and supervisor wiring.
func (p *Provider) SetConnectorProcessService(svc core.ConnectorProcessService) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connectorProc = svc
}

// CredentialStore stores and retrieves encrypted tunnel tokens.
// Tokens are persisted so permanent connectors can survive supervisor restart.
type CredentialStore interface {
	SaveTunnelCredential(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string, token []byte) error
	LoadTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) (token string, err error)
	DeleteTunnelCredentialExact(ctx context.Context, connID core.ConnectionID, providerID core.ProviderID, tunnelID string) error
}

// SetCredentialStore sets the credential store for durable tunnel token storage.
func (p *Provider) SetCredentialStore(store CredentialStore) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.credStore = store
}

// RehydrateConnection reconstructs a connection's tunnel state from stored credentials.
// Returns true if credentials were found and the connection state was restored.
// This enables permanent connectors to survive supervisor restart.
func (p *Provider) RehydrateConnection(ctx context.Context, connID core.ConnectionID, tunnelID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	conn, exists := p.connections[connID]
	if !exists {
		conn = &cfConnection{ownership: core.OwnershipManaged}
		p.connections[connID] = conn
	}

	if p.credStore == nil {
		return false
	}

	if tunnelID == "" {
		// A credential must always be selected by the exact tunnel it belongs
		// to. Picking whichever row happens to be newest can start a connector
		// with a replacement or stale token after recovery.
		return false
	}
	token, err := p.credStore.LoadTunnelCredentialExact(ctx, connID, "cloudflare", tunnelID)
	if err != nil || token == "" {
		return false
	}

	conn.tunnel = &tunnel.Info{
		TunnelID:   tunnelID,
		TunnelName: fmt.Sprintf("portico-%s", safeShortID(string(connID), 8)),
		Token:      token,
	}
	slog.Info("rehydrated tunnel credential from store", "connection", connID, "tunnel", tunnelID)
	return true
}

// Identity returns the provider identity.
func (p *Provider) Identity() core.ProviderIdentity {
	return core.ProviderIdentity{
		ID:          "cloudflare",
		Name:        "cloudflare",
		DisplayName: "Cloudflare",
	}
}

// ProviderAccountID returns the exact Cloudflare account configured for this
// adapter instance. It is intentionally empty for Quick Tunnels, which do not
// operate against an authenticated account.
func (p *Provider) ProviderAccountID() core.ProviderAccountID {
	return core.ProviderAccountID(p.accountID)
}

// Capabilities returns Cloudflare's capability set.
// Quick-only providers advertise only capabilities that can execute
// without an authenticated Cloudflare API account.
func (p *Provider) Capabilities(ctx context.Context) (core.Capabilities, error) {
	if p.mode == ProviderModeQuickOnly {
		return core.Capabilities{
			TemporaryAddresses: core.CapabilitySupport{
				Supported: true,
				Stability: core.StabilityBeta,
				Notes:     []string{"Quick Tunnel — development-oriented; rejects SSE"},
			},
			CustomHostnames: core.CapabilitySupport{
				Supported: false,
				Stability: core.StabilityStable,
				Notes:     []string{"Quick Tunnels do not support custom hostnames"},
			},
			ManagedDNS: core.CapabilitySupport{
				Supported: false,
				Stability: core.StabilityStable,
				Notes:     []string{"Managed DNS requires an authenticated Cloudflare account"},
			},
			BuiltInProtection: []core.ProtectionCapability{
				{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
			},
			Protocols: map[core.Protocol]core.ProtocolCapability{
				core.ProtocolHTTP:  {Supported: true, Public: true},
				core.ProtocolHTTPS: {Supported: true, Public: true},
			},
			Telemetry: core.TelemetryCapability{
				Supported:     false,
				RequestCounts: false,
				Stability:     core.StabilityStable,
			},
			Constraints: []core.CapabilityConstraint{
				{
					Code:          "quick_tunnel_no_sse",
					MCPTransports: []core.MCPTransport{core.MCPTransportSSE},
					ExposureModes: []core.ExposureMode{core.ExposureTemporary},
					Supported:     false,
					Message:       "SSE transport is not supported with Quick Tunnels (temporary exposure)",
				},
				{
					Code:                     "quick_tunnel_no_custom_hostname",
					ExposureModes:            []core.ExposureMode{core.ExposureTemporary},
					RequiresRequestedAddress: true,
					Supported:                false,
					Message:                  "Quick Tunnels do not support custom hostnames",
				},
			},
		}, nil
	}

	return core.Capabilities{
		TemporaryAddresses: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityBeta,
			Notes:     []string{"Quick Tunnel — development-oriented; rejects SSE"},
		},
		CustomHostnames: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
			Requires:  []core.Requirement{{Resource: "Cloudflare account with domain"}},
		},
		ManagedDNS: core.CapabilitySupport{
			Supported: true,
			Stability: core.StabilityStable,
		},
		BuiltInProtection: []core.ProtectionCapability{
			{Kind: core.ProtectionNone, Supported: true, Stability: core.StabilityStable},
			{Kind: core.ProtectionEmailOTP, Supported: true, Stability: core.StabilityStable},
			// Identity Provider and Service Token are not implemented:
			// policy generation only creates email/email-domain rules.
			{Kind: core.ProtectionIdentity, Supported: false, Stability: core.StabilityExperimental},
			{Kind: core.ProtectionServiceToken, Supported: false, Stability: core.StabilityExperimental},
		},
		Protocols: map[core.Protocol]core.ProtocolCapability{
			core.ProtocolHTTP:  {Supported: true, Public: true},
			core.ProtocolHTTPS: {Supported: true, Public: true},
		},
		Telemetry: core.TelemetryCapability{
			// Metrics collection is not wired into the supervisor yet. Do not
			// advertise traffic data simply because Cloudflare can provide it.
			Supported:     false,
			RequestCounts: false,
			Stability:     core.StabilityExperimental,
		},
		Constraints: []core.CapabilityConstraint{
			{
				Code:          "quick_tunnel_no_sse",
				MCPTransports: []core.MCPTransport{core.MCPTransportSSE},
				ExposureModes: []core.ExposureMode{core.ExposureTemporary},
				Supported:     false,
				Message:       "SSE transport is not supported with Quick Tunnels (temporary exposure)",
			},
			{
				Code:          "quick_tunnel_no_custom_hostname",
				ExposureModes: []core.ExposureMode{core.ExposureTemporary},
				Supported:     false,
				Message:       "Quick Tunnels do not support custom hostnames",
			},
			{
				Code:          "permanent_requires_zone",
				ExposureModes: []core.ExposureMode{core.ExposurePermanent},
				Supported:     true,
				Requirement:   &core.Requirement{Resource: "Cloudflare zone/domain"},
				Message:       "Permanent exposure requires a Cloudflare zone ID",
			},
		},
	}, nil
}

// Authenticate validates the API token against the Cloudflare API.
// Quick-only providers do not support API authentication.
func (p *Provider) Authenticate(ctx context.Context, req core.AuthRequest) error {
	if p.mode == ProviderModeQuickOnly {
		return &core.PorticoError{
			Code:      "PTO-CF-AUTH-NOT-REQUIRED",
			Message:   "Quick Tunnels do not require Cloudflare API authentication",
			Retryable: false,
		}
	}
	_, _, err := p.apiClient.Accounts(ctx, cf.AccountsListParams{})
	if err != nil {
		return fmt.Errorf("cloudflare auth: %w", err)
	}
	return nil
}

// Plan creates an operation plan for the desired connection state.
func (p *Provider) Plan(ctx context.Context, desired core.DesiredConnection) (*core.OperationPlan, error) {
	if desired.Profile == nil {
		return nil, fmt.Errorf("cloudflare: profile required")
	}

	// Defensive kind check: this provider only supports service_exposure.
	// Without it, a non-service-exposure profile would be planned with empty
	// source/exposure/protection (the zero values returned by the backward-
	// compat accessors), causing silent misconfiguration or — worse — an
	// unprotected public service.
	if desired.Profile.Kind != "" && desired.Profile.Kind != core.ConnectionServiceExposure {
		return nil, fmt.Errorf("cloudflare: connection kind %q is not supported, only service_exposure", desired.Profile.Kind)
	}

	// Closing only stops a connector and must remain possible when the local
	// source is no longer available.
	if desired.Profile.Desired != core.DesiredClosed && desired.Origin == nil {
		return nil, fmt.Errorf("cloudflare: resolved origin required")
	}

	profile := desired.Profile
	originURL := ""
	if desired.Origin != nil {
		originURL = desired.Origin.URL
	}
	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    profile.ID,
		ProfileRevision: profile.Revision,
		Provider:        "cloudflare",
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	switch profile.Desired {
	case core.DesiredOpen:
		plan.Intent = core.IntentOpen

		if profile.GetExposure().Mode == core.ExposureTemporary {
			// Quick Tunnel — no named tunnel, no DNS, no Access
			// Reject SSE
			source := profile.GetSource()
			if source.MCP != nil && source.MCP.Transport == core.MCPTransportSSE {
				return nil, fmt.Errorf("SSE transport is not supported with Quick Tunnels (temporary exposure)")
			}
			// Reject custom hostname requests
			if profile.GetExposure().RequestedAddress != "" {
				return nil, fmt.Errorf("quick tunnels do not support custom hostnames")
			}
			// Reject unsupported protection for temporary mode.
			//
			// The test is positive. Written as "kind is not none" it reads the
			// zero value — which GetProtection returns for a profile with no
			// protection spec at all — as protection being configured, and
			// refuses to open a connection that asked for nothing. The same
			// mistake elsewhere granted work rather than refusing it; here it
			// would have made a valid connection unopenable.
			if profile.IsProtected() {
				return nil, fmt.Errorf("quick tunnels do not support access protection")
			}

			plan.Steps = append(plan.Steps,
				core.PlanStep{ID: "cf-verify-origin", Kind: core.StepVerifyOrigin, Summary: "Verify local service is reachable",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_origin",
						Parameters: map[string]string{"origin_url": originURL}}},
				core.PlanStep{ID: "cf-connector", Kind: core.StepStartConnector, Summary: "Start cloudflared quick tunnel",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "start_connector",
						Parameters: map[string]string{
							"mode":       "quick",
							"origin_url": originURL,
						}}},
				core.PlanStep{ID: "cf-verify", Kind: core.StepVerifyEndpoint, Summary: "Verify public URL reachable",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_endpoint",
						Parameters: map[string]string{"origin_url": originURL}}},
			)
			plan.Expected.State = core.RuntimeOpen
			plan.Expected.PublicAddress = "temporary (assigned by Cloudflare)"

		} else if profile.GetExposure().Mode == core.ExposurePermanent {
			// Permanent exposure — named tunnel + DNS + optional Access
			if profile.GetExposure().RequestedAddress == "" {
				return nil, fmt.Errorf("permanent exposure requires a requested hostname")
			}
			if p.zoneID == "" {
				return nil, fmt.Errorf("permanent exposure requires a configured Cloudflare zone")
			}

			// P0 #8: Validate protection configuration for protected connections.
			// Identity Provider, Service Token, and Private Network protection
			// are not implemented — only email/email-domain Access rules are
			// generated. Refuse rather than falsely advertise protection.
			prot := profile.GetProtection()
			switch prot.Kind {
			case core.ProtectionIdentity, core.ProtectionServiceToken, core.ProtectionPrivateNet:
				return nil, fmt.Errorf("protection %s is not supported by the Cloudflare provider yet", prot.Kind)
			}
			if prot.Kind != core.ProtectionNone {
				if len(prot.AllowedEmails) == 0 && len(prot.AllowedDomains) == 0 {
					return nil, fmt.Errorf("protection %s requires at least one allowed email or domain", prot.Kind)
				}
			}

			tunnelName := fmt.Sprintf("portico-%s", safeShortID(string(profile.ID), 8))
			hostname := profile.GetExposure().RequestedAddress

			plan.Steps = append(plan.Steps,
				core.PlanStep{ID: "cf-validate", Kind: core.StepValidateAccount, Summary: "Validate Cloudflare account and zone",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "validate_account"}},
				// The origin is probed before the first provider mutation.
				// Tunnels, DNS records and Access policies are externally
				// visible and survive a failed operation, so none of them may
				// be created for an origin that is not answering.
				core.PlanStep{ID: "cf-verify-origin", Kind: core.StepVerifyOrigin, Summary: "Verify local service is reachable",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_origin",
						Parameters: map[string]string{"origin_url": originURL}}},
				core.PlanStep{ID: "cf-tunnel", Kind: core.StepCreateTunnel, Summary: fmt.Sprintf("Create named tunnel %q", tunnelName),
					Technical:   core.TechnicalOperation{Provider: "cloudflare", Type: "create_tunnel", Parameters: map[string]string{"name": tunnelName}},
					Destructive: false, Irreversible: false,
					Ownership: core.OwnershipManaged,
					Compensation: &core.CompensationStep{
						ID:   "comp-cf-tunnel-delete",
						Kind: core.StepDeleteTunnel,
						Technical: core.TechnicalOperation{
							Provider: "cloudflare",
							Type:     "delete_tunnel",
						},
					}},
				core.PlanStep{ID: "cf-route", Kind: core.StepConfigureRoute, Summary: fmt.Sprintf("Configure tunnel route for %s", hostname),
					Technical:   core.TechnicalOperation{Provider: "cloudflare", Type: "configure_route", Parameters: map[string]string{"hostname": hostname, "origin_url": originURL}},
					Destructive: false, Irreversible: false,
					Ownership: core.OwnershipManaged,
					Compensation: &core.CompensationStep{
						ID:   "comp-cf-route-delete",
						Kind: core.StepDeleteDNSRecord,
						Technical: core.TechnicalOperation{
							Provider: "cloudflare",
							Type:     "delete_dns",
						},
					}},
				core.PlanStep{ID: "cf-dns", Kind: core.StepCreateDNSRecord, Summary: fmt.Sprintf("Create DNS CNAME for %s", hostname),
					Technical:   core.TechnicalOperation{Provider: "cloudflare", Type: "create_dns", Parameters: map[string]string{"hostname": hostname}},
					Destructive: false, Irreversible: false,
					Ownership: core.OwnershipManaged,
					Compensation: &core.CompensationStep{
						ID:   "comp-cf-dns-delete",
						Kind: core.StepDeleteDNSRecord,
						Technical: core.TechnicalOperation{
							Provider: "cloudflare",
							Type:     "delete_dns",
						},
					}},
			)

			// Add Access steps based on protection kind
			if profile.GetProtection().Kind != core.ProtectionNone {
				authMode := protectionToAuthMode(profile.GetProtection().Kind)
				plan.Steps = append(plan.Steps,
					core.PlanStep{ID: "cf-access-app", Kind: core.StepCreateAccessApp, Summary: "Create Access application and policy",
						Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "create_access_app", Parameters: map[string]string{
							"hostname":         hostname,
							"auth_mode":        authMode,
							"allowed_emails":   strings.Join(profile.GetProtection().AllowedEmails, ","),
							"allowed_domains":  strings.Join(profile.GetProtection().AllowedDomains, ","),
							"session_duration": profile.GetProtection().SessionTTL.String(),
						}},
						Destructive: false, Irreversible: false,
						Ownership: core.OwnershipManaged,
						Compensation: &core.CompensationStep{
							ID:   "comp-cf-access-cleanup",
							Kind: core.StepDeleteAccessApp,
							Technical: core.TechnicalOperation{
								Provider: "cloudflare",
								Type:     "delete_access",
							},
						}},
				)
			}

			plan.Steps = append(plan.Steps,
				core.PlanStep{ID: "cf-connector", Kind: core.StepStartConnector, Summary: "Start cloudflared connector",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "start_connector",
						Parameters: map[string]string{"mode": "permanent", "origin_url": originURL}}},
				core.PlanStep{ID: "cf-verify-connector", Kind: core.StepVerifyConnector, Summary: "Verify connector process",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_connector"}},
				core.PlanStep{ID: "cf-verify", Kind: core.StepVerifyEndpoint, Summary: "Verify endpoint reachable",
					Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_endpoint",
						Parameters: map[string]string{"origin_url": originURL}}},
			)
			plan.Expected.State = core.RuntimeOpen
			plan.Expected.PublicAddress = hostname
		}

	case core.DesiredClosed:
		plan.Intent = core.IntentClose
		plan.Steps = []core.PlanStep{
			{ID: "cf-stop", Kind: core.StepStopConnector, Summary: "Stop cloudflared connector",
				Technical:   core.TechnicalOperation{Provider: "cloudflare", Type: "stop_connector"},
				Destructive: false, Irreversible: false},
			{ID: "cf-verify-stopped", Kind: core.StepVerifyConnector, Summary: "Verify connector stopped",
				Technical: core.TechnicalOperation{Provider: "cloudflare", Type: "verify_connector", Parameters: map[string]string{"expected_state": "stopped"}}},
		}
		plan.Expected.State = core.RuntimeClosed
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, fmt.Errorf("cloudflare fingerprint: %w", err)
	}
	return plan, nil
}

// protectionToAuthMode maps ProtectionKind to Access auth mode string.
func protectionToAuthMode(kind core.ProtectionKind) string {
	switch kind {
	case core.ProtectionNone:
		return "none"
	case core.ProtectionEmailOTP:
		return "otp"
	case core.ProtectionIdentity:
		return "identity_provider"
	case core.ProtectionServiceToken:
		return "service_token"
	case core.ProtectionPrivateNet:
		return "private_network"
	default:
		return "otp"
	}
}

// accessAuthMode accepts both the Cloudflare-specific mode used by normal
// provider plans and the provider-neutral protection kind used by targeted
// reconciliation plans. The latter keeps desired-state interpretation out of
// the supervisor while preserving this adapter as the Cloudflare mapping
// boundary.
func accessAuthMode(params map[string]string) string {
	if mode := params["auth_mode"]; mode != "" {
		return mode
	}
	if kind := core.ProtectionKind(params["protection_kind"]); kind != "" {
		return protectionToAuthMode(kind)
	}
	return "otp"
}

// verifyLocalOrigin probes the origin URL over HTTP to confirm the local
// service is reachable before routing traffic through the tunnel.
func (p *Provider) verifyLocalOrigin(ctx context.Context, originURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("origin returned status %d", resp.StatusCode)
	}
	return nil
}

// verifyDNSResolution performs a DNS lookup for the hostname to confirm
// that the record has propagated and the hostname resolves.
func (p *Provider) verifyDNSResolution(ctx context.Context, hostname string) error {
	resolver := &net.Resolver{}
	addrs, err := resolver.LookupHost(ctx, hostname)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fmt.Errorf("no addresses resolved for %s", hostname)
	}
	return nil
}

// verifyPublicEndpoint performs an HTTP probe against the public endpoint to
// confirm the tunnel is serving traffic end-to-end.
// Acceptable status codes: 2xx (success), 3xx (redirect), 401/403 (auth required = reachable).
func (p *Provider) verifyPublicEndpoint(ctx context.Context, publicURL string) error {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Accept 2xx (success), 3xx (redirect), 401/403 (auth required = reachable)
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil
	}
	return fmt.Errorf("public endpoint returned status %d", resp.StatusCode)
}

// Observe returns the observed connection state using resources known
// to adapter memory. It preserves the core.Provider interface; callers
// that hold persisted provider resources (e.g. the supervisor after a
// restart, when adapter memory is empty) should use
// ObserveWithResources so observation stays authoritative.
func (p *Provider) Observe(ctx context.Context, id core.ConnectionID) (*core.ObservedConnection, error) {
	return p.ObserveWithResources(ctx, id, p.memoryResources(id))
}

// memoryResources reconstructs the resource list for a connection from
// adapter memory. Ownership is intentionally left unset — observation
// never assigns ownership.
func (p *Provider) memoryResources(id core.ConnectionID) []core.ProviderResource {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.connections[id]
	if !ok {
		return nil
	}
	var resources []core.ProviderResource
	if conn.tunnel != nil && conn.tunnel.TunnelID != "" {
		resources = append(resources, core.ProviderResource{
			ConnectionID: id, ProviderID: "cloudflare",
			Type: core.ResourceTunnel, ExternalID: conn.tunnel.TunnelID,
		})
	}
	if conn.dnsID != "" {
		resources = append(resources, core.ProviderResource{
			ConnectionID: id, ProviderID: "cloudflare",
			Type: core.ResourceDNSRecord, ExternalID: conn.dnsID,
		})
	}
	if conn.accessID != "" {
		resources = append(resources, core.ProviderResource{
			ConnectionID: id, ProviderID: "cloudflare",
			Type: core.ResourceAccessApp, ExternalID: conn.accessID,
		})
	}
	if conn.policyID != "" {
		resources = append(resources, core.ProviderResource{
			ConnectionID: id, ProviderID: "cloudflare",
			Type: core.ResourceAccessPolicy, ExternalID: conn.policyID,
			Metadata: map[string]string{"app_id": conn.accessID},
		})
	}
	return resources
}

// ObserveWithResources observes provider state authoritatively: for
// each persisted resource it queries the Cloudflare API by the
// resource's exact external ID (never list-by-name) and classifies the
// result as present, missing (authoritative 404), unauthorized
// (401/403), rate_limited (429), or transient (5xx, network). A
// transient error is never treated as missing, and observation never
// assigns ownership.
func (p *Provider) ObserveWithResources(ctx context.Context, id core.ConnectionID, resources []core.ProviderResource) (*core.ObservedConnection, error) {
	obs := &core.ObservedConnection{
		ConnectionID: id,
		ProviderID:   "cloudflare",
		Connector:    &core.ObservedConnector{Status: "stopped"},
	}

	// Observe connector process state directly from the process
	// service so it remains accurate even when adapter memory is
	// empty after a supervisor restart.
	if handle, ok := p.connectorProc.Observe(id); ok && handle.PID != 0 {
		if err := verifyProcessIdentity(handle); err == nil {
			obs.Connector.Status = "running"
			obs.Connector.PID = handle.PID
			obs.Connector.StartTime = handle.Identity.StartTime
			obs.Connector.ExecutablePath = handle.Identity.ExecutablePath
			obs.Connector.CommandHash = handle.Identity.CommandHash
		} else {
			obs.Connector.Status = fmt.Sprintf("error: %s", err.Error())
		}
	}

	for _, res := range resources {
		if res.ExternalID == "" {
			continue
		}
		status := core.ObservedResourceStatus{Type: res.Type, ExternalID: res.ExternalID}

		switch res.Type {
		case core.ResourceTunnel:
			if p.tunnels == nil {
				status.Status, status.Detail = core.ObservationTransient, "tunnel API unavailable (quick-only provider)"
				break
			}
			state, err := p.tunnels.Get(ctx, p.accountID, res.ExternalID)
			switch {
			case err != nil:
				status.Status, status.Detail = classifyObservationError(err)
			case state == nil:
				// Manager reports authoritative 404 as nil, nil.
				status.Status = core.ObservationMissing
			default:
				status.Status = core.ObservationPresent
				obs.Tunnel = &core.ObservedTunnel{ID: state.ID, Name: state.Name, State: state.Status}
				// Hydrate only the exact observed identifier/name. This is not
				// ownership adoption; it lets a narrowly scoped follow-up (such
				// as DNS recreation) operate after a supervisor restart without
				// inventing or listing provider resources.
				p.hydrateObservedTunnel(id, state.ID, state.Name)
			}

		case core.ResourceDNSRecord:
			if p.dns == nil {
				status.Status, status.Detail = core.ObservationTransient, "DNS API unavailable (quick-only provider)"
				break
			}
			state, err := p.dns.GetRecord(ctx, p.zoneID, res.ExternalID)
			switch {
			case err != nil:
				status.Status, status.Detail = classifyObservationError(err)
			case state == nil:
				status.Status = core.ObservationMissing
			default:
				status.Status = core.ObservationPresent
				obs.DNSRecords = append(obs.DNSRecords, core.ObservedDNSRecord{
					ID: state.ID, Name: state.Name, Type: state.Type, Target: state.Content,
				})
				p.hydrateObservedDNS(id, state.ID, state.Name)
			}

		case core.ResourceAccessApp:
			if p.access == nil {
				status.Status, status.Detail = core.ObservationTransient, "Access API unavailable (quick-only provider)"
				break
			}
			state, err := p.access.GetApp(ctx, p.accountID, res.ExternalID)
			switch {
			case err != nil:
				status.Status, status.Detail = classifyObservationError(err)
			case state == nil:
				status.Status = core.ObservationMissing
			default:
				status.Status = core.ObservationPresent
				obs.AccessApps = append(obs.AccessApps, core.ObservedAccessApp{
					ID: state.ID, Name: state.Name, Domain: state.Domain, AuthMode: state.AuthMode,
				})
			}

		case core.ResourceAccessPolicy:
			getter, ok := p.access.(access.PolicyGetter)
			if p.access == nil || !ok {
				status.Status, status.Detail = core.ObservationTransient, "Access policy lookup unavailable"
				break
			}
			state, err := getter.GetPolicy(ctx, p.accountID, res.Metadata["app_id"], res.ExternalID)
			switch {
			case err != nil:
				status.Status, status.Detail = classifyObservationError(err)
			case state == nil:
				status.Status = core.ObservationMissing
			default:
				status.Status = core.ObservationPresent
				obs.AccessPolicies = append(obs.AccessPolicies, core.ObservedAccessPolicy{
					ID: state.ID, AppID: res.Metadata["app_id"], Decision: state.Decision,
					AllowedEmails: state.AllowedEmails, AllowedDomains: state.AllowedDomains,
					SessionDuration: state.SessionDuration,
				})
			}

		default:
			// Connector state is observed above; other resource types
			// have no remote representation to query.
			continue
		}

		obs.ResourceStatuses = append(obs.ResourceStatuses, status)
	}

	// Note: Observation NEVER assigns ownership. Ownership comes from durable
	// provenance (created by Portico, explicitly adopted, or externally observed).
	// This observation only reports state for drift detection.

	return obs, nil
}

func (p *Provider) hydrateObservedTunnel(connectionID core.ConnectionID, tunnelID, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	conn := p.connections[connectionID]
	if conn == nil {
		conn = &cfConnection{ownership: core.OwnershipExternal}
		p.connections[connectionID] = conn
	}
	conn.tunnel = &tunnel.Info{TunnelID: tunnelID, TunnelName: name}
}

func (p *Provider) hydrateObservedDNS(connectionID core.ConnectionID, dnsID, hostname string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	conn := p.connections[connectionID]
	if conn == nil {
		conn = &cfConnection{ownership: core.OwnershipExternal}
		p.connections[connectionID] = conn
	}
	conn.dnsID = dnsID
	if conn.hostname == "" {
		conn.hostname = hostname
	}
}

// classifyObservationError maps a Cloudflare API error to an
// observation status. Only an authoritative 404 is classified as
// missing; everything unrecognized is transient so that flaky lookups
// are never mistaken for deleted resources.
func classifyObservationError(err error) (core.ObservationStatus, string) {
	var cfErr *cf.Error
	if errors.As(err, &cfErr) {
		switch {
		case cfErr.StatusCode == http.StatusNotFound || cfErr.StatusCode == http.StatusGone:
			return core.ObservationMissing, err.Error()
		case cfErr.StatusCode == http.StatusUnauthorized || cfErr.StatusCode == http.StatusForbidden:
			return core.ObservationUnauthorized, err.Error()
		case cfErr.StatusCode == http.StatusTooManyRequests || cfErr.ClientRateLimited():
			return core.ObservationRateLimited, err.Error()
		}
	}
	// 5xx, network failures, timeouts, cancellations, and anything
	// unclassified are transient — never treated as missing.
	return core.ObservationTransient, err.Error()
}

// connectorReadyTimeout bounds how long launchConnector waits for a
// newly started permanent connector to be observed running before the
// token credential file is removed.
const connectorReadyTimeout = 10 * time.Second

// connectorReadyGrace is how long the connector process must stay alive
// before it is considered to have consumed the token credential file.
const connectorReadyGrace = 2 * time.Second

// launchConnector starts a cloudflared connector for the connection and
// records the resulting PID in the in-memory connection state.
//
// For quick tunnels the process is started directly. For permanent
// tunnels the tunnel token is written to a scoped credential file
// (0600 in a 0700 directory) passed via --token-file; the file is
// removed on every path — start failure, readiness failure, panic,
// cancellation, and success (after the connector has consumed it).
// The token path and contents are never logged.
func (p *Provider) launchConnector(ctx context.Context, connectionID core.ConnectionID, conn *cfConnection, mode, originURL string) (core.ConnectorHandle, error) {
	spec := core.ProcessSpec{
		Executable: p.cloudflaredBin,
		Restart:    core.RestartAlways,
		StdoutPath: filepath.Join(p.logDir, fmt.Sprintf("connector-%s.out", safeShortID(string(connectionID), 8))),
		StderrPath: filepath.Join(p.logDir, fmt.Sprintf("connector-%s.err", safeShortID(string(connectionID), 8))),
	}

	var handle core.ConnectorHandle
	var err error
	if mode == "quick" {
		if originURL == "" {
			originURL = "http://localhost:8080"
		}
		spec.Args = []string{"tunnel", "--no-autoupdate", "--url", originURL}
		handle, err = p.connectorProc.Start(ctx, core.ProcessConfig{ConnectionID: connectionID, Spec: spec})
	} else {
		p.mu.RLock()
		tun := conn.tunnel
		p.mu.RUnlock()
		if tun == nil || tun.Token == "" {
			return core.ConnectorHandle{}, fmt.Errorf("no tunnel token available for connector")
		}
		handle, err = withCredentialFileUntilReady(p.credentialDir(), []byte(tun.Token),
			func(path string) (core.ConnectorHandle, error) {
				s := spec
				s.Args = []string{"tunnel", "--no-autoupdate", "run", "--token-file", path}
				// Use minimal env + explicit override. Never inherit the
				// supervisor's full environment, which may contain unrelated
				// provider credentials.
				s.Env = []string{
					"PATH=" + os.Getenv("PATH"),
					"HOME=" + os.Getenv("HOME"),
					"LANG=C.UTF-8",
					"TZ=UTC",
					"TUNNEL_TOKEN_FILE=" + path,
				}
				return p.connectorProc.Start(ctx, core.ProcessConfig{ConnectionID: connectionID, Spec: s})
			},
			func(h core.ConnectorHandle) error {
				return p.waitConnectorReady(ctx, connectionID, connectorReadyTimeout)
			},
		)
		if err != nil && handle.PID != 0 {
			// Readiness failed after the process started — best-effort
			// stop so a half-started connector is not leaked.
			_ = p.connectorProc.Stop(connectionID, 2*time.Second)
		}
	}
	if err != nil {
		return core.ConnectorHandle{}, err
	}

	p.mu.Lock()
	conn.connectorPID = handle.PID
	conn.connectorStarted = true
	p.mu.Unlock()
	return handle, nil
}

// waitConnectorReady polls the connector process status until it has
// been observed running for a short grace period (token consumed) or
// the timeout expires. It returns an error if the process exits during
// the wait or is never observed running.
func (p *Provider) waitConnectorReady(ctx context.Context, connectionID core.ConnectionID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var aliveSince time.Time
	for {
		handle, ok := p.connectorProc.Observe(connectionID)
		alive := ok && handle.PID != 0 && verifyProcessIdentity(handle) == nil
		switch {
		case alive && aliveSince.IsZero():
			aliveSince = time.Now()
		case alive && time.Since(aliveSince) >= connectorReadyGrace:
			return nil
		case !alive && !aliveSince.IsZero():
			return fmt.Errorf("connector process exited before becoming ready")
		}
		if time.Now().After(deadline) {
			if !aliveSince.IsZero() {
				// Still running at the deadline — treat as ready.
				return nil
			}
			return fmt.Errorf("connector process not running after %v", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// discoverQuickTunnelAddress reads the connector stdout log file
// and extracts the trycloudflare.com URL assigned by cloudflared.
// It polls the file until the URL appears or timeout expires.
// Returns the public address string or an error if the process
// exits before the URL is discovered.
func (p *Provider) discoverQuickTunnelAddress(connectionID core.ConnectionID, stdoutPath string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Read the stdout log file for the trycloudflare URL.
		data, err := os.ReadFile(stdoutPath)
		if err != nil {
			// Log file may not exist yet or be empty; keep polling.
			time.Sleep(100 * time.Millisecond)
			continue
		}

		addr, err := tunnel.ExtractQuickTunnelURL(string(data))
		if err == nil {
			return addr, nil
		}

		// If the process has exited and no URL was found, fail.
		// Use the process manager for liveness — the provider does not
		// own process identity.
		handle, tracked := p.connectorProc.Observe(connectionID)
		if !tracked || handle.PID == 0 {
			return "", fmt.Errorf("connector process exited before tunnel address was assigned")
		}

		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("quick tunnel address not discovered within %v", timeout)
}

// verifyProcessIdentity checks that a connector handle from the process
// manager represents a valid, tracked process. Process liveness and
// identity belong to process.Manager; the provider trusts the handle
// returned by Observe.
func verifyProcessIdentity(handle core.ConnectorHandle) error {
	if handle.PID <= 0 {
		return fmt.Errorf("invalid PID %d", handle.PID)
	}
	return nil
}

// ExecuteStep executes a single plan step for Cloudflare.
func (p *Provider) ExecuteStep(ctx context.Context, connectionID core.ConnectionID, step core.PlanStep) (core.StepResult, error) {
	p.mu.Lock()
	conn, exists := p.connections[connectionID]
	if !exists {
		conn = &cfConnection{ownership: core.OwnershipManaged}
		p.connections[connectionID] = conn
	}
	p.mu.Unlock()

	switch step.Kind {
	case core.StepValidateAccount:
		_, _, err := p.apiClient.Accounts(ctx, cf.AccountsListParams{})
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("account validation failed: %w", err)}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepVerifyOrigin:
		// Runs before any provider mutation. Failing here means no tunnel, DNS
		// record or Access policy is created, so nothing externally visible is
		// left behind pointing at a service that is not answering.
		originURL := step.Technical.Parameters["origin_url"]
		if originURL == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("cannot verify origin: plan did not carry a resolved origin URL")}, nil
		}
		if err := p.verifyLocalOrigin(ctx, originURL); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false,
				Error: fmt.Errorf("local service at %s is not reachable: %w", originURL, err)}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepCreateTunnel:
		tunnelName := step.Technical.Parameters["name"]
		if tunnelName == "" {
			tunnelName = fmt.Sprintf("portico-%s", safeShortID(string(connectionID), 8))
		}
		info, err := p.tunnels.Create(ctx, p.accountID, tunnelName)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.tunnel = info
		p.mu.Unlock()
		return core.StepResult{
			StepID:    step.ID,
			Succeeded: true,
			Resources: []core.ProviderResource{{
				ConnectionID: connectionID,
				Type:         core.ResourceTunnel,
				ExternalID:   info.TunnelID,
				ProviderID:   "cloudflare",
				Ownership:    core.OwnershipManaged,
			}},
			CredentialMutations: []core.CredentialMutation{{
				TunnelID: info.TunnelID,
				Secret:   []byte(info.Token),
			}},
		}, nil

	case core.StepConfigureRoute:
		hostname := step.Technical.Parameters["hostname"]
		originURL := step.Technical.Parameters["origin_url"]
		p.mu.RLock()
		tun := conn.tunnel
		p.mu.RUnlock()
		if tun == nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("cannot configure route: no tunnel created yet")}, nil
		}
		if hostname == "" || originURL == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("cannot configure route: missing hostname or origin_url")}, nil
		}
		err := p.tunnels.ConfigureIngress(ctx, p.accountID, tun.TunnelID, hostname, originURL)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		if conn.hostname == "" {
			conn.hostname = hostname
		}
		p.mu.Unlock()
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepCreateDNSRecord:
		hostname := step.Technical.Parameters["hostname"]
		if hostname == "" {
			return core.StepResult{StepID: step.ID, Succeeded: true}, nil
		}
		// Reconciliation materializes the durable tunnel ID in the step so a
		// DNS-only repair remains valid after adapter process memory has been
		// reconstructed. Normal open plans keep using the tunnel just created
		// in this operation.
		tunnelID := step.Technical.Parameters["tunnel_id"]
		if tunnelID == "" {
			p.mu.RLock()
			tun := conn.tunnel
			p.mu.RUnlock()
			if tun != nil {
				tunnelID = tun.TunnelID
			}
		}
		if tunnelID == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("no tunnel available")}, nil
		}
		dnsID, err := p.dns.CreateCNAME(ctx, p.zoneID, hostname, tunnelID)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.dnsID = dnsID
		p.mu.Unlock()
		return core.StepResult{
			StepID:    step.ID,
			Succeeded: true,
			Resources: []core.ProviderResource{{
				ConnectionID: connectionID,
				Type:         core.ResourceDNSRecord,
				ExternalID:   dnsID,
				ProviderID:   "cloudflare",
				Ownership:    core.OwnershipManaged,
			}},
		}, nil

	case core.StepCreateAccessApp:
		hn := step.Technical.Parameters["hostname"]
		if hn == "" {
			p.mu.RLock()
			hn = conn.hostname
			p.mu.RUnlock()
		}
		if hn == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access application requires a hostname")}, nil
		}
		authMode := accessAuthMode(step.Technical.Parameters)
		if authMode != "none" && authMode != "private_network" {
			emails := step.Technical.Parameters["allowed_emails"]
			domains := step.Technical.Parameters["allowed_domains"]
			if emails == "" && domains == "" {
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy requires at least one allowed email or domain for %s protection", authMode)}, nil
			}
		}
		sessionDuration := ""
		if rawDur, ok := step.Technical.Parameters["session_duration"]; ok && rawDur != "" {
			sessionDuration = rawDur
		}
		appInfo, err := p.access.CreateApp(ctx, p.accountID, hn, access.Policy{
			AuthMode:        authMode,
			AllowedEmails:   strings.Split(step.Technical.Parameters["allowed_emails"], ","),
			AllowedDomains:  strings.Split(step.Technical.Parameters["allowed_domains"], ","),
			SessionDuration: sessionDuration,
		})
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.accessID = appInfo.AppID
		conn.policyID = appInfo.PolicyID
		if conn.hostname == "" {
			conn.hostname = hn
		}
		p.mu.Unlock()
		return core.StepResult{
			StepID:    step.ID,
			Succeeded: true,
			Resources: []core.ProviderResource{
				{ConnectionID: connectionID, Type: core.ResourceAccessApp, ExternalID: appInfo.AppID, ProviderID: "cloudflare", Ownership: core.OwnershipManaged},
				{ConnectionID: connectionID, Type: core.ResourceAccessPolicy, ExternalID: appInfo.PolicyID, ProviderID: "cloudflare", Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": appInfo.AppID}},
			},
		}, nil

	case core.StepCreateAccessPolicy:
		appID := step.Technical.Parameters["app_id"]
		if appID == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy creation requires exact application ID")}, nil
		}
		creator, ok := p.access.(access.PolicyCreator)
		if p.access == nil || !ok {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy creation is unavailable")}, nil
		}
		policy := access.Policy{
			AuthMode:        accessAuthMode(step.Technical.Parameters),
			AllowedEmails:   splitNonEmpty(step.Technical.Parameters["allowed_emails"]),
			AllowedDomains:  splitNonEmpty(step.Technical.Parameters["allowed_domains"]),
			SessionDuration: step.Technical.Parameters["session_duration"],
		}
		if policy.AuthMode != "none" && policy.AuthMode != "private_network" && len(policy.AllowedEmails) == 0 && len(policy.AllowedDomains) == 0 {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy requires at least one allowed email or domain")}, nil
		}
		policyID, err := creator.CreatePolicy(ctx, p.accountID, appID, policy)
		if err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.accessID = appID
		conn.policyID = policyID
		p.mu.Unlock()
		return core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{{
			ConnectionID: connectionID, ProviderID: "cloudflare", Type: core.ResourceAccessPolicy,
			ExternalID: policyID, Ownership: core.OwnershipManaged, Metadata: map[string]string{"app_id": appID},
		}}}, nil

	case core.StepUpdateAccessApp:
		appID := step.Technical.ResourceID
		hostname := step.Technical.Parameters["hostname"]
		if appID == "" || hostname == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access application update requires exact application ID and hostname")}, nil
		}
		updater, ok := p.access.(access.AppUpdater)
		if p.access == nil || !ok {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access application update is unavailable")}, nil
		}
		if err := updater.UpdateApp(ctx, p.accountID, appID, hostname); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.accessID = appID
		if conn.hostname == "" {
			conn.hostname = hostname
		}
		p.mu.Unlock()
		return core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{{
			ConnectionID: connectionID, ProviderID: "cloudflare", Type: core.ResourceAccessApp,
			ExternalID: appID, Ownership: core.OwnershipManaged,
		}}}, nil

	case core.StepUpdateAccessPolicy:
		policyID := step.Technical.ResourceID
		appID := step.Technical.Parameters["app_id"]
		p.mu.RLock()
		if policyID == "" {
			policyID = conn.policyID
		}
		if appID == "" {
			appID = conn.accessID
		}
		p.mu.RUnlock()
		if appID == "" || policyID == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy update requires exact application and policy IDs")}, nil
		}
		policy := access.Policy{
			AuthMode:        accessAuthMode(step.Technical.Parameters),
			AllowedEmails:   splitNonEmpty(step.Technical.Parameters["allowed_emails"]),
			AllowedDomains:  splitNonEmpty(step.Technical.Parameters["allowed_domains"]),
			SessionDuration: step.Technical.Parameters["session_duration"],
		}
		if policy.AuthMode != "none" && policy.AuthMode != "private_network" && len(policy.AllowedEmails) == 0 && len(policy.AllowedDomains) == 0 {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("access policy update requires at least one allowed email or domain")}, nil
		}
		err := p.access.UpdatePolicy(ctx, p.accountID, appID, policyID, policy)
		return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, nil

	case core.StepUpdateDNSRecord:
		dnsID := step.Technical.ResourceID
		hostname := step.Technical.Parameters["hostname"]
		tunnelID := step.Technical.Parameters["tunnel_id"]
		if dnsID == "" || hostname == "" || tunnelID == "" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("DNS update requires exact record ID, hostname, and tunnel ID")}, nil
		}
		if err := p.dns.UpdateCNAME(ctx, p.zoneID, dnsID, hostname, tunnelID); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: err}, nil
		}
		p.mu.Lock()
		conn.dnsID = dnsID
		if conn.hostname == "" {
			conn.hostname = hostname
		}
		p.mu.Unlock()
		return core.StepResult{StepID: step.ID, Succeeded: true, Resources: []core.ProviderResource{{
			ConnectionID: connectionID, ProviderID: "cloudflare", Type: core.ResourceDNSRecord,
			ExternalID: dnsID, Ownership: core.OwnershipManaged,
		}}}, nil

	case core.StepStartConnector:
		mode := step.Technical.Parameters["mode"]
		handle, startErr := p.launchConnector(ctx, connectionID, conn, mode, step.Technical.Parameters["origin_url"])
		if startErr != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("starting connector process: %w", startErr)}, nil
		}
		slog.Info("cloudflared connector started via process service", "pid", handle.PID, "mode", mode)

		if mode == "quick" {
			stdoutPath := filepath.Join(p.logDir, fmt.Sprintf("connector-%s.out", safeShortID(string(connectionID), 8)))
			publicAddr, discoverErr := p.discoverQuickTunnelAddress(connectionID, stdoutPath, 10*time.Second)
			if discoverErr != nil {
				// Stop leaked connector to prevent process leak. (SPEC P0 #5)
				_ = p.connectorProc.Stop(connectionID, 2*time.Second)
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("discovering quick tunnel address: %w", discoverErr)}, nil
			}
			if publicAddr == "" {
				// Stop leaked connector to prevent process leak. (SPEC P0 #5)
				_ = p.connectorProc.Stop(connectionID, 2*time.Second)
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("quick tunnel address not discovered within timeout")}, nil
			}
			p.mu.Lock()
			conn.publicURL = publicAddr
			p.mu.Unlock()
			slog.Info("quick tunnel address discovered", "address", publicAddr)
		}

		// Connector PID is process runtime state, not a durable provider resource.
		// It is stored in ConnectionRuntime.Connector, not provider_resources.
		return core.StepResult{
			StepID:    step.ID,
			Succeeded: true,
			Resources: []core.ProviderResource{},
		}, nil

	case core.StepVerifyConnector:
		params := step.Technical.Parameters
		expectedState := params["expected_state"]
		p.mu.RLock()
		pid := conn.connectorPID
		started := conn.connectorStarted
		p.mu.RUnlock()

		if !started || pid == 0 {
			if expectedState == "stopped" {
				return core.StepResult{StepID: step.ID, Succeeded: true}, nil
			}
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("connector not initialized")}, nil
		}

		handle, ok := p.connectorProc.Observe(connectionID)
		if !ok || handle.PID == 0 {
			if expectedState == "stopped" {
				p.mu.Lock()
				conn.connectorPID = 0
				conn.connectorStarted = false
				p.mu.Unlock()
				return core.StepResult{StepID: step.ID, Succeeded: true}, nil
			}
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("connector process not found")}, nil
		}

		if err := verifyProcessIdentity(handle); err != nil {
			if expectedState == "stopped" {
				p.mu.Lock()
				conn.connectorPID = 0
				conn.connectorStarted = false
				p.mu.Unlock()
				return core.StepResult{StepID: step.ID, Succeeded: true}, nil
			}
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("connector identity verification failed: %w", err)}, nil
		}

		if expectedState == "stopped" {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("connector expected stopped but is still running (pid %d)", handle.PID)}, nil
		}

		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepVerifyEndpoint:
		p.mu.RLock()
		pid := conn.connectorPID
		hostname := conn.hostname
		publicURL := conn.publicURL
		starter := conn.connectorStarted
		p.mu.RUnlock()

		if pid == 0 || !starter {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: connector not initialized")}, nil
		}
		handle, ok := p.connectorProc.Observe(connectionID)
		if !ok || handle.PID == 0 {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: connector process not found")}, nil
		}
		if err := verifyProcessIdentity(handle); err != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: connector identity verification failed: %w", err)}, nil
		}

		originURL := step.Technical.Parameters["origin_url"]
		if originURL != "" {
			if err := p.verifyLocalOrigin(ctx, originURL); err != nil {
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: origin probe failed: %w", err)}, nil
			}
		}

		if hostname != "" && p.zoneID != "" {
			if err := p.verifyDNSResolution(ctx, hostname); err != nil {
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: DNS verification failed: %w", err)}, nil
			}
		}

		probeURL := ""
		if publicURL != "" {
			probeURL = publicURL
		} else if hostname != "" {
			probeURL = fmt.Sprintf("https://%s", hostname)
		}
		if probeURL != "" {
			if err := p.verifyPublicEndpoint(ctx, probeURL); err != nil {
				return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("endpoint not reachable: public probe failed: %w", err)}, nil
			}
		}

		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepStopConnector:
		handle, ok := p.connectorProc.Observe(connectionID)
		if ok && handle.PID != 0 {
			stepErr := p.connectorProc.Stop(connectionID, 5*time.Second)
			if stepErr == nil {
				p.mu.Lock()
				conn.connectorPID = 0
				conn.connectorStarted = false
				p.mu.Unlock()
			}
			return core.StepResult{StepID: step.ID, Succeeded: stepErr == nil, Error: stepErr}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepDeleteTunnel:
		// Use the exact resource ID from the step's technical operation.
		tunnelID := step.Technical.ResourceID
		if tunnelID == "" {
			p.mu.RLock()
			tun := conn.tunnel
			p.mu.RUnlock()
			if tun != nil {
				tunnelID = tun.TunnelID
			}
		}
		if tunnelID != "" {
			err := p.tunnels.Delete(ctx, p.accountID, tunnelID)
			if err == nil {
				p.mu.Lock()
				if conn.tunnel != nil && conn.tunnel.TunnelID == tunnelID {
					conn.tunnel = nil
				}
				p.mu.Unlock()
			}
			return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepDeleteDNSRecord:
		dnsID := step.Technical.ResourceID
		if dnsID == "" {
			p.mu.RLock()
			dnsID = conn.dnsID
			p.mu.RUnlock()
		}
		if dnsID != "" {
			err := p.dns.DeleteRecord(ctx, p.zoneID, dnsID)
			if err == nil {
				p.mu.Lock()
				if conn.dnsID == dnsID {
					conn.dnsID = ""
				}
				p.mu.Unlock()
			}
			return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepDeleteAccessApp:
		accessID := step.Technical.ResourceID
		if accessID == "" {
			p.mu.RLock()
			accessID = conn.accessID
			p.mu.RUnlock()
		}
		if accessID != "" {
			err := p.access.DeleteApp(ctx, p.accountID, accessID)
			if err == nil {
				p.mu.Lock()
				if conn.accessID == accessID {
					conn.accessID = ""
					conn.policyID = ""
				}
				p.mu.Unlock()
			}
			return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepDeleteAccessPolicy:
		// Delete the specific Access policy by its exact external ID.
		policyID := step.Technical.ResourceID
		if policyID == "" {
			p.mu.RLock()
			policyID = conn.policyID
			p.mu.RUnlock()
		}
		if policyID != "" {
			err := p.access.DeletePolicy(ctx, p.accountID, policyID)
			if err == nil {
				p.mu.Lock()
				if conn.policyID == policyID {
					conn.policyID = ""
				}
				p.mu.Unlock()
			}
			return core.StepResult{StepID: step.ID, Succeeded: err == nil, Error: err}, nil
		}
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepRestartConnector:
		handle, ok := p.connectorProc.Observe(connectionID)
		if ok && handle.PID != 0 {
			if err := p.connectorProc.Stop(connectionID, 5*time.Second); err != nil {
				slog.Warn("connector stop during restart", "error", err)
			}
		}
		mode := step.Technical.Parameters["mode"]
		handle2, restartErr := p.launchConnector(ctx, connectionID, conn, mode, step.Technical.Parameters["origin_url"])
		if restartErr != nil {
			return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("restarting connector process: %w", restartErr)}, nil
		}
		slog.Info("cloudflared connector restarted via process service", "pid", handle2.PID, "mode", mode)
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	case core.StepFinalizeLocalDeletion:
		// The controller commits the local profile/runtime deletion after this
		// provider step succeeds. There is no remote Cloudflare action here.
		return core.StepResult{StepID: step.ID, Succeeded: true}, nil

	default:
		return core.StepResult{StepID: step.ID, Succeeded: false, Error: fmt.Errorf("unknown step kind: %s", step.Kind)}, nil
	}
}

func splitNonEmpty(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

// SetupFlow declares what Cloudflare needs in order to configure an account.
//
// The fields are declared here rather than hardcoded in the TUI, so the setup
// screen renders a form it does not have to understand and a new provider does
// not require a UI change.
func cloudflareSetupFlow() core.SetupFlow {
	return core.SetupFlow{
		Kind:          core.SetupAccount,
		IdentityField: "account_id",
		SecretField:   "credential",
		Summary:       "Configure a Cloudflare account so Portico can create managed tunnels for you.",
		Fields: []core.SetupField{
			{
				ID:          "account_id",
				Label:       "Account ID",
				Description: "Found on the Cloudflare dashboard overview page.",
				Required:    true,
				EnvVars:     []string{"PORTICO_CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_ACCOUNT_ID"},
			},
			{
				ID:          "label",
				Label:       "Label",
				Description: "A name for this account inside Portico. Defaults to the account ID.",
			},
			{
				ID:      "zone_id",
				Label:   "Zone ID",
				EnvVars: []string{"PORTICO_CLOUDFLARE_ZONE_ID", "CLOUDFLARE_ZONE_ID"},
				Description: "Only needed for permanent hostnames and DNS. " +
					"Leave blank to use tunnels with temporary addresses.",
			},
			{
				ID:          "credential",
				Label:       "API token",
				Description: "A token that can read your account and manage tunnels.",
				Secret:      true,
				Required:    true,
				// Read from the environment, never from a command argument: an
				// argument is in the shell history and visible in the process
				// list to every user on the machine.
				EnvVars: []string{"PORTICO_CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_TOKEN"},
			},
		},
		CapabilityNotes: []string{
			"Without any account: temporary addresses using Quick Tunnels.",
			"With an account and token: managed tunnels that survive restarts.",
			"With a zone as well: permanent hostnames, DNS records and Access protection.",
		},
	}
}

func (p *Provider) SetupFlow() core.SetupFlow { return cloudflareSetupFlow() }
