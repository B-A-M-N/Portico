package cloudflare

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"

	"github.com/B-A-M-N/portico/internal/core"
)

// Classifying why an observation failed.
//
// Only an authoritative 404 means a resource is gone. Everything else is transient,
// because a lookup that failed says nothing about whether the resource exists — and
// treating a failure as an absence makes reconciliation recreate a tunnel, a DNS
// record or an Access policy that is already there.
//
// Within "transient", a rate limit is not the same advice as an unclassified failure:
// one says wait, the other says something is wrong. ObservationRateLimited exists as
// its own state because callers act on the difference.

func TestOnlyAnAuthoritativeAbsenceIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   core.ObservationStatus
	}{
		{name: "not found", status: http.StatusNotFound, want: core.ObservationMissing},
		{name: "gone", status: http.StatusGone, want: core.ObservationMissing},
		{name: "unauthorized", status: http.StatusUnauthorized, want: core.ObservationUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, want: core.ObservationUnauthorized},
		{name: "rate limited", status: http.StatusTooManyRequests, want: core.ObservationRateLimited},
		{name: "server error", status: http.StatusInternalServerError, want: core.ObservationTransient},
		{name: "bad gateway", status: http.StatusBadGateway, want: core.ObservationTransient},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: core.ObservationTransient},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, want: core.ObservationTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A message as well as a status: the detail the classifier returns is
			// the error's own text, and a cf.Error built with only a status has
			// none. That is a property of the fixture, not of the classifier.
			err := &cf.Error{
				StatusCode: tc.status,
				Errors:     []cf.ResponseInfo{{Code: 10000, Message: "provider said no"}},
			}
			got, detail := classifyObservationError(err)
			if got != tc.want {
				t.Fatalf("status %d classified as %q, want %q", tc.status, got, tc.want)
			}
			if detail == "" {
				t.Error("the classification carries no detail, so nothing can explain it")
			}
		})
	}
}

// TestAWrappedErrorIsStillClassified pins the defect the audit already found once.
//
// The client wraps its errors, so a direct type assertion silently fails and a
// deleted resource is reported as a failed lookup rather than an absent one. This
// holds errors.As in place at the classifier.
func TestAWrappedErrorIsStillClassified(t *testing.T) {
	wrapped := fmt.Errorf("getting tunnel: %w",
		fmt.Errorf("api call: %w", &cf.Error{StatusCode: http.StatusNotFound}))

	got, _ := classifyObservationError(wrapped)
	if got != core.ObservationMissing {
		t.Fatalf("a wrapped 404 classified as %q, want missing", got)
	}
}

// TestRetryExhaustionIsARateLimit pins the blind spot the tunnel tests exposed.
//
// The client retries a 429 on its own schedule and, when it runs out of attempts,
// returns its own exhaustion error rather than the status that caused it. The
// *cf.Error check therefore cannot see the 429, and a rate limit was being reported
// as an unclassified transient failure — which tells a user something is broken when
// the answer is to wait.
func TestRetryExhaustionIsARateLimit(t *testing.T) {
	// The client's own wording when it gives up.
	exhausted := errors.New("exceeded available rate limit retries")

	got, _ := classifyObservationError(exhausted)
	if got != core.ObservationRateLimited {
		t.Fatalf("rate limit exhaustion classified as %q, want rate_limited", got)
	}

	// Wrapped, as it arrives from a manager.
	wrapped := fmt.Errorf("getting DNS record: %w", exhausted)
	if got, _ := classifyObservationError(wrapped); got != core.ObservationRateLimited {
		t.Fatalf("wrapped exhaustion classified as %q, want rate_limited", got)
	}
}

// TestAnUnrelatedErrorIsNotRelabelled pins that the exhaustion match is narrow.
//
// It matches on the client's message because the status is no longer available, which
// is not something to be pleased about. The risk of that approach is over-matching, so
// this holds the boundary: an error that merely mentions limits is not a rate limit.
func TestAnUnrelatedErrorIsNotRelabelled(t *testing.T) {
	for _, err := range []error{
		errors.New("connection refused"),
		errors.New("context deadline exceeded"),
		errors.New("the account has exceeded its zone limit"),
		errors.New("retries are configured"),
	} {
		got, _ := classifyObservationError(err)
		if got == core.ObservationRateLimited {
			t.Errorf("%q was relabelled as a rate limit", err)
		}
		if got == core.ObservationMissing {
			t.Errorf("%q was classified as the resource being absent", err)
		}
	}
}

// TestNothingUnclassifiedIsMissing is the invariant behind all of the above.
//
// A resource is recreated when it is reported missing. Any failure that is not an
// authoritative absence must therefore avoid that classification, whatever it is.
func TestNothingUnclassifiedIsMissing(t *testing.T) {
	for _, err := range []error{
		errors.New("some entirely new failure mode"),
		fmt.Errorf("wrapped: %w", errors.New("unknown")),
		&cf.Error{StatusCode: http.StatusTeapot},
		&cf.Error{StatusCode: 0},
	} {
		if got, _ := classifyObservationError(err); got == core.ObservationMissing {
			t.Errorf("%q was classified as missing, so reconciliation would recreate the "+
				"resource", err)
		}
	}
}
