// Package cferr classifies errors from the cloudflare-go client.
//
// This is the single authority for what a failed Cloudflare API call means.
// The tunnel, DNS, Access managers and the adapter's observation classifier
// must all agree, because callers act on the difference:
//
//   - Absent: an authoritative 404/410. The resource is gone. Callers may
//     recreate it or treat cleanup as done.
//   - Unauthorized: 401/403. Says nothing about whether the resource exists.
//     Acting would guess; report instead.
//   - RateLimited: a 429, or the client's own retry-exhaustion error (the
//     client retries 429 internally and reports exhaustion without carrying
//     the status). The answer is to wait, not to recreate anything.
//   - Transient: 5xx, network failures, timeouts, cancellations, malformed
//     responses — everything else. Never evidence of absence.
//
// This package is deliberately the only place that inspects cf.Error shapes.
// When cloudflare-go changes how errors are represented — it has before: the
// wrapped-error change silently broke direct type assertions across this
// repository — this is the one file to fix.
package cferr

import (
	"errors"
	"net/http"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
)

// Class is the meaning of an API failure.
type Class int

const (
	// ClassTransient means the outcome is unknown or retryable. It is the
	// zero value so a mistake reads as "retry later", never as "delete it".
	ClassTransient Class = iota
	// ClassAbsent authoritatively means the resource does not exist.
	ClassAbsent
	// ClassUnauthorized means the credential was rejected or lacks permission.
	ClassUnauthorized
	// ClassRateLimited means Cloudflare throttled the caller. Wait; do not act.
	ClassRateLimited
)

// String names the class for logs and user-facing detail strings.
func (c Class) String() string {
	switch c {
	case ClassAbsent:
		return "absent"
	case ClassUnauthorized:
		return "unauthorized"
	case ClassRateLimited:
		return "rate_limited"
	default:
		return "transient"
	}
}

// IsAbsent reports whether err authoritatively means "the resource does not
// exist". Only 404 and 410 qualify: any other failure says nothing about
// existence, and treating it as absence recreates resources that exist.
func IsAbsent(err error) bool {
	var cfErr *cf.Error
	if errors.As(err, &cfErr) {
		return cfErr.StatusCode == http.StatusNotFound || cfErr.StatusCode == http.StatusGone
	}
	return false
}

// IsUnauthorized reports whether err means the credential was rejected or
// lacks permission for the attempted operation.
func IsUnauthorized(err error) bool {
	var cfErr *cf.Error
	if !errors.As(err, &cfErr) {
		return false
	}
	return cfErr.StatusCode == http.StatusUnauthorized || cfErr.StatusCode == http.StatusForbidden
}

// IsRateLimited reports whether err means Cloudflare throttled the caller,
// either as a live 429 or as client-side retry exhaustion.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var cfErr *cf.Error
	if errors.As(err, &cfErr) && cfErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "rate limit") &&
		(strings.Contains(message, "retries") || strings.Contains(message, "exceeded"))
}

// Well-known Cloudflare API error codes. Matching on codes rather than
// message strings keeps classification stable when Cloudflare rewords
// messages; these code meanings have been stable for years.
const (
	// codeDNSRecordAlreadyExists is returned by DNS create when a record
	// with the same name/type already exists (code 81057).
	codeDNSRecordAlreadyExists = 81057
	// codeTunnelNameInUse is returned by tunnel create when a
	// not-deleted tunnel with the same name already exists (code 81053).
	codeTunnelNameInUse = 81053
)

// HasErrorCode reports whether err carries the given Cloudflare API error
// code anywhere in its wrapped chain.
func HasErrorCode(err error, code int) bool {
	var cfErr *cf.Error
	if !errors.As(err, &cfErr) {
		return false
	}
	for _, c := range cfErr.ErrorCodes {
		if c == code {
			return true
		}
	}
	return false
}

// IsAlreadyExists reports whether err means the resource Portico tried to
// create already exists under that name/identity. This is a *recoverable*
// failure: it usually means an earlier create attempt succeeded but its
// outcome was never recorded (timeout after the request was sent). Callers
// can look the resource up and adopt or delete it instead of failing.
func IsAlreadyExists(err error) bool {
	return HasErrorCode(err, codeDNSRecordAlreadyExists) || HasErrorCode(err, codeTunnelNameInUse)
}

// ClassifyError maps an API error to its class. A nil error classifies as
// transient — callers should not ask about success, and a mistaken query
// must read as "retry later", never as "delete it".
func ClassifyError(err error) Class {
	switch {
	case err == nil:
		return ClassTransient
	case IsAbsent(err):
		return ClassAbsent
	case IsUnauthorized(err):
		return ClassUnauthorized
	case IsRateLimited(err):
		return ClassRateLimited
	default:
		return ClassTransient
	}
}

// IsMalformedResponse reports whether err came from the client failing to
// parse an API response — for example a proxy's HTML error page returned
// with a 200. Callers use it to explain transient failures accurately
// instead of reporting them as provider outages.
func IsMalformedResponse(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unmarshalling") ||
		strings.Contains(message, "unmarshal")
}
