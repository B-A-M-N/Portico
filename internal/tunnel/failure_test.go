package tunnel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What a failed tunnel lookup means.
//
// Get answers two different questions with the same return: a tunnel that is absent
// (nil, nil) and one that could not be read (nil, err). Reconciliation branches on
// that answer, so classifying a rate limit or a server error as an absence makes
// Portico create a second tunnel for a connection that already has one — every time
// the API is busy.
//
// Unlike the DNS and Access managers, Get already classifies each status explicitly.
// These tests hold that classification in place: it is the kind of switch that gets
// "simplified" into a single fallthrough by someone who has not read this comment.

// failingManager serves one status and body for every request.
func failingManager(t *testing.T, status int, body string) *APIManager {
	t.Helper()
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts":    func() (int, string) { return status, body },
		"POST /client/v4/accounts":   func() (int, string) { return status, body },
		"PUT /client/v4/accounts":    func() (int, string) { return status, body },
		"DELETE /client/v4/accounts": func() (int, string) { return status, body },
	})
	return NewAPIManager(api)
}

// TestATransientFailureIsNotAnAbsence pins the classification for each status that
// must never mean "the tunnel is gone".
func TestATransientFailureIsNotAnAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		// wantReason is the word the error must carry, so the caller can tell a
		// rate limit from a permission problem without parsing the provider's own
		// message.
		wantReason string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantReason: "rate limited"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantReason: "unauthorized"},
		{name: "forbidden", status: http.StatusForbidden, wantReason: "unauthorized"},
		{name: "server error", status: http.StatusInternalServerError, wantReason: "transient"},
		{name: "bad gateway", status: http.StatusBadGateway, wantReason: "transient"},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantReason: "transient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := failingManager(t, tc.status,
				`{"success":false,"errors":[{"code":10000,"message":"x"}]}`)

			state, err := m.Get(context.Background(), "acct-1", "tunnel-1")
			if err == nil {
				t.Fatalf("status %d was reported as the tunnel being absent", tc.status)
			}
			if state != nil {
				t.Fatalf("a failed lookup returned a tunnel: %+v", state)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("the error does not classify the failure as %q: %v", tc.wantReason, err)
			}
		})
	}
}

// TestOnlyA404MeansTheTunnelIsGone pins the positive case.
func TestOnlyA404MeansTheTunnelIsGone(t *testing.T) {
	m := failingManager(t, http.StatusNotFound,
		`{"success":false,"errors":[{"code":1000,"message":"tunnel not found"}]}`)

	state, err := m.Get(context.Background(), "acct-1", "tunnel-1")
	if err != nil {
		t.Fatalf("a 404 was reported as a failure rather than an absence: %v", err)
	}
	if state != nil {
		t.Fatalf("a 404 returned a tunnel: %+v", state)
	}
}

// TestAMalformedBodyDoesNotReadAsAbsent pins that an unparseable response is not a
// successful empty result.
//
// The equivalent test for DNS found a nil dereference that crashed the supervisor, so
// this checks that a body the API did not produce neither panics nor produces a
// silent absence.
func TestAMalformedBodyDoesNotReadAsAbsent(t *testing.T) {
	for _, body := range []string{
		`<html><body>502 Bad Gateway</body></html>`,
		`{"success":true,"result":`,
		``,
		`null`,
		`{"success":true,"result":{"id":"tunnel-1"}}`,
	} {
		m := failingManager(t, http.StatusOK, body)

		state, err := m.Get(context.Background(), "acct-1", "tunnel-1")
		if err == nil && state == nil {
			t.Errorf("the body %q was read as the tunnel being absent", body)
		}
	}
}

// TestATimedOutLookupIsTransient pins that a request that never completes is not an
// absence.
func TestATimedOutLookupIsTransient(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			time.Sleep(300 * time.Millisecond)
			return http.StatusOK, `{"success":true,"result":{"id":"tunnel-1"}}`
		},
	})
	m := NewAPIManager(api)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	state, err := m.Get(ctx, "acct-1", "tunnel-1")
	if err == nil {
		t.Fatal("a timed-out lookup was reported as the tunnel being absent")
	}
	if state != nil {
		t.Fatalf("a timed-out lookup returned a tunnel: %+v", state)
	}
	// Transient, because a cancelled request says nothing about whether the tunnel
	// exists. Classifying it as missing would recreate one.
	if !strings.Contains(err.Error(), "transient") {
		t.Errorf("a timeout is not classified as transient: %v", err)
	}
}

// TestAFailedCreateReportsNoTunnel pins that a failed create returns nothing a caller
// could record as a managed resource.
func TestAFailedCreateReportsNoTunnel(t *testing.T) {
	m := failingManager(t, http.StatusTooManyRequests,
		`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`)

	info, err := m.Create(context.Background(), "acct-1", "portico-test")
	if err == nil {
		t.Fatal("a rate-limited create was reported as successful")
	}
	if info != nil {
		t.Fatalf("a failed create returned tunnel info: %+v", info)
	}
}

// TestTokenRetrievalFailsClosed pins that a credential fetch does not return an empty
// token as if it had succeeded.
//
// An empty token written to a credential file produces a connector that fails to
// authenticate, which is a much harder failure to trace back than a refused fetch.
func TestTokenRetrievalFailsClosed(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		m := failingManager(t, status, `{"success":false,"errors":[{"code":10000,"message":"x"}]}`)

		token, err := m.GetToken(context.Background(), "acct-1", "tunnel-1")
		if err == nil {
			t.Errorf("status %d returned a token without an error", status)
		}
		if token != "" {
			t.Errorf("status %d returned a non-empty token on failure", status)
		}
	}
}

// TestRemovalIsIdempotentButNotOptimistic pins the cleanup contract.
//
// A 404 means the tunnel is already gone, which is the desired state. A 429 or 5xx
// does not: reporting one as a completed removal discharges an obligation that was
// never discharged, leaving the tunnel at the provider with nothing recorded as
// needing to remove it.
func TestRemovalIsIdempotentButNotOptimistic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "already gone", status: http.StatusNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := failingManager(t, tc.status,
				`{"success":false,"errors":[{"code":10000,"message":"x"}]}`)

			err := m.Delete(context.Background(), "acct-1", "tunnel-1")
			if tc.wantErr && err == nil {
				t.Fatalf("status %d was reported as a completed removal", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d should complete the removal: %v", tc.status, err)
			}
		})
	}
}
