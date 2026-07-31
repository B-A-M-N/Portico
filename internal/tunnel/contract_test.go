package tunnel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// Contract tests for the Cloudflare tunnel API.
//
// The adapter was covered only by tests that call its own helpers, which
// asserts the code agrees with itself. These pin the requests Portico actually
// puts on the wire and what it does with the responses Cloudflare documents —
// the two things that break when a dependency is upgraded or an endpoint
// changes, and the two things a unit test of our own helpers cannot see.
//
// They are not a claim that Cloudflare behaves this way. They pin our half of
// the contract: the method, the path, the body we send, and our handling of the
// error shapes Cloudflare's API returns.

// recordedRequest is one request the fake API received.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeCloudflare serves canned responses and records what it was asked.
type fakeCloudflare struct {
	t        *testing.T
	requests []recordedRequest
	// respond maps "METHOD /path/prefix" to a status and body.
	respond map[string]func() (int, string)
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
	if r.Body != nil {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.Body = body
	}
	f.requests = append(f.requests, rec)

	for prefix, respond := range f.respond {
		method, path, _ := strings.Cut(prefix, " ")
		if r.Method == method && strings.HasPrefix(r.URL.Path, path) {
			status, body := respond()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}]}`))
}

// newFakeAPI starts a fake Cloudflare API and returns a client pointed at it.
func newFakeAPI(t *testing.T, respond map[string]func() (int, string)) (*cf.API, *fakeCloudflare) {
	t.Helper()
	fake := &fakeCloudflare{t: t, respond: respond}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	api, err := cf.NewWithAPIToken("test-token",
		cf.BaseURL(server.URL+"/client/v4"),
		cf.UsingRetryPolicy(0, 0, 0),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return api, fake
}

func ok(body string) func() (int, string) {
	return func() (int, string) { return http.StatusOK, body }
}

// TestCreatingATunnelSendsARemotelyManagedTunnel pins the request Portico makes.
//
// ConfigSrc "cloudflare" is what makes the tunnel remotely managed. Losing it
// would produce a tunnel that ignores the ingress Portico configures, and no
// test of our own helpers would notice.
func TestCreatingATunnelSendsARemotelyManagedTunnel(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"tun-1","name":"portico-test","status":"inactive"}}`),
		"GET /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":"run-token-value"}`),
	})

	info, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.TunnelID != "tun-1" || info.Token != "run-token-value" {
		t.Fatalf("Create returned %#v", info)
	}

	var create *recordedRequest
	for i := range fake.requests {
		if fake.requests[i].Method == http.MethodPost {
			create = &fake.requests[i]
			break
		}
	}
	if create == nil {
		t.Fatal("no tunnel was created")
	}
	if !strings.Contains(create.Path, "/accounts/acct-1/cfd_tunnel") {
		t.Errorf("tunnel created at %q", create.Path)
	}
	if got := create.Body["config_src"]; got != "cloudflare" {
		t.Errorf("config_src = %v, want cloudflare: the tunnel would ignore its ingress", got)
	}
	if got := create.Body["name"]; got != "portico-test" {
		t.Errorf("name = %v", got)
	}
	if secret, _ := create.Body["tunnel_secret"].(string); secret == "" {
		t.Error("no tunnel secret was sent")
	}
}

// TestATunnelSecretIsNeverReturnedToTheCaller pins that the generated secret
// stays inside the manager. It is a credential, and Create's result is logged,
// rendered and persisted.
func TestATunnelSecretIsNeverReturnedToTheCaller(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"tun-1","name":"portico-test"}}`),
		"GET /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":"run-token-value"}`),
	})

	info, err := NewAPIManager(api).Create(context.Background(), "acct-1", "portico-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var sentSecret string
	for _, r := range fake.requests {
		if s, okType := r.Body["tunnel_secret"].(string); okType && s != "" {
			sentSecret = s
		}
	}
	if sentSecret == "" {
		t.Fatal("the fixture no longer exercises a secret")
	}

	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sentSecret) {
		t.Fatalf("the tunnel secret is in the result the caller keeps: %s", encoded)
	}
}

// TestAMissingTunnelIsNotAFailure pins the deleted-outside-Portico case.
//
// Cloudflare answers 404 with error code 1000-series for a tunnel that no
// longer exists. Treating that as an error would make a connection whose tunnel
// was deleted in the dashboard unrepairable, because every repair would fail on
// the lookup rather than recreating what is missing.
func TestAMissingTunnelIsNotAFailure(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusNotFound, `{"success":false,"errors":[{"code":1049,"message":"tunnel not found"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-gone")
	if err != nil {
		t.Fatalf("a deleted tunnel reported an error: %v", err)
	}
	if state != nil {
		t.Fatalf("a deleted tunnel reported state %#v", state)
	}
}

// TestAnAuthenticationFailureIsReportedNotSwallowed pins the opposite: a token
// that is rejected must not look like a tunnel that does not exist, or the
// repair path would loop recreating tunnels it cannot see.
func TestAnAuthenticationFailureIsReportedNotSwallowed(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api).Get(context.Background(), "acct-1", "tun-1")
	if err == nil {
		t.Fatal("a rejected token was reported as a missing tunnel")
	}
	if state != nil {
		t.Fatalf("a failed lookup returned state %#v", state)
	}
}

// TestConfiguringIngressSendsTheOriginAndHostname pins the request that decides
// what the tunnel actually serves.
func TestConfiguringIngressSendsTheOriginAndHostname(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"PUT /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],"result":{}}`),
	})

	err := NewAPIManager(api).ConfigureIngress(context.Background(),
		"acct-1", "tun-1", "app.example.com", "http://127.0.0.1:3000")
	if err != nil {
		t.Fatalf("ConfigureIngress: %v", err)
	}

	if len(fake.requests) == 0 {
		t.Fatal("no ingress request was made")
	}
	body, err := json.Marshal(fake.requests[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app.example.com", "http://127.0.0.1:3000"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the ingress request does not carry %q: %s", want, body)
		}
	}
	// A catch-all rule is required; without it Cloudflare rejects the config.
	if !strings.Contains(string(body), "http_status:404") {
		t.Errorf("the ingress has no catch-all rule: %s", body)
	}
}
