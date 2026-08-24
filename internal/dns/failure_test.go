package dns

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What a failed lookup means.
//
// GetRecord answers two different questions with the same return: a record that is
// absent (nil, nil) and a record that could not be read (nil, err). The repair path
// branches on that answer — an absence is recreated, a failure is reported — so
// classifying a rate limit or a server error as an absence would make Portico
// recreate a DNS record that already exists, every time the API was busy.
//
// 404 and 401/403 were covered. 429, 5xx, malformed JSON and timeouts were not,
// which is the gap docs/REMAINING_WORK.md names.

// TestARateLimitIsNotAnAbsence pins the most costly misclassification.
//
// A 429 means "ask again later", not "the record is gone". Treating it as an absence
// makes the repair path create a second record for a hostname that already has one.
func TestARateLimitIsNotAnAbsence(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/zones": func() (int, string) {
			return http.StatusTooManyRequests,
				`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`
		},
	})

	state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-1")
	if err == nil {
		t.Fatal("a rate-limited lookup was reported as a successful absence")
	}
	if state != nil {
		t.Fatalf("a failed lookup returned a record: %+v", state)
	}
}

// TestAServerErrorIsNotAnAbsence pins the same rule for 5xx.
func TestAServerErrorIsNotAnAbsence(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		api, _ := newFakeAPI(t, map[string]func() (int, string){
			"GET /client/v4/zones": func() (int, string) {
				return status, `{"success":false,"errors":[{"code":10000,"message":"server error"}]}`
			},
		})

		state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-1")
		if err == nil {
			t.Errorf("status %d was reported as a successful absence", status)
		}
		if state != nil {
			t.Errorf("status %d returned a record: %+v", status, state)
		}
	}
}

// TestMalformedJSONIsAnError pins that an unparseable response is not a success.
//
// A body that is not JSON at all — a proxy's HTML error page, most commonly — must
// not be read as an empty result. An empty result is indistinguishable from an
// absence, and an absence gets recreated.
func TestMalformedJSONIsAnError(t *testing.T) {
	for _, body := range []string{
		`<html><body>502 Bad Gateway</body></html>`,
		`{"success":true,"result":`,
		``,
		`null`,
	} {
		api, _ := newFakeAPI(t, map[string]func() (int, string){
			"GET /client/v4/zones": func() (int, string) { return http.StatusOK, body },
		})

		state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-1")
		// Either an error, or a record — never a silent absence, which is what the
		// repair path would act on.
		if err == nil && state == nil {
			t.Errorf("the malformed body %q was read as the record being absent", body)
		}
	}
}

// TestATimeoutIsAnError pins that a request that never completes is not an absence.
func TestATimeoutIsAnError(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/zones": func() (int, string) {
			// Longer than the context below, so the client gives up first.
			time.Sleep(300 * time.Millisecond)
			return http.StatusOK, `{"success":true,"result":{"id":"rec-1"}}`
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	state, err := NewAPIManager(api).GetRecord(ctx, "zone-1", "rec-1")
	if err == nil {
		t.Fatal("a timed-out lookup was reported as a successful absence")
	}
	if state != nil {
		t.Fatalf("a timed-out lookup returned a record: %+v", state)
	}
}

// TestOnlyA404IsAnAbsence pins the positive case, so the tests above are not
// satisfied by a manager that simply never reports an absence.
func TestOnlyA404IsAnAbsence(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"GET /client/v4/zones": func() (int, string) {
			return http.StatusNotFound,
				`{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`
		},
	})

	state, err := NewAPIManager(api).GetRecord(context.Background(), "zone-1", "rec-1")
	if err != nil {
		t.Fatalf("a 404 was reported as a failure rather than an absence: %v", err)
	}
	if state != nil {
		t.Fatalf("a 404 returned a record: %+v", state)
	}
}

// TestDeletingAnAlreadyGoneRecordSucceeds pins that removal is idempotent, and that
// a rate limit during removal is not mistaken for success.
//
// The distinction matters for cleanup: reporting a 429 as done discharges an
// obligation that was never discharged, leaving the record at the provider with
// nothing recorded as needing to remove it.
func TestDeletingAnAlreadyGoneRecordSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "already gone", status: http.StatusNotFound,
			body: `{"success":false,"errors":[{"code":81044,"message":"gone"}]}`},
		{name: "gone permanently", status: http.StatusGone,
			body: `{"success":false,"errors":[{"code":81044,"message":"gone"}]}`},
		{name: "rate limited", status: http.StatusTooManyRequests,
			body:    `{"success":false,"errors":[{"code":10000,"message":"slow down"}]}`,
			wantErr: true},
		{name: "server error", status: http.StatusInternalServerError,
			body:    `{"success":false,"errors":[{"code":10000,"message":"boom"}]}`,
			wantErr: true},
		{name: "forbidden", status: http.StatusForbidden,
			body:    `{"success":false,"errors":[{"code":10000,"message":"no permission"}]}`,
			wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := newFakeAPI(t, map[string]func() (int, string){
				"DELETE /client/v4/zones": func() (int, string) { return tc.status, tc.body },
			})

			err := NewAPIManager(api).DeleteRecord(context.Background(), "zone-1", "rec-1")
			if tc.wantErr && err == nil {
				t.Fatalf("status %d was reported as a successful removal", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d should be a completed removal: %v", tc.status, err)
			}
		})
	}
}

// TestARateLimitedCreateDoesNotReportAnID pins that a failed create returns nothing
// a caller could record as a managed resource.
//
// Recording an ID for a resource that was not created leaves Portico believing it
// owns something that does not exist, which is the mirror of the cleanup problem:
// the record says remove it and there is nothing to remove.
func TestARateLimitedCreateDoesNotReportAnID(t *testing.T) {
	api, _ := newFakeAPI(t, map[string]func() (int, string){
		"POST /client/v4/zones": func() (int, string) {
			return http.StatusTooManyRequests,
				`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`
		},
	})

	id, err := NewAPIManager(api).CreateCNAME(
		context.Background(), "zone-1", "app.example.com", "tunnel-1")
	if err == nil {
		t.Fatal("a rate-limited create was reported as successful")
	}
	if id != "" {
		t.Fatalf("a failed create returned the ID %q", id)
	}
	if !strings.Contains(err.Error(), "DNS") && !strings.Contains(err.Error(), "record") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}
