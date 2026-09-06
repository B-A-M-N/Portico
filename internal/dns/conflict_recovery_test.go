package dns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// newTestServer points the manager's client at an arbitrary http.Handler,
// for fixtures that need per-request logic rather than canned path replies.
func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// testClient builds a cloudflare-go client against a test server URL.
func testClient(t *testing.T, server *httptest.Server) *cf.API {
	t.Helper()
	api, err := cf.NewWithAPIToken("test-token",
		cf.BaseURL(server.URL+"/client/v4"),
		cf.UsingRetryPolicy(0, 0, 0),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return api
}

// CreateCNAME recovering from an 81057 record-exists conflict.
//
// Same failure shape as the tunnel name conflict: an interrupted earlier
// create left a CNAME behind, and Cloudflare refuses a duplicate with
// 81057. The manager resolves the record by exact name+type and returns its
// ID so the operation continues — and so drift detection (which compares
// the record's target to the tracked tunnel) judges it rather than blind
// acceptance. A stale-target record from an abandoned connection is
// corrected in place by the normal drift path.

type dnsConflictFake struct {
	t       *testing.T
	creates int
}

func (f *dnsConflictFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dns_records"):
		f.creates++
		if f.creates > 1 {
			f.t.Error("a record-exists recovery must not retry the create")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81057,"message":"Record already exists"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
		// The list lookup resolving the existing record by name+type.
		// The target is deliberately wrong: recovery must surface the
		// record for drift detection, not manufacture a healthy one.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"rec-existing-1","type":"CNAME","name":"web.example.com","content":"old-tunnel.example.com","proxied":true}],"result_info":{"count":1,"page":1,"per_page":20}}`))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCreateCNAMERecoversFromRecordExists(t *testing.T) {
	fake := &dnsConflictFake{t: t}
	server := newTestServer(t, fake)

	id, err := NewAPIManager(testClient(t, server)).CreateCNAME(
		context.Background(), "zone-1", "web.example.com", "tunnel-new")
	if err != nil {
		t.Fatalf("a recoverable record-exists conflict failed the create: %v", err)
	}
	if id != "rec-existing-1" {
		t.Fatalf("recovery returned record %q, want the existing record", id)
	}
	if fake.creates != 1 {
		t.Fatalf("create called %d times, want exactly 1", fake.creates)
	}
}
