// Package decisionclient — typed decision failures (ADR-0013 D6, M3).
//
// Every failure Decision can classify is a *DecisionError carrying a stable
// kind, the HTTP status (when one exists), whether a workflow-level retry has
// a chance to succeed, and the full error message with the package prefix.
// Adapters map it onto the routed failure outcome without inspecting error
// strings.
package decisionclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DecisionErrorKind classifies why a Decision call failed. The kinds are a
// closed set routed onto adapter failure outcomes:
//
//   - http:      the backend answered with a non-2xx status (retryable only
//     for 429 and the 5xx range);
//   - auth:      the backend rejected the credentials (401/403, never
//     retryable);
//   - timeout:   the call outlived its deadline (retryable);
//   - decode:    the response arrived but is unusable (wrong size, not JSON,
//     wrong envelope — never retryable);
//   - transport: the exchange failed below HTTP (dial/write/read, retryable);
//   - canceled:  the caller aborted the context (never retryable).
type DecisionErrorKind string

const (
	DecisionErrorKindHTTP      DecisionErrorKind = "http"
	DecisionErrorKindAuth      DecisionErrorKind = "auth"
	DecisionErrorKindTimeout   DecisionErrorKind = "timeout"
	DecisionErrorKindDecode    DecisionErrorKind = "decode"
	DecisionErrorKindTransport DecisionErrorKind = "transport"
	DecisionErrorKindCanceled  DecisionErrorKind = "canceled"
)

// DecisionError is the typed form of a failed Decision call.
type DecisionError struct {
	// Kind is the failure class; see [DecisionErrorKind].
	Kind DecisionErrorKind
	// Status is the HTTP status code, or 0 when the failure happened before
	// a status existed (transport, timeout, cancel) or the response carried
	// one (decode defects on a 2xx response).
	Status int
	// Retryable reports whether a workflow-level retry of the whole step has
	// a chance to succeed. It does not imply the transport retried already —
	// that is governed by the retry knob, which stays off by default.
	Retryable bool
	// Message is the full error text, already carrying the package prefix.
	Message string

	// retryAfter is the wait the failed response prescribed via its
	// Retry-After header; zero when absent or unparseable. It is honored
	// internally by Decision between retry attempts (429/5xx only) and is
	// not part of the routed payload.
	retryAfter time.Duration

	// cause preserves error identity for errors.As/Is through the typed
	// form.
	cause error
}

// Error implements error.
func (e *DecisionError) Error() string { return e.Message }

// Unwrap exposes the underlying cause, if any, so callers can errors.Is
// against e.g. context deadlines through the typed form. Only transport,
// timeout, and canceled errors carry a cause.
func (e *DecisionError) Unwrap() error { return e.cause }

// asDecisionError returns err as a *DecisionError, wrapping plain errors as
// decode-kind failures (the least retryable assumption for an unexpected
// error shape).
func asDecisionError(err error) *DecisionError {
	var de *DecisionError
	if errors.As(err, &de) {
		return de
	}
	return &DecisionError{
		Kind:      DecisionErrorKindDecode,
		Retryable: false,
		Message:   err.Error(),
		cause:     err,
	}
}

// isRetryStatus reports whether the status belongs to the transport retry
// set (429 and the 5xx range, ADR-0013 D5).
func isRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status < 600)
}

// transportError classifies a failed HTTP exchange. Deadline exhaustion and
// net timeouts are timeouts (retryable); caller cancellation is its own
// never-retryable kind; anything else is transport (retryable).
func transportError(endpoint string, err error) *DecisionError {
	de := &DecisionError{
		Status: 0,
		// fmt.Stringer on the wrapped error keeps the message identical to
		// the historical "POST <endpoint>: <cause>" form.
		Message: fmt.Sprintf(errPrefix+"POST %s: %v", endpoint, err),
		cause:   err,
	}
	switch {
	case errors.Is(err, context.Canceled):
		de.Kind = DecisionErrorKindCanceled
		de.Retryable = false
	case errors.Is(err, context.DeadlineExceeded):
		de.Kind = DecisionErrorKindTimeout
		de.Retryable = true
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			de.Kind = DecisionErrorKindTimeout
			de.Retryable = true
		} else {
			de.Kind = DecisionErrorKindTransport
			de.Retryable = true
		}
	}
	return de
}

// statusError classifies a non-2xx response, carrying the Retry-After wait
// the response prescribed for the transport retry knob.
func statusError(endpoint string, resp *http.Response, excerpt string) *DecisionError {
	de := &DecisionError{
		Status:     resp.StatusCode,
		Message:    fmt.Sprintf(errPrefix+"POST %s: unexpected status %s: %s", endpoint, resp.Status, excerpt),
		retryAfter: retryAfterOf(resp.Header.Get("Retry-After"), time.Now()),
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		de.Kind = DecisionErrorKindAuth
		de.Retryable = false
		return de
	}
	de.Kind = DecisionErrorKindHTTP
	de.Retryable = isRetryStatus(resp.StatusCode)
	return de
}

// decodeError builds a decode-kind failure (response arrived but is
// unusable). Decode failures are deterministic for the same request, so they
// are never retryable. The optional cause preserves error identity through
// [DecisionError.Unwrap].
func decodeError(message string, cause ...error) *DecisionError {
	de := &DecisionError{
		Kind:      DecisionErrorKindDecode,
		Status:    0,
		Retryable: false,
		Message:   message,
	}
	if len(cause) == 1 {
		de.cause = cause[0]
	}
	return de
}

// retryAfterOf parses a Retry-After header value: a delta-seconds integer
// (never negative) or an HTTP date. Unparseable or already-elapsed values
// yield zero — the caller retries immediately instead of inventing a wait.
func retryAfterOf(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds == 0 {
			return 0
		}
		// time.Duration(seconds) * time.Second overflows for huge deltas
		// (and would land in the past); clamp to the representable span.
		// Context deadlines remain the practical bound for callers.
		const maxSeconds = uint64(math.MaxInt64 / int64(time.Second))
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		return time.Duration(seconds) * time.Second
	}
	if stamp, err := http.ParseTime(value); err == nil {
		if delay := stamp.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}
