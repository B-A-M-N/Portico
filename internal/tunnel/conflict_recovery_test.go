package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// CreateTunnel recovering from an 81053 name-in-use conflict.
//
// A create whose outcome was never recorded (timeout after the request was
// sent) used to leave every retry failing permanently: Cloudflare refuses a
// duplicate name with error code 81053 and nothing looked the existing
// tunnel up. The manager now resolves the deterministic name and continues,
// so a week-old interrupted open does not strand the connection in a state
// no retry can fix.

// conflictFakeAPI answers create with 81053 once, serves the list lookup,
// and fails the test if the create is retried.
type conflictFakeAPI struct {
	fakeCloudflare
	creates int
}

func (f *conflictFakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
		f.creates++
		if f.creates > 1 {
			f.t.Error("a name-conflict recovery must not retry the create")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81053,"message":"Tunnel already exists with the given name"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"11111111-2222-3333-4444-555555555555","name":"portico-abcd1234","status":"healthy"}],"result_info":{"count":1,"page":1,"per_page":20}}`))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCreateTunnelRecoversFromNameConflict(t *testing.T) {
	fake := &conflictFakeAPI{fakeCloudflare: fakeCloudflare{t: t}}
	api := newTestClient(t, fake)

	info, err := NewAPIManager(api).CreateTunnel(context.Background(), "acct-1", "portico-abcd1234")
	if err != nil {
		t.Fatalf("a recoverable name conflict failed the create: %v", err)
	}
	if info == nil || info.ID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("recovery returned the wrong tunnel: %+v", info)
	}
	if fake.creates != 1 {
		t.Fatalf("create called %d times, want exactly 1", fake.creates)
	}
}

// newTestClient points a cloudflare-go client at an arbitrary http.Handler,
// for fixtures that need per-request logic rather than canned path replies.
func newTestClient(t *testing.T, handler http.Handler) *cf.API {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	api, err := cf.NewWithAPIToken("test-token",
		cf.BaseURL(server.URL+"/client/v4"),
		cf.UsingRetryPolicy(0, 0, 0),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return api
}

func TestCreateTunnelDoesNotRecoverFromAuthorizationFailure(t *testing.T) {
	fake := &forbiddenFakeAPI{fakeCloudflare: fakeCloudflare{t: t}}
	api := newTestClient(t, fake)

	if _, err := NewAPIManager(api).CreateTunnel(context.Background(), "acct-1", "portico-x"); err == nil {
		t.Fatal("an authorization failure was swallowed by conflict recovery")
	}
}

type forbiddenFakeAPI struct {
	fakeCloudflare
}

func (f *forbiddenFakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cfd_tunnel") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized to access the requested resource"}]}`))
		return
	}
	f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}
