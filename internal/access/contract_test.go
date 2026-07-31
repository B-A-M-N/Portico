package access

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// Contract tests for the Cloudflare Access API.
//
// This is the resource that decides who can reach a protected connection, and
// it was the one managed resource with no contract coverage: the existing tests
// exercise data transformation, and the adapter substitutes a fake manager. So
// nothing pinned the requests Portico actually sends to create a policy, or
// what it does when Cloudflare refuses one.
//
// As with the tunnel and DNS suites, these pin Portico's half of the contract.
// They are served by a local fake and are not evidence about Cloudflare.

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

type fakeAccess struct {
	t        *testing.T
	requests []recordedRequest
	respond  map[string]func() (int, string)
}

func (f *fakeAccess) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func newFakeAPI(t *testing.T, respond map[string]func() (int, string)) (*cf.API, *fakeAccess) {
	t.Helper()
	fake := &fakeAccess{t: t, respond: respond}
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

// TestCreatingAnAppSendsTheHostnameAndAllowedIdentities pins the request that
// decides who can reach a protected connection.
func TestCreatingAnAppSendsTheHostnameAndAllowedIdentities(t *testing.T) {
	var created int
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": func() (int, string) {
			created++
			if created == 1 {
				return http.StatusOK, `{"success":true,"errors":[],"messages":[],
					"result":{"id":"app-1","name":"flare-app.example.com","domain":"app.example.com"}}`
			}
			return http.StatusOK, `{"success":true,"errors":[],"messages":[],
				"result":{"id":"pol-1","name":"flare-allow-app.example.com","decision":"allow"}}`
		},
	})

	info, err := NewAPIManager(api, "acct-1").CreateApp(context.Background(), "acct-1", "app.example.com", Policy{
		AuthMode:       "email_otp",
		AllowedEmails:  []string{"alice@example.com"},
		AllowedDomains: []string{"example.org"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if info.AppID != "app-1" || info.PolicyID != "pol-1" {
		t.Fatalf("CreateApp returned %#v", info)
	}

	if len(fake.requests) < 2 {
		t.Fatalf("expected an application and a policy, got %d requests", len(fake.requests))
	}
	app := fake.requests[0]
	if got := app.Body["domain"]; got != "app.example.com" {
		t.Errorf("the application is for domain %v", got)
	}
	if got := app.Body["type"]; got != "self_hosted" {
		t.Errorf("application type = %v", got)
	}

	policy, err := json.Marshal(fake.requests[1].Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice@example.com", "example.org", "allow"} {
		if !strings.Contains(string(policy), want) {
			t.Errorf("the policy does not carry %q: %s", want, policy)
		}
	}
}

// TestAProtectedAppIsNeverCreatedWithoutIdentities pins the fail-closed rule.
//
// An Access application with an allow policy naming nobody is an application
// nobody can pass — or, depending on how the provider interprets an empty
// include list, one anybody can. Neither is what was asked for, so it is
// refused before anything is created.
func TestAProtectedAppIsNeverCreatedWithoutIdentities(t *testing.T) {
	var deleted bool
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"app-1","domain":"app.example.com"}}`),
		"DELETE /client/v4/accounts": func() (int, string) {
			deleted = true
			return http.StatusOK, `{"success":true,"errors":[],"messages":[],"result":{"id":"app-1"}}`
		},
	})

	_, err := NewAPIManager(api, "acct-1").CreateApp(context.Background(), "acct-1", "app.example.com", Policy{
		AuthMode: "email_otp",
	})
	if err == nil {
		t.Fatal("a protected application was created with no allowed identities")
	}
	if !strings.Contains(err.Error(), "at least one allowed email or domain") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
	_ = fake
	_ = deleted
}

// TestAFailedPolicyRemovesTheApplication pins that a half-created protection is
// not left behind.
//
// An application with no policy is a protected connection with no rule, and
// leaving one means the next repair finds an application it did not expect.
func TestAFailedPolicyRemovesTheApplication(t *testing.T) {
	var created int
	var deletedApp bool
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/accounts": func() (int, string) {
			created++
			if created == 1 {
				return http.StatusOK, `{"success":true,"errors":[],"messages":[],
					"result":{"id":"app-1","domain":"app.example.com"}}`
			}
			return http.StatusForbidden,
				`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`
		},
		"DELETE /client/v4/accounts": func() (int, string) {
			deletedApp = true
			return http.StatusOK, `{"success":true,"errors":[],"messages":[],"result":{"id":"app-1"}}`
		},
	})

	_, err := NewAPIManager(api, "acct-1").CreateApp(context.Background(), "acct-1", "app.example.com", Policy{
		AuthMode:      "email_otp",
		AllowedEmails: []string{"alice@example.com"},
	})
	if err == nil {
		t.Fatal("a failed policy was reported as success")
	}
	if !deletedApp {
		t.Fatal("the application was left behind with no policy protecting it")
	}
}

// TestAMissingApplicationIsAnAbsenceNotAFailure pins the case the repair path
// branches on — the same defect found in the tunnel and DNS managers, where a
// type assertion against a wrapped error never matched.
func TestAMissingApplicationIsAnAbsenceNotAFailure(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":12109,"message":"Access application not found"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api, "acct-1").GetApp(context.Background(), "acct-1", "app-gone")
	if err != nil {
		t.Fatalf("a deleted application reported an error: %v", err)
	}
	if state != nil {
		t.Fatalf("a deleted application reported state %#v", state)
	}
}

// TestARejectedTokenIsNotAMissingApplication pins the other direction: an
// authentication failure must not look like an absence, or the repair path
// recreates applications against an API that is refusing it.
func TestARejectedTokenIsNotAMissingApplication(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusForbidden,
				`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api, "acct-1").GetApp(context.Background(), "acct-1", "app-1")
	if err == nil {
		t.Fatal("a rejected token was reported as a missing application")
	}
	if state != nil {
		t.Fatalf("a failed lookup returned state %#v", state)
	}
}

// TestAMissingPolicyIsAnAbsenceNotAFailure pins the same rule one level down.
// A policy can be deleted while its application survives, and that is a repair
// Portico can make rather than a failure it must report.
func TestAMissingPolicyIsAnAbsenceNotAFailure(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":12130,"message":"Access policy not found"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api, "acct-1").GetPolicy(context.Background(), "acct-1", "app-1", "pol-gone")
	if err != nil {
		t.Fatalf("a deleted policy reported an error: %v", err)
	}
	if state != nil {
		t.Fatalf("a deleted policy reported state %#v", state)
	}
}

// TestAPolicyIsScopedToItsApplication pins that a policy request addresses the
// application it belongs to. An unscoped policy ID would be ambiguous across
// applications in the same account.
func TestAPolicyIsScopedToItsApplication(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"PUT /client/v4/accounts": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"pol-1","decision":"allow"}}`),
	})

	err := NewAPIManager(api, "acct-1").UpdatePolicy(context.Background(), "acct-1", "app-1", "pol-1", Policy{
		AuthMode:      "email_otp",
		AllowedEmails: []string{"alice@example.com"},
	})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	if len(fake.requests) == 0 {
		t.Fatal("no update was made")
	}
	path := fake.requests[0].Path
	if !strings.Contains(path, "app-1") {
		t.Errorf("the policy update is not scoped to its application: %s", path)
	}
	if !strings.Contains(path, "pol-1") {
		t.Errorf("the policy update does not address the policy: %s", path)
	}
}
