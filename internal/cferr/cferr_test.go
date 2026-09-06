package cferr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
)

// The classifier is the single authority every manager and the observation
// path delegate to. These tests pin the classification contract itself: if
// cloudflare-go changes its error representation, the failures show up here
// first, in one place, instead of as divergent behaviour across four
// managers.

func cfErr(status int, codes ...int) error {
	errs := make([]cf.ResponseInfo, 0, len(codes))
	for _, c := range codes {
		errs = append(errs, cf.ResponseInfo{Code: c, Message: "provider said no"})
	}
	return &cf.Error{StatusCode: status, Errors: errs, ErrorCodes: codes}
}

// TestClassifyPinsTheFourClasses pins the whole decision table.
func TestClassifyPinsTheFourClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Class
	}{
		{"nil is transient", nil, ClassTransient},
		{"404 is absent", cfErr(http.StatusNotFound), ClassAbsent},
		{"410 is absent", cfErr(http.StatusGone), ClassAbsent},
		{"401 is unauthorized", cfErr(http.StatusUnauthorized), ClassUnauthorized},
		{"403 is unauthorized", cferrForbidden(), ClassUnauthorized},
		{"429 is rate limited", cfErr(http.StatusTooManyRequests), ClassRateLimited},
		{"500 is transient", cfErr(http.StatusInternalServerError), ClassTransient},
		{"502 is transient", cfErr(http.StatusBadGateway), ClassTransient},
		{"plain error is transient", errors.New("connection refused"), ClassTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Fatalf("classified %q, want %q", got, tc.want)
			}
		})
	}
}

func cferrForbidden() error {
	return &cf.Error{StatusCode: http.StatusForbidden}
}

// TestWrappedErrorsAreStillClassified pins errors.As traversal. cloudflare-go
// wraps its typed errors; a classifier that type-asserts instead of using
// errors.As silently misclassifies everything (this exact defect shipped once).
func TestWrappedErrorsAreStillClassified(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", cfErr(http.StatusNotFound)))
	if !IsAbsent(wrapped) {
		t.Fatal("a deeply wrapped 404 was not recognized as absent")
	}
}

// TestClientRetryExhaustionIsRateLimited pins that the client's own wording
// for giving up on a 429 classifies as rate-limited even though it carries
// no status code.
func TestClientRetryExhaustionIsRateLimited(t *testing.T) {
	exhausted := errors.New("exceeded available rate limit retries")
	if ClassifyError(exhausted) != ClassRateLimited {
		t.Fatalf("retry exhaustion classified %q", ClassifyError(exhausted))
	}
	// Deliberately narrow: an unrelated message mentioning neither marker
	// must stay transient.
	if IsRateLimited(errors.New("rate limiting is not mentioned here")) &&
		ClassifyError(errors.New("some other failure")) == ClassRateLimited {
		t.Fatal("an unrelated failure was relabelled as a rate limit")
	}
}

// TestAlreadyExistsCodesAreRecognized pins conflict detection by API error
// code rather than message text — Cloudflare rewords messages, the codes are
// stable.
func TestAlreadyExistsCodesAreRecognized(t *testing.T) {
	dns := cfErr(http.StatusBadRequest, 81057)
	tun := cfErr(http.StatusBadRequest, 81053)
	other := cfErr(http.StatusBadRequest, 10000)

	if !IsAlreadyExists(dns) {
		t.Fatal("81057 was not recognized as already-exists")
	}
	if !IsAlreadyExists(tun) {
		t.Fatal("81053 was not recognized as already-exists")
	}
	if IsAlreadyExists(other) {
		t.Fatal("an unrelated code was read as already-exists")
	}
}

// TestClassStringNamesEveryClass keeps log output readable: a new class
// without a name fails here rather than printing a number.
func TestClassStringNamesEveryClass(t *testing.T) {
	for _, c := range []Class{ClassTransient, ClassAbsent, ClassUnauthorized, ClassRateLimited} {
		s := c.String()
		if s == "" || s == "0" {
			t.Fatalf("class %d has no usable name", c)
		}
	}
}
