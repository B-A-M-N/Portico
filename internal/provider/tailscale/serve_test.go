package tailscale

import (
	"testing"
)

// ServeRoute is the canonical representation. These tests pin its invariants.

func TestServeRouteIdentityIsFrontendOnly(t *testing.T) {
	a := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	b := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "4000"}
	if !a.IsSameIdentity(b) {
		t.Fatal("same frontend identity should be reported as same route")
	}
	if a.Identity() != "http:3000:/" {
		t.Fatalf("identity = %q", a.Identity())
	}
}

func TestServeRouteDifferentPortIsDifferentResource(t *testing.T) {
	a := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	b := ServeRoute{FrontendProtocol: "http", FrontendPort: "4000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	if a.IsSameIdentity(b) {
		t.Fatal("different frontend ports are different resources")
	}
}

func TestServeRouteDifferentPathIsDifferentResource(t *testing.T) {
	a := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	b := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/api", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	if a.IsSameIdentity(b) {
		t.Fatal("different frontend paths are different resources")
	}
}

func TestServeRouteSameBackendTwoFrontendsIsTwoResources(t *testing.T) {
	a := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	b := ServeRoute{FrontendProtocol: "https", FrontendPort: "3000", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "3000"}
	if a.IsSameIdentity(b) {
		t.Fatal("different frontend protocols are different resources")
	}
}

func TestServeRouteBackendProtocolIsPartOfEquality(t *testing.T) {
	a := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	b := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "3000"}
	if a.Equal(b) {
		t.Fatal("different backend protocols are not equal")
	}
}

func TestServeRouteHTTPBackendIncludesScheme(t *testing.T) {
	r := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	if got := r.BackendEndpoint(); got != "http://127.0.0.1:3000" {
		t.Fatalf("HTTP backend endpoint = %q", got)
	}
}

func TestServeRouteTCPBackendIncludesScheme(t *testing.T) {
	r := ServeRoute{FrontendProtocol: "tcp", FrontendPort: "5432", BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432"}
	if got := r.BackendEndpoint(); got != "tcp://127.0.0.1:5432" {
		t.Fatalf("TCP backend endpoint = %q", got)
	}
}

func TestServeRouteOpenArgsMatchCloseArgsFrontend(t *testing.T) {
	r := ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}
	open := r.OpenArgs()
	close := r.CloseArgs()
	if len(open) < 3 || len(close) < 3 {
		t.Fatalf("open=%v close=%v", open, close)
	}
	// Both carry the same frontend flag.
	if open[2] != close[2] {
		t.Fatalf("frontend flag diverged: open=%q close=%q", open[2], close[2])
	}
	// Open carries the backend; close carries "off".
	if open[len(open)-1] != "http://127.0.0.1:3000" {
		t.Fatalf("open backend = %q", open[len(open)-1])
	}
	if close[len(close)-1] != "off" {
		t.Fatalf("close last arg = %q", close[len(close)-1])
	}
}

func TestServeRouteRoundTripsThroughStepParameters(t *testing.T) {
	original := ServeRoute{FrontendProtocol: "https", FrontendPort: "8443", FrontendPath: "/api", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "8443"}
	params := original.StepParameters()
	reconstructed, err := serveRouteFromStep(params)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Equal(reconstructed) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", original, reconstructed)
	}
	if original.Identity() != reconstructed.Identity() {
		t.Fatalf("identity diverged: %q vs %q", original.Identity(), reconstructed.Identity())
	}
}

func TestServeRouteRoundTripsThroughResourceMetadata(t *testing.T) {
	original := ServeRoute{FrontendProtocol: "tcp", FrontendPort: "5432", BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432"}
	metadata := original.ResourceMetadata()
	metadata["address"] = "workstation.tail0abc.ts.net"
	reconstructed, err := serveRouteFromResource(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Equal(reconstructed) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", original, reconstructed)
	}
}

func TestServeRouteValidationRejectsInvalid(t *testing.T) {
	cases := []ServeRoute{
		{FrontendProtocol: "ftp", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "ftp", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: ""},
		{FrontendProtocol: "tcp", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "nopath", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
	}
	for i, r := range cases {
		if err := r.Validate(); err == nil {
			t.Errorf("case %d: invalid route accepted: %+v", i, r)
		}
	}
}

func TestServeRouteValidationAcceptsValid(t *testing.T) {
	cases := []ServeRoute{
		{FrontendProtocol: "http", FrontendPort: "3000", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
		{FrontendProtocol: "https", FrontendPort: "8443", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "8443"},
		{FrontendProtocol: "tcp", FrontendPort: "5432", BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432"},
		{FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/api", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"},
	}
	for i, r := range cases {
		if err := r.Validate(); err != nil {
			t.Errorf("case %d: valid route rejected: %+v: %v", i, r, err)
		}
	}
}

func TestNewServeRouteFromAddressMapsLocalPortToFrontend(t *testing.T) {
	cases := []struct {
		protocol string
		address  string
		want     ServeRoute
	}{
		{"http", "127.0.0.1:3000", ServeRoute{FrontendProtocol: "http", FrontendPort: "3000", FrontendPath: "/", BackendProtocol: "http", BackendHost: "127.0.0.1", BackendPort: "3000"}},
		{"https", "127.0.0.1:8443", ServeRoute{FrontendProtocol: "https", FrontendPort: "8443", FrontendPath: "/", BackendProtocol: "https", BackendHost: "127.0.0.1", BackendPort: "8443"}},
		{"tcp", "127.0.0.1:5432", ServeRoute{FrontendProtocol: "tcp", FrontendPort: "5432", FrontendPath: "/", BackendProtocol: "tcp", BackendHost: "127.0.0.1", BackendPort: "5432"}},
	}
	for _, c := range cases {
		r, err := newServeRouteFromAddress(c.protocol, c.address)
		if err != nil {
			t.Errorf("%s %s: %v", c.protocol, c.address, err)
			continue
		}
		if !r.Equal(c.want) {
			t.Errorf("%s %s: got %+v want %+v", c.protocol, c.address, r, c.want)
		}
	}
}

func TestNewServeRouteFromAddressRejectsUnsupportedProtocol(t *testing.T) {
	if _, err := newServeRouteFromAddress("udp", "127.0.0.1:53"); err == nil {
		t.Fatal("udp should be rejected")
	}
}

func TestNewServeRouteFromAddressRejectsBareAddress(t *testing.T) {
	if _, err := newServeRouteFromAddress("http", "not-a-host:port"); err == nil {
		t.Fatal("invalid address should be rejected")
	}
}

func TestObservedRoutesParsesRealHTTPConfig(t *testing.T) {
	out := []byte(serveHTTP("127.0.0.1:3000"))
	routes, err := observedRoutes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("parsed %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.FrontendProtocol != "http" || r.FrontendPort != "3000" || r.FrontendPath != "/" {
		t.Fatalf("frontend mismatch: %+v", r)
	}
	if r.BackendEndpoint() != "http://127.0.0.1:3000" {
		t.Fatalf("backend mismatch: %s", r.BackendEndpoint())
	}
}

func TestObservedRoutesParsesRealHTTPSConfig(t *testing.T) {
	out := []byte(serveHTTPS("127.0.0.1:8443"))
	routes, err := observedRoutes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("parsed %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.FrontendProtocol != "https" || r.FrontendPort != "8443" {
		t.Fatalf("frontend mismatch: %+v", r)
	}
	if r.BackendEndpoint() != "https://127.0.0.1:8443" {
		t.Fatalf("backend mismatch: %s", r.BackendEndpoint())
	}
}

func TestObservedRoutesParsesRealTCPConfig(t *testing.T) {
	out := []byte(serveTCP("127.0.0.1:5432"))
	routes, err := observedRoutes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("parsed %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.FrontendProtocol != "tcp" || r.FrontendPort != "5432" {
		t.Fatalf("frontend mismatch: %+v", r)
	}
	if r.BackendEndpoint() != "tcp://127.0.0.1:5432" {
		t.Fatalf("backend mismatch: %s", r.BackendEndpoint())
	}
}

func TestObservedRoutesEmptyConfigReturnsNothing(t *testing.T) {
	for _, out := range []string{"", "null", "{}", "  "} {
		routes, err := observedRoutes([]byte(out))
		if err != nil {
			t.Errorf("%q: %v", out, err)
		}
		if len(routes) != 0 {
			t.Errorf("%q: parsed %d routes, want 0", out, len(routes))
		}
	}
}

func TestObservedRoutesMalformedWebKeyFailsClosed(t *testing.T) {
	out := []byte(`{"Web":{"not-host-port":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	if _, err := observedRoutes(out); err == nil {
		t.Fatal("malformed web key should fail closed")
	}
}

func TestObservedRoutesMissingTCPProtocolFailsClosed(t *testing.T) {
	// Web entry with no matching TCP protocol entry.
	out := []byte(`{"Web":{"workstation.tail0abc.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	if _, err := observedRoutes(out); err == nil {
		t.Fatal("missing TCP protocol entry should fail closed")
	}
}

func TestObservedRoutesInvalidBackendFailsClosed(t *testing.T) {
	out := []byte(serveHTTPProxy("not-an-address"))
	if _, err := observedRoutes(out); err == nil {
		t.Fatal("invalid backend should fail closed")
	}
}

func TestParseBackendEndpointNormalizes(t *testing.T) {
	cases := []struct {
		in       string
		protocol string
		host     string
		port     string
	}{
		{"http://127.0.0.1:3000", "http", "127.0.0.1", "3000"},
		{"https://127.0.0.1:8443", "https", "127.0.0.1", "8443"},
		{"tcp://127.0.0.1:5432", "tcp", "127.0.0.1", "5432"},
		{"http://localhost:3000", "http", "127.0.0.1", "3000"},
		{"http://127.0.0.1:3000/", "http", "127.0.0.1", "3000"},
		{"http://127.0.0.1:3000/api", "http", "127.0.0.1", "3000"},
		{"127.0.0.1:3000", "", "127.0.0.1", "3000"},
	}
	for _, c := range cases {
		protocol, host, port, err := parseBackendEndpoint(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if protocol != c.protocol || host != c.host || port != c.port {
			t.Errorf("%q: got %q %q %q want %q %q %q", c.in, protocol, host, port, c.protocol, c.host, c.port)
		}
	}
}
