// decisionclient/errors_test.go — typed error classification and the
// transport retry knob (ADR-0013 D6/D5, M3).

package decisionclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusStub is a scripted backend: it replays the scripted exchanges in
// order and records how many requests arrived.
type statusStub struct {
	server    *httptest.Server
	mu        sync.Mutex
	exchanges []stubExchange
	requests  int
}

// stubExchange is one scripted response.
type stubExchange struct {
	status     int
	body       string
	retryAfter string // optional Retry-After header value
}

func newStatusStub(t *testing.T, exchanges ...stubExchange) *statusStub {
	t.Helper()
	stub := &statusStub{exchanges: exchanges}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		x := stub.exchanges[0]
		stub.requests++
		// Replay the last scripted exchange forever (sticky tail).
		if len(stub.exchanges) > 1 {
			stub.exchanges = stub.exchanges[1:]
		}
		if x.retryAfter != "" {
			w.Header().Set("Retry-After", x.retryAfter)
		}
		w.WriteHeader(x.status)
		_, _ = fmt.Fprint(w, x.body)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *statusStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// TestDecisionErrorMatrix pins the typed mapping Decision reports for every
// failure class the adapter routes onto outcome failure: kind, status, and
// retryability, with the legacy message text intact.
func TestDecisionErrorMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		response      stubExchange
		cancelContext bool
		wantKind      DecisionErrorKind
		wantStatus    int
		wantRetryable bool
		wantMsgSubs   []string
	}{
		{
			name:          "internal error",
			response:      stubExchange{status: 500, body: `{"error":"boom"}`},
			wantKind:      DecisionErrorKindHTTP,
			wantStatus:    500,
			wantRetryable: true,
			wantMsgSubs:   []string{"unexpected status 500", `{"error":"boom"}`},
		},
		{
			name:          "unavailable",
			response:      stubExchange{status: 503, body: ""},
			wantKind:      DecisionErrorKindHTTP,
			wantStatus:    503,
			wantRetryable: true,
			wantMsgSubs:   []string{"unexpected status 503", "(empty body)"},
		},
		{
			name:          "rate limited",
			response:      stubExchange{status: 429, body: `{"error":"slow down"}`},
			wantKind:      DecisionErrorKindHTTP,
			wantStatus:    429,
			wantRetryable: true,
			wantMsgSubs:   []string{"unexpected status 429"},
		},
		{
			name:          "not found",
			response:      stubExchange{status: 404, body: "nope"},
			wantKind:      DecisionErrorKindHTTP,
			wantStatus:    404,
			wantRetryable: false,
			wantMsgSubs:   []string{"unexpected status 404"},
		},
		{
			name:          "unauthorized",
			response:      stubExchange{status: 401, body: `{"error":"bad key"}`},
			wantKind:      DecisionErrorKindAuth,
			wantStatus:    401,
			wantRetryable: false,
			wantMsgSubs:   []string{"unexpected status 401"},
		},
		{
			name:          "forbidden",
			response:      stubExchange{status: 403, body: ""},
			wantKind:      DecisionErrorKindAuth,
			wantStatus:    403,
			wantRetryable: false,
			wantMsgSubs:   []string{"unexpected status 403"},
		},
		{
			name:          "not valid JSON",
			response:      stubExchange{status: 200, body: `{"state": unquoted`},
			wantKind:      DecisionErrorKindDecode,
			wantStatus:    0,
			wantRetryable: false,
			wantMsgSubs:   []string{"not valid JSON"},
		},
		{
			name:          "canceled context",
			response:      stubExchange{status: 200, body: "{}"},
			cancelContext: true,
			wantKind:      DecisionErrorKindCanceled,
			wantStatus:    0,
			wantRetryable: false,
			wantMsgSubs:   []string{"context canceled"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newStatusStub(t, tc.response)
			client := newTestClient(t, stub.server.URL, "clef")

			ctx := context.Background()
			if tc.cancelContext {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}
			_, err := client.Decision(ctx, "ready", testQuestions())
			if err == nil {
				t.Fatal("Decision() err = nil; want typed failure")
			}

			var de *DecisionError
			if !errors.As(err, &de) {
				t.Fatalf("Decision() err = %v; want *DecisionError", err)
			}
			if de.Kind != tc.wantKind {
				t.Errorf("Kind = %q; want %q", de.Kind, tc.wantKind)
			}
			if de.Status != tc.wantStatus {
				t.Errorf("Status = %d; want %d", de.Status, tc.wantStatus)
			}
			if de.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v; want %v", de.Retryable, tc.wantRetryable)
			}
			for _, want := range tc.wantMsgSubs {
				if !strings.Contains(de.Message, want) {
					t.Errorf("Message = %q; want containing %q", de.Message, want)
				}
			}
		})
	}
}

// TestDecisionRetryDisabledByDefault pins the D5 default exactly: without an
// explicit retry knob a 429 fails the call after a single attempt.
func TestDecisionRetryDisabledByDefault(t *testing.T) {
	t.Parallel()

	stub := newStatusStub(t,
		stubExchange{status: 429, body: `{"error":"slow down"}`, retryAfter: "1"},
		stubExchange{status: 200, body: "{}"},
	)
	client := newTestClient(t, stub.server.URL, "clef")

	_, err := client.Decision(context.Background(), "ready", testQuestions())
	if err == nil {
		t.Fatal("Decision() err = nil; want 429 failure with retries disabled")
	}
	if got := stub.count(); got != 1 {
		t.Errorf("request count = %d; want exactly 1 (retry off by default)", got)
	}
	de := asDecisionError(err)
	if de.Kind != DecisionErrorKindHTTP || de.Status != 429 || !de.Retryable {
		t.Errorf("typed error = %+v; want http/429/retryable", de)
	}
}

// TestDecisionRetryAttempts429And5xx covers the two retried classes with a
// scripted failure-then-success pair, plus exhaustion and non-retry classes.
func TestDecisionRetryAttempts429And5xx(t *testing.T) {
	t.Parallel()

	t.Run("429 then success", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 429, body: "", retryAfter: "0"},
			stubExchange{status: 200, body: `{"verdict":"ok"}`},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(2)

		raw, err := client.Decision(context.Background(), "ready", testQuestions())
		if err != nil {
			t.Fatalf("Decision() err = %v; want nil after retry", err)
		}
		if string(raw) != `{"verdict":"ok"}` {
			t.Errorf("raw = %s; want %s", raw, `{"verdict":"ok"}`)
		}
		if got := stub.count(); got != 2 {
			t.Errorf("request count = %d; want 2", got)
		}
	})

	t.Run("5xx then success", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 502, body: "bad gateway"},
			stubExchange{status: 200, body: "{}"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(1)

		if _, err := client.Decision(context.Background(), "ready", testQuestions()); err != nil {
			t.Fatalf("Decision() err = %v; want nil after retry", err)
		}
		if got := stub.count(); got != 2 {
			t.Errorf("request count = %d; want 2", got)
		}
	})

	t.Run("exhaustion keeps the last typed failure", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 500, body: "boom-1"},
			stubExchange{status: 500, body: "boom-2"},
			stubExchange{status: 500, body: "boom-final"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(2)

		_, err := client.Decision(context.Background(), "ready", testQuestions())
		if err == nil {
			t.Fatal("Decision() err = nil; want exhausted-retry failure")
		}
		if got := stub.count(); got != 3 {
			t.Errorf("request count = %d; want 3 (1 + 2 retries)", got)
		}
		de := asDecisionError(err)
		if de.Kind != DecisionErrorKindHTTP || de.Status != 500 || !de.Retryable {
			t.Errorf("typed error = %+v; want http/500/retryable", de)
		}
		if !strings.Contains(de.Message, "boom-final") {
			t.Errorf("Message = %q; want the final attempt's body", de.Message)
		}
	})

	t.Run("auth failures never retry", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t, stubExchange{status: 401, body: ""})
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(5)

		if _, err := client.Decision(context.Background(), "ready", testQuestions()); err == nil {
			t.Fatal("Decision() err = nil; want 401 failure")
		}
		if got := stub.count(); got != 1 {
			t.Errorf("request count = %d; want 1 (no retry on auth)", got)
		}
	})

	t.Run("client errors never retry", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t, stubExchange{status: 404, body: ""})
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(5)

		if _, err := client.Decision(context.Background(), "ready", testQuestions()); err == nil {
			t.Fatal("Decision() err = nil; want 404 failure")
		}
		if got := stub.count(); got != 1 {
			t.Errorf("request count = %d; want 1 (no retry on 404)", got)
		}
	})
}

// TestDecisionRetryHonorsRetryAfter pins the Retry-After contract: an
// integer wait is slept before the retry (minus the test tolerance), an
// HTTP-date is supported, and a cutoff context deadline aborts the wait with
// a timeout failure instead of waiting it out.
func TestDecisionRetryHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	t.Run("integer delta-seconds", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 429, body: "", retryAfter: "1"},
			stubExchange{status: 200, body: "{}"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(1)

		start := time.Now()
		if _, err := client.Decision(context.Background(), "ready", testQuestions()); err != nil {
			t.Fatalf("Decision() err = %v; want nil after honoring Retry-After", err)
		}
		if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
			t.Errorf("elapsed = %v; want >= 900ms (Retry-After honored)", elapsed)
		}
		if got := stub.count(); got != 2 {
			t.Errorf("request count = %d; want 2", got)
		}
	})

	t.Run("HTTP date", func(t *testing.T) {
		t.Parallel()
		date := time.Now().Add(2 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
		stub := newStatusStub(t,
			stubExchange{status: 503, body: "", retryAfter: date},
			stubExchange{status: 200, body: "{}"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(1)

		if _, err := client.Decision(context.Background(), "ready", testQuestions()); err != nil {
			t.Fatalf("Decision() err = %v; want nil after honoring Retry-After date", err)
		}
	})

	t.Run("deadline cuts the backoff short", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 429, body: "", retryAfter: "60"},
			stubExchange{status: 200, body: "{}"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(1)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := client.Decision(ctx, "ready", testQuestions())
		if err == nil {
			t.Fatal("Decision() err = nil; want the deadline to cut the backoff")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("elapsed = %v; want far below the 60s Retry-After", elapsed)
		}
		de := asDecisionError(err)
		if de.Kind != DecisionErrorKindTimeout || !de.Retryable {
			t.Errorf("typed error = %+v; want timeout/retryable", de)
		}
		if got := stub.count(); got != 1 {
			t.Errorf("request count = %d; want 1", got)
		}
	})

	t.Run("canceled context is not retryable", func(t *testing.T) {
		t.Parallel()
		stub := newStatusStub(t,
			stubExchange{status: 429, body: "", retryAfter: "60"},
		)
		client := newTestClient(t, stub.server.URL, "clef").WithRetries(1)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Decision(ctx, "ready", testQuestions())
		if err == nil {
			t.Fatal("Decision() err = nil; want cancel to abort the call")
		}
		de := asDecisionError(err)
		if de.Kind != DecisionErrorKindCanceled || de.Retryable {
			t.Errorf("typed error = %+v; want canceled/not-retryable", de)
		}
	})
}

// TestWithRetriesDoesNotMutateReceiver pins the copy semantics of the knob:
// the original client stays on its own configured retry count.
func TestWithRetriesDoesNotMutateReceiver(t *testing.T) {
	t.Parallel()

	stub := newStatusStub(t,
		stubExchange{status: 429, body: ""},
		stubExchange{status: 200, body: "{}"},
		stubExchange{status: 429, body: ""},
		stubExchange{status: 200, body: "{}"},
	)
	original := newTestClient(t, stub.server.URL, "clef")
	withRetry := original.WithRetries(1)

	if _, err := withRetry.Decision(context.Background(), "ready", testQuestions()); err != nil {
		t.Fatalf("retried client Decision() err = %v; want nil", err)
	}
	if _, err := original.Decision(context.Background(), "ready", testQuestions()); err == nil {
		t.Fatal("original client Decision() err = nil; want 429 failure (still retries=0)")
	}
	if got := stub.count(); got != 3 {
		t.Errorf("request count = %d; want 3 (retried client: 2, original: 1)", got)
	}
}

// TestRetryAfterOfParsing covers the header parser standalone.
func TestRetryAfterOfParsing(t *testing.T) {
	t.Parallel()

	now := time.Date(2030, 5, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value    string
		want     time.Duration
		wantZero bool
	}{
		{value: "", wantZero: true},
		{value: "0", wantZero: true},
		{value: "30", want: 30 * time.Second},
		{value: "  30  ", want: 30 * time.Second},
		{value: "-5", wantZero: true},                          // negative deltas are invalid
		{value: "1.5", wantZero: true},                         // fractional deltas are invalid
		{value: "soon", wantZero: true},                        // unparseable → immediate retry
		{value: "9999999999999999999999", wantZero: true},      // range-overflows uint64 → immediate retry
		{value: "20000000000", want: 9223372036 * time.Second}, // clamped, no overflow
		{value: now.Add(-2 * time.Minute).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), wantZero: true}, // elapsed date
		{value: now.Add(time.Minute).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), want: time.Minute},
	}

	for _, tc := range tests {
		got := retryAfterOf(tc.value, now)
		if tc.wantZero {
			if got != 0 {
				t.Errorf("retryAfterOf(%q) = %v; want 0", tc.value, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("retryAfterOf(%q) = %v; want %v", tc.value, got, tc.want)
		}
	}
}
