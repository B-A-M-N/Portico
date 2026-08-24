package tailscale

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ServeRoute is one Tailscale Serve route — the canonical provider-domain representation.
//
// Inside this package, ALL Serve-specific code operates on this type. There are no
// parallel meanings (string target, map[string]bool, backend-as-identity). One route
// type, one normalization, one identity.
//
// Conceptual model:
//
//	Plan step        = what Portico intends to do (carries route fields as parameters)
//	ProviderResource = durable identity + provenance of what was actually done
//	ServeRoute       = the shared object translating between those stages
//
// Metadata is NOT used to carry a route INTO the initial operation — it is the durable
// RESULT of a successful one. The plan must fully describe the mutation before any
// resource exists.
//
// Tailscale grammar (v1.74+):
//
//	tailscale serve --bg --http=<port> <backend>       # HTTP
//	tailscale serve --bg --https=<port> <backend>      # HTTPS
//	tailscale serve --bg --tcp=<port> tcp://<backend>  # raw TCP
//	tailscale serve --bg --http=<port> --set-path=<p> <backend>  # path routing
//	tailscale serve --bg --http=<port> off             # disable
//	tailscale serve --bg --tcp=<port> off              # disable
type ServeRoute struct {
	FrontendProtocol string `json:"frontend_protocol"`
	FrontendPort     string `json:"frontend_port"`
	FrontendPath     string `json:"frontend_path,omitempty"`
	BackendProtocol  string `json:"backend_protocol,omitempty"`
	BackendHost      string `json:"backend_host"`
	BackendPort      string `json:"backend_port"`
}

// Identity is the stable provider-side identity of the frontend binding Portico owns.
// It is what Portico persists as the resource ExternalID.
//
// Same frontend + same path + changed backend = same resource, drifted configuration.
// Different frontend port or path = different resource. Same backend behind two
// different frontends = two different resources.
//
// The backend target is deliberately NOT part of the identity.
func (r ServeRoute) Identity() string {
	if r.FrontendProtocol == "" || r.FrontendPort == "" {
		return ""
	}
	path := r.FrontendPath
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s:%s:%s", r.FrontendProtocol, r.FrontendPort, path)
}

// Validate rejects a route that cannot be realized or compared. Every constructor
// (serveRouteFromStep, serveRouteFromResource, newServeRouteFromAddress) invokes it,
// so a malformed persisted resource can never become a partially valid route.
func (r ServeRoute) Validate() error {
	switch r.FrontendProtocol {
	case "http", "https", "tcp":
	default:
		return fmt.Errorf("unknown frontend protocol %q", r.FrontendProtocol)
	}
	if r.FrontendPort == "" {
		return fmt.Errorf("missing frontend port")
	}
	if !isValidPort(r.FrontendPort) {
		return fmt.Errorf("invalid frontend port %q", r.FrontendPort)
	}
	switch r.BackendProtocol {
	case "http", "https", "tcp":
	default:
		return fmt.Errorf("unknown backend protocol %q", r.BackendProtocol)
	}
	if r.BackendHost == "" {
		return fmt.Errorf("missing backend host")
	}
	if r.BackendPort == "" {
		return fmt.Errorf("missing backend port")
	}
	if !isValidPort(r.BackendPort) {
		return fmt.Errorf("invalid backend port %q", r.BackendPort)
	}
	if r.FrontendProtocol == "tcp" && r.BackendProtocol != "tcp" {
		return fmt.Errorf("tcp frontend requires tcp backend, got %q", r.BackendProtocol)
	}
	if r.FrontendProtocol != "tcp" && r.BackendProtocol == "tcp" {
		return fmt.Errorf("%q frontend requires %q backend, got tcp", r.FrontendProtocol, r.FrontendProtocol)
	}
	// An empty path is equivalent to "/"; anything else must start with "/".
	if r.FrontendPath != "" && !strings.HasPrefix(r.FrontendPath, "/") {
		return fmt.Errorf("invalid frontend path %q: must start with /", r.FrontendPath)
	}
	return nil
}

// isValidPort reports whether p is a valid numeric port string.
func isValidPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n <= 65535
}

// BackendTarget is the local host:port traffic is proxied to. It does not include
// the protocol — use BackendEndpoint for the canonical protocol-aware form.
func (r ServeRoute) BackendTarget() string {
	if r.BackendHost == "" || r.BackendPort == "" {
		return ""
	}
	return net.JoinHostPort(r.BackendHost, r.BackendPort)
}

// BackendEndpoint is the canonical protocol-aware backend address. Two routes that
// differ only in backend protocol are NOT equivalent — http://127.0.0.1:3000 and
// https://127.0.0.1:3000 are different backends. Equal compares this complete form.
func (r ServeRoute) BackendEndpoint() string {
	scheme := r.BackendProtocol
	if scheme == "" {
		scheme = r.FrontendProtocol
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(r.BackendHost, r.BackendPort))
}

// OpenArgs builds the tailscale serve arguments that create this route.
func (r ServeRoute) OpenArgs() []string {
	args := []string{"serve", "--bg"}
	switch r.FrontendProtocol {
	case "http":
		args = append(args, "--http="+r.FrontendPort)
	case "https":
		args = append(args, "--https="+r.FrontendPort)
	case "tcp":
		args = append(args, "--tcp="+r.FrontendPort)
	}
	if r.FrontendPath != "" && r.FrontendPath != "/" {
		args = append(args, "--set-path="+r.FrontendPath)
	}
	backend := r.BackendEndpoint()
	if r.FrontendProtocol == "tcp" && !strings.HasPrefix(backend, "tcp://") {
		backend = "tcp://" + backend
	}
	args = append(args, backend)
	return args
}

// CloseArgs builds the tailscale serve arguments that withdraw this route.
//
// Tailscale disables a route by repeating its frontend flags with `off` at the end,
// NOT by passing the backend target. Passing the wrong target (or none) would either
// fail or disable a different route. If a path was set, --set-path must be repeated.
func (r ServeRoute) CloseArgs() []string {
	args := []string{"serve", "--bg"}
	switch r.FrontendProtocol {
	case "http":
		args = append(args, "--http="+r.FrontendPort)
	case "https":
		args = append(args, "--https="+r.FrontendPort)
	case "tcp":
		args = append(args, "--tcp="+r.FrontendPort)
	}
	if r.FrontendPath != "" && r.FrontendPath != "/" {
		args = append(args, "--set-path="+r.FrontendPath)
	}
	args = append(args, "off")
	return args
}

// Equal reports whether two routes are the same Portico-owned route: same identity
// and same complete backend (protocol + host + port). Different backends with the
// same frontend are drift; different frontends are different routes.
func (r ServeRoute) Equal(other ServeRoute) bool {
	return r.Identity() == other.Identity() && r.BackendEndpoint() == other.BackendEndpoint()
}

// IsSameIdentity reports whether two routes occupy the same frontend binding, regardless
// of backend target. A different backend on the same frontend is the same route that
// has drifted.
func (r ServeRoute) IsSameIdentity(other ServeRoute) bool {
	return r.Identity() == other.Identity()
}

// StepParameters encodes the route as primitive plan-step parameters. This is what a
// plan step carries so that ExecuteStep can reconstruct the route without any adapter
// memory. Exactly one canonical encoding — resource metadata reuses this.
func (r ServeRoute) StepParameters() map[string]string {
	return map[string]string{
		"frontend_protocol": r.FrontendProtocol,
		"frontend_port":     r.FrontendPort,
		"frontend_path":     r.FrontendPath,
		"backend_protocol":  r.BackendProtocol,
		"backend_host":      r.BackendHost,
		"backend_port":      r.BackendPort,
	}
}

// ResourceMetadata returns the structured metadata needed to reconstruct this route
// later — from a persisted ProviderResource. It reuses the canonical step encoding,
// so the two can never drift.
func (r ServeRoute) ResourceMetadata() map[string]string {
	return r.StepParameters()
}

// serveRouteFromStep reconstructs a ServeRoute from a plan step's parameters.
func serveRouteFromStep(params map[string]string) (ServeRoute, error) {
	route := ServeRoute{
		FrontendProtocol: params["frontend_protocol"],
		FrontendPort:     params["frontend_port"],
		FrontendPath:     params["frontend_path"],
		BackendProtocol:  params["backend_protocol"],
		BackendHost:      params["backend_host"],
		BackendPort:      params["backend_port"],
	}
	if err := route.Validate(); err != nil {
		return ServeRoute{}, fmt.Errorf("plan carried an invalid serve route: %w", err)
	}
	return route, nil
}

// serveRouteFromResource reconstructs a ServeRoute from a persisted ProviderResource's
// metadata. This is how restart, close, repair, and reconciliation recover the exact
// route Portico created — without any dependency on adapter memory.
func serveRouteFromResource(metadata map[string]string) (ServeRoute, error) {
	route := ServeRoute{
		FrontendProtocol: metadata["frontend_protocol"],
		FrontendPort:     metadata["frontend_port"],
		FrontendPath:     metadata["frontend_path"],
		BackendProtocol:  metadata["backend_protocol"],
		BackendHost:      metadata["backend_host"],
		BackendPort:      metadata["backend_port"],
	}
	if err := route.Validate(); err != nil {
		return ServeRoute{}, fmt.Errorf("persisted resource carries an invalid serve route: %w", err)
	}
	return route, nil
}

// newServeRouteFromAddress builds a ServeRoute from a local protocol and address,
// mapping the local port to the frontend port. For the current Portico model, the
// deterministic provider mapping is:
//
//	LocalProtocol=http  127.0.0.1:3000 → tailscale HTTP  :3000 → http://127.0.0.1:3000
//	LocalProtocol=https 127.0.0.1:8443 → tailscale HTTPS :8443 → https://127.0.0.1:8443
//	LocalProtocol=tcp   127.0.0.1:5432 → tailscale TCP   :5432 → tcp://127.0.0.1:5432
func newServeRouteFromAddress(protocol, address string) (ServeRoute, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return ServeRoute{}, fmt.Errorf("%q is not a host:port address: %v", address, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	route := ServeRoute{
		BackendHost:     host,
		BackendPort:     port,
		BackendProtocol: protocol,
		FrontendPath:    "/",
	}
	switch protocol {
	case "http", "https", "tcp":
		route.FrontendProtocol = protocol
		route.FrontendPort = port
	default:
		return ServeRoute{}, fmt.Errorf("unsupported protocol %q", protocol)
	}
	if err := route.Validate(); err != nil {
		return ServeRoute{}, err
	}
	return route, nil
}

// --- Tailscale serve status JSON parsing ---

// serveStatusConfig is the JSON structure returned by `tailscale serve status --json`.
//
// The upstream config has TCP and Web entries. Web keys are `$SNI_NAME:$PORT`, not
// bare ports. Each TCP port handler carries HTTP/HTTPS booleans identifying the
// frontend protocol; raw TCP forwards carry TCPForward.
//
// Reference: https://github.com/tailscale/tailscale/blob/main/ipn/serve.go
type serveStatusConfig struct {
	TCP map[string]*serveStatusTCP `json:"TCP"`
	Web map[string]*serveStatusWeb `json:"Web"`
}

type serveStatusTCP struct {
	HTTPS      bool   `json:"HTTPS"`
	HTTP       bool   `json:"HTTP"`
	TCPForward string `json:"TCPForward"`
}

type serveStatusWeb struct {
	Handlers map[string]*serveStatusHandler `json:"Handlers"`
}

type serveStatusHandler struct {
	Proxy string `json:"Proxy"`
}

// observedRoutes parses the JSON output of `tailscale serve status --json` into the
// routes the client currently has configured. It models enough of the real JSON to
// identify frontend protocol deterministically. For the current scope, Tailscale
// Services, Funnel, L3/TUN, and TLS-terminated TCP are not parsed.
//
// Fail closed: a route that cannot be interpreted (e.g. a Web key whose port doesn't
// parse, or a TCP entry with no protocol flags) returns an observation error rather
// than silently dropping the entry. Silently dropping malformed provider state turns
// "I could not understand this route" into "this route is missing", and reconciliation
// then creates infrastructure based on a false absence.
func observedRoutes(out []byte) ([]ServeRoute, error) {
	routes := []ServeRoute{}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return routes, nil
	}

	var config serveStatusConfig
	if err := json.Unmarshal(out, &config); err != nil {
		return nil, fmt.Errorf("could not read the tailscale serve configuration: %w", err)
	}

	// Web entries are keyed by SNI name + port (e.g. "workstation.tailnet.ts.net:3000").
	// The frontend protocol is determined by the corresponding TCP[port] entry.
	for frontendKey, web := range config.Web {
		if web == nil {
			continue
		}
		_, port, err := net.SplitHostPort(frontendKey)
		if err != nil {
			return nil, fmt.Errorf("the tailscale serve configuration has a web frontend %q that is not host:port: %v", frontendKey, err)
		}
		protocol := webProtocol(config.TCP, port)
		if protocol == "" {
			return nil, fmt.Errorf("the tailscale serve configuration has a web frontend on port %s with no matching TCP protocol entry", port)
		}
		for path, handler := range web.Handlers {
			if handler == nil {
				continue
			}
			backendProtocol, backendHost, backendPort, err := parseBackendEndpoint(handler.Proxy)
			if err != nil {
				return nil, fmt.Errorf("the tailscale serve configuration has an unreadable backend for %s%s: %v", frontendKey, path, err)
			}
			// When the client reports a bare host:port (no scheme), the backend
			// protocol is implied by the frontend protocol.
			if backendProtocol == "" {
				backendProtocol = protocol
			}
			route := ServeRoute{
				FrontendProtocol: protocol,
				FrontendPort:     port,
				FrontendPath:     path,
				BackendProtocol:  backendProtocol,
				BackendHost:      backendHost,
				BackendPort:      backendPort,
			}
			if err := route.Validate(); err != nil {
				return nil, fmt.Errorf("the tailscale serve configuration has an invalid route for %s%s: %v", frontendKey, path, err)
			}
			routes = append(routes, route)
		}
	}

	// Raw TCP forwards: TCPForward != "" means the frontend speaks TCP.
	for port, tcp := range config.TCP {
		if tcp == nil || tcp.TCPForward == "" {
			continue
		}
		backendProtocol, backendHost, backendPort, err := parseBackendEndpoint(tcp.TCPForward)
		if err != nil {
			return nil, fmt.Errorf("the tailscale serve configuration has an unreadable TCP forward for port %s: %v", port, err)
		}
		route := ServeRoute{
			FrontendProtocol: "tcp",
			FrontendPort:     port,
			FrontendPath:     "/",
			BackendProtocol:  backendProtocol,
			BackendHost:      backendHost,
			BackendPort:      backendPort,
		}
		if err := route.Validate(); err != nil {
			return nil, fmt.Errorf("the tailscale serve configuration has an invalid TCP forward for port %s: %v", port, err)
		}
		routes = append(routes, route)
	}

	return routes, nil
}

// webProtocol resolves the frontend protocol for a Web entry by looking up the
// corresponding TCP[port] entry. HTTP=true → http, HTTPS=true → https.
func webProtocol(tcp map[string]*serveStatusTCP, port string) string {
	entry, ok := tcp[port]
	if !ok {
		return ""
	}
	switch {
	case entry.HTTP:
		return "http"
	case entry.HTTPS:
		return "https"
	}
	return ""
}

// parseBackendEndpoint reduces a proxy target to its protocol, host, and port. The
// client may report `http://127.0.0.1:3000`, `tcp://127.0.0.1:5432`, or a bare
// `127.0.0.1:3000`. The scheme (if present) determines the backend protocol; a bare
// address returns an empty protocol.
//
// The result is canonical so that recorded and observed values compare equal through
// ServeRoute.Equal. A trailing path is stripped from the address (only host:port is
// identity). localhost is normalized to 127.0.0.1.
func parseBackendEndpoint(value string) (protocol, host, port string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", "", nil
	}

	// Strip a known scheme to recover the protocol.
	for _, scheme := range []string{"http://", "https://", "https+insecure://", "tcp://"} {
		if strings.HasPrefix(value, scheme) {
			protocol = strings.TrimSuffix(scheme, "://")
			value = value[len(scheme):]
			break
		}
	}

	// A trailing path is not part of the address.
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	value = strings.TrimSuffix(value, "/")

	host, port, err = net.SplitHostPort(value)
	if err != nil {
		return "", "", "", fmt.Errorf("%q is not a host:port address: %v", value, err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return protocol, host, port, nil
}
