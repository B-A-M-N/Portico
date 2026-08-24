package access

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// What a failed Access lookup means.
//
// GetApp and GetPolicy answer two different questions with the same return: a
// resource that is absent (nil, nil) and one that could not be read (nil, err). The
// reconcile path branches on that answer — an absence is recreated, a failure is
// reported — so classifying a rate limit or a server error as an absence makes
// Portico create a second Access application for a hostname that already has one,
// and a second policy on it, every time the API is busy.
//
// 404 and 401/403 were covered. 429, 5xx, malformed JSON and timeouts were not,
// which is the gap docs/REMAINING_WORK.md names. The equivalent tests for DNS found
// a nil dereference that crashed the supervisor, so the same shape is checked here.

// failingAPI serves one status and body for every request.
func failingAPI(t *testing.T, status int, body string) *APIManager {
	t.Helper()
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts":    func() (int, string) { return status, body },
		"POST /client/v4/accounts":   func() (int, string) { return status, body },
		"PUT /client/v4/accounts":    func() (int, string) { return status, body },
		"DELETE /client/v4/accounts": func() (int, string) { return status, body },
	})
	return NewAPIManager(api, "team.cloudflareaccess.com")
}

// TestARateLimitedAppLookupIsNotAnAbsence pins the most costly misclassification.
func TestARateLimitedAppLookupIsNotAnAbsence(t *testing.T) {
	m := failingAPI(t, http.StatusTooManyRequests,
		`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`)

	state, err := m.GetApp(context.Background(), "acct-1", "app-1")
	if err == nil {
		t.Fatal("a rate-limited lookup was reported as the application being absent")
	}
	if state != nil {
		t.Fatalf("a failed lookup returned an application: %+v", state)
	}
}

// TestServerErrorsAreNotAbsences pins the same rule for 5xx, for both lookups.
func TestServerErrorsAreNotAbsences(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		body := `{"success":false,"errors":[{"code":10000,"message":"server error"}]}`
		m := failingAPI(t, status, body)

		if state, err := m.GetApp(context.Background(), "acct-1", "app-1"); err == nil {
			t.Errorf("app lookup: status %d reported as an absence (state %+v)", status, state)
		}
		if state, err := m.GetPolicy(context.Background(), "acct-1", "app-1", "pol-1"); err == nil {
			t.Errorf("policy lookup: status %d reported as an absence (state %+v)", status, state)
		}
	}
}

// TestMalformedBodiesDoNotCrashOrReadAsAbsent pins the shape that crashed the DNS
// manager.
//
// A body the API did not produce — a proxy's HTML error page returned with a 200 —
// leaves optional pointer fields nil. Dereferencing one killed the process holding
// every connection. GetPolicy reads a *string for the session duration, so the same
// mistake was available here.
func TestMalformedBodiesDoNotCrashOrReadAsAbsent(t *testing.T) {
	for _, body := range []string{
		`<html><body>502 Bad Gateway</body></html>`,
		`{"success":true,"result":`,
		``,
		`null`,
		// A successful envelope whose result omits every optional field.
		`{"success":true,"result":{"id":"pol-1"}}`,
	} {
		m := failingAPI(t, http.StatusOK, body)

		// The requirement is that neither call panics, and that neither reports a
		// silent absence the reconcile path would act on by recreating.
		appState, appErr := m.GetApp(context.Background(), "acct-1", "app-1")
		if appErr == nil && appState == nil {
			t.Errorf("app lookup read the body %q as the application being absent", body)
		}
		policyState, policyErr := m.GetPolicy(context.Background(), "acct-1", "app-1", "pol-1")
		if policyErr == nil && policyState == nil {
			t.Errorf("policy lookup read the body %q as the policy being absent", body)
		}
	}
}

// TestATimedOutLookupIsAnError pins that a request that never completes is not an
// absence.
func TestATimedOutLookupIsAnError(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/accounts": func() (int, string) {
			time.Sleep(300 * time.Millisecond)
			return http.StatusOK, `{"success":true,"result":{"id":"app-1"}}`
		},
	})
	m := NewAPIManager(api, "team.cloudflareaccess.com")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	state, err := m.GetApp(ctx, "acct-1", "app-1")
	if err == nil {
		t.Fatal("a timed-out lookup was reported as the application being absent")
	}
	if state != nil {
		t.Fatalf("a timed-out lookup returned an application: %+v", state)
	}
}

// TestOnlyA404IsAnAbsence pins the positive case, so the tests above are not
// satisfied by a manager that never reports an absence at all.
func TestOnlyA404IsAnAbsence(t *testing.T) {
	m := failingAPI(t, http.StatusNotFound,
		`{"success":false,"errors":[{"code":12109,"message":"does not exist"}]}`)

	state, err := m.GetApp(context.Background(), "acct-1", "app-1")
	if err != nil {
		t.Fatalf("a 404 was reported as a failure rather than an absence: %v", err)
	}
	if state != nil {
		t.Fatalf("a 404 returned an application: %+v", state)
	}

	policy, err := m.GetPolicy(context.Background(), "acct-1", "app-1", "pol-1")
	if err != nil {
		t.Fatalf("a 404 policy was reported as a failure: %v", err)
	}
	if policy != nil {
		t.Fatalf("a 404 returned a policy: %+v", policy)
	}
}

// TestARateLimitedCreateReportsNoID pins that a failed create returns nothing a
// caller could record as a managed resource.
//
// Recording an ID for a resource that was not created leaves Portico believing it
// owns something that does not exist: the cleanup record says remove it and there is
// nothing to remove.
func TestARateLimitedCreateReportsNoID(t *testing.T) {
	m := failingAPI(t, http.StatusTooManyRequests,
		`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`)

	appID, err := m.CreateAppOnly(context.Background(), "acct-1", "app.example.com")
	if err == nil {
		t.Fatal("a rate-limited application create was reported as successful")
	}
	if appID != "" {
		t.Fatalf("a failed create returned the ID %q", appID)
	}

	policyID, err := m.CreatePolicy(context.Background(), "acct-1", "app-1", Policy{
		AllowedEmails: []string{"someone@example.com"},
	})
	if err == nil {
		t.Fatal("a rate-limited policy create was reported as successful")
	}
	if policyID != "" {
		t.Fatalf("a failed policy create returned the ID %q", policyID)
	}
}

// TestRemovalIsIdempotentButNotOptimistic pins the cleanup contract.
//
// A 404 or 410 means the resource is already gone, which is the desired state. A 429
// or 5xx does not: reporting one as a completed removal discharges an obligation that
// was never discharged, leaving the resource at the provider with nothing recorded as
// needing to remove it.
func TestRemovalIsIdempotentButNotOptimistic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "already gone", status: http.StatusNotFound},
		{name: "gone permanently", status: http.StatusGone},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"success":false,"errors":[{"code":10000,"message":"x"}]}`
			m := failingAPI(t, tc.status, body)

			appErr := m.DeleteApp(context.Background(), "acct-1", "app-1")
			if tc.wantErr && appErr == nil {
				t.Errorf("status %d was reported as a completed application removal", tc.status)
			}
			if !tc.wantErr && appErr != nil {
				t.Errorf("status %d should complete the removal: %v", tc.status, appErr)
			}

			policyErr := m.DeletePolicy(context.Background(), "acct-1", "pol-1")
			if tc.wantErr && policyErr == nil {
				t.Errorf("status %d was reported as a completed policy removal", tc.status)
			}
			if !tc.wantErr && policyErr != nil {
				t.Errorf("status %d should complete the policy removal: %v", tc.status, policyErr)
			}
		})
	}
}
