package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// Contract tests for the Cloudflare DNS API.
//
// This package had no tests. It creates and retargets the DNS record that makes
// a permanent hostname resolve, and it decides whether a record that has gone
// missing is an error or an absence — which is what the repair path branches on.

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

type fakeDNS struct {
	t        *testing.T
	requests []recordedRequest
	respond  map[string]func() (int, string)
}

func (f *fakeDNS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func newFakeAPI(t *testing.T, respond map[string]func() (int, string)) (*cf.API, *fakeDNS) {
	t.Helper()
	fake := &fakeDNS{t: t, respond: respond}
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

// TestARecordIsCreatedProxiedAndPointedAtTheTunnel pins the request that makes
// a hostname resolve.
//
// The content must be the tunnel's cfargotunnel address and the record must be
// proxied; an unproxied CNAME to that address does not route, and the failure
// appears as a hostname that resolves but does not answer.
func TestARecordIsCreatedProxiedAndPointedAtTheTunnel(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/zones": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"rec-1","name":"app.example.com","type":"CNAME"}}`),
	})

	id, err := NewAPIManager(api).CreateCNAME(context.Background(), "zone-1", "app.example.com", "tun-1")
	if err != nil {
		t.Fatalf("CreateCNAME: %v", err)
	}
	if id != "rec-1" {
		t.Fatalf("record id = %q", id)
	}
	if len(fake.requests) == 0 {
		t.Fatal("no record was created")
	}

	body := fake.requests[0].Body
	if got := body["type"]; got != "CNAME" {
		t.Errorf("type = %v, want CNAME", got)
	}
	if got := body["content"]; got != "tun-1.cfargotunnel.com" {
		t.Errorf("content = %v, want the tunnel address", got)
	}
	if got := body["proxied"]; got != true {
		t.Errorf("proxied = %v: an unproxied record does not route", got)
	}
	if got := body["name"]; got != "app.example.com" {
		t.Errorf("name = %v", got)
	}
	// Portico marks what it creates so an operator can tell it apart from
	// records they made themselves.
	if comment, _ := body["comment"].(string); !strings.Contains(comment, "Portico") {
		t.Errorf("the record is not marked as Portico's: %v", body["comment"])
	}
}

// TestAMissingRecordIsAnAbsenceNotAFailure pins the case the repair path
// branches on.
//
// A record deleted in the Cloudflare dashboard must be reported as absent so it
// can be recreated. Reported as an error, every repair fails on the lookup and
// the connection can never be fixed — which is what happened, because the
// not-found check used a type assertion against an error the client wraps.
func TestAMissingRecordIsAnAbsenceNotAFailure(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/zones": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":81044,"message":"Record not found."}],"result":null}`
		},
	})

	state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-gone")
	if err != nil {
		t.Fatalf("a deleted record reported an error: %v", err)
	}
	if state != nil {
		t.Fatalf("a deleted record reported state %#v", state)
	}
}

// TestARejectedTokenIsNotReportedAsAMissingRecord pins the opposite. If an
// authentication failure looked like an absent record, the repair path would
// recreate records forever against an API that is refusing it.
func TestARejectedTokenIsNotReportedAsAMissingRecord(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/zones": func() (int, string) {
			return http.StatusForbidden,
				`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`
		},
	})

	state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-1")
	if err == nil {
		t.Fatal("a rejected token was reported as a missing record")
	}
	if state != nil {
		t.Fatalf("a failed lookup returned state %#v", state)
	}
}

// TestRetargetingUpdatesRatherThanReplaces pins that a hostname change edits the
// existing record. Deleting and recreating would drop the record briefly, and
// would lose the durable identity Portico tracks it by.
func TestRetargetingUpdatesRatherThanReplaces(t *testing.T) {
	api, fake := newFakeAPI(t, map[string]func() (int, string){
		"PATCH /client/v4/zones": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"rec-1","name":"app.example.com","type":"CNAME"}}`),
		"PUT /client/v4/zones": ok(`{"success":true,"errors":[],"messages":[],
			"result":{"id":"rec-1","name":"app.example.com","type":"CNAME"}}`),
	})

	err := NewAPIManager(api).UpdateCNAME(context.Background(), "zone-1", "rec-1", "app.example.com", "tun-2")
	if err != nil {
		t.Fatalf("UpdateCNAME: %v", err)
	}

	if len(fake.requests) == 0 {
		t.Fatal("no update was made")
	}
	for _, r := range fake.requests {
		if r.Method == http.MethodDelete {
			t.Fatal("retargeting deleted the record instead of updating it")
		}
	}
	if got := fake.requests[0].Body["content"]; got != "tun-2.cfargotunnel.com" {
		t.Errorf("content = %v, want the new tunnel", got)
	}
	if !strings.Contains(fake.requests[0].Path, "rec-1") {
		t.Errorf("the update did not address the existing record: %s", fake.requests[0].Path)
	}
}
