package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria-adapter-decision/decisionclient"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// captureSink collects events emitted by Execute.
type captureSink struct{ events []*v2.ExecuteEvent }

func (c *captureSink) Send(e *v2.ExecuteEvent) error {
	c.events = append(c.events, e)
	return nil
}

// captureLogSender collects events emitted by Log.
type captureLogSender struct{ events []*v2.LogEvent }

func (c *captureLogSender) Send(e *v2.LogEvent) error {
	c.events = append(c.events, e)
	return nil
}

func newTestService() *decisionService {
	return &decisionService{sessions: map[string]*decisionSession{}}
}

func TestInfo(t *testing.T) {
	resp, err := newTestService().Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != "decision" {
		t.Errorf("name = %q, want decision", resp.GetName())
	}
	if resp.GetVersion() != Version {
		t.Errorf("version = %q, want %q", resp.GetVersion(), Version)
	}
	if resp.GetSourceUrl() == "" {
		t.Error("source_url must be set for publishing")
	}
	if len(resp.GetPlatforms()) == 0 {
		t.Error("platforms must be set for a multi-arch publish")
	}
	if resp.GetSdkProtocolVersion() != "2" {
		t.Errorf("sdk_protocol_version = %q, want 2", resp.GetSdkProtocolVersion())
	}
	if _, declared := resp.GetSecrets()[apiKeyName]; !declared {
		t.Errorf("secrets = %v; api_key must be declared so hosts resolve it", resp.GetSecrets())
	}
}

// executeValidQuestionsJSON is a minimal valid Input for Execute: one noul
// question. The state is supplied per test.
const executeValidQuestionsJSON = `[{"id":"verdict","type":"noul","instructions":"Is the run safe?"}]`

// answerVerbatim is the verbatim answers array the test backend returns,
// deliberately padded with asymmetric whitespace to pin byte-verbatim
// passthrough.
const answerVerbatim = `[ {"id":"verdict","noul":"yes"} ]`

// usageVerbatim is the verbatim usage object the test backend returns, with
// deliberately uneven internal formatting to pin byte-verbatim passthrough.
const usageVerbatim = `{"input_tokens": 812, "output_tokens":42, "note":"done" }`

func successResponse() string {
	return `{"model":"sysone-1","answers":` + answerVerbatim + `,"usage":` + usageVerbatim + `}`
}

// stubExchange scripts one stub-backend exchange.
type stubExchange struct {
	status int               // response status (0 = 200)
	body   string            // response body
	header map[string]string // response headers (e.g. Retry-After)
}

// stubBackend returns a decision service with session "s1" wired to a
// scripted HTTP backend. Exchanges are consumed in order; the last one is
// sticky (replayed for every further request). The returned counter records
// every request seen.
func stubBackend(t *testing.T, exchanges ...stubExchange) (*decisionService, *int32) {
	return stubBackendWithConfig(t, nil, exchanges...)
}

// stubBackendWithConfig is stubBackend with the session opened carrying the
// given extra config keys (e.g. the outcome_question mapping key).
func stubBackendWithConfig(t *testing.T, extraConfig map[string]string, exchanges ...stubExchange) (*decisionService, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&calls, 1) - 1
		if int(i) >= len(exchanges) {
			i = int32(len(exchanges) - 1)
		}
		ex := exchanges[i]
		status := ex.status
		if status == 0 {
			status = http.StatusOK
		}
		for key, value := range ex.header {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(ex.body))
	}))
	t.Cleanup(srv.Close)
	s := newTestService()
	openDecisionSession(t, s, srv.URL, extraConfig, nil)
	return s, &calls
}

// stubBackendWithAuth is stubBackend whose handler also records the latest
// Authorization header it received.
func stubBackendWithAuth(t *testing.T, exchanges ...stubExchange) (*decisionService, *int32, *atomic.Value) {
	t.Helper()
	var calls int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&calls, 1) - 1
		if int(i) >= len(exchanges) {
			i = int32(len(exchanges) - 1)
		}
		ex := exchanges[i]
		status := ex.status
		if status == 0 {
			status = http.StatusOK
		}
		for key, value := range ex.header {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(ex.body))
		auth.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	s := newTestService()
	openDecisionSession(t, s, srv.URL, nil, nil)
	return s, &calls, &auth
}

// openDecisionSession opens session "s1" with a valid minimal config against
// baseURL, merged with the given extra config keys and secrets.
func openDecisionSession(t *testing.T, s *decisionService, baseURL string, extra map[string]string, secrets map[string]string) {
	t.Helper()
	config := map[string]string{configKeyBaseURL: baseURL, configKeyModel: "sysone-1"}
	for key, value := range extra {
		config[key] = value
	}
	request := &v2.OpenSessionRequest{SessionId: "s1", Config: config, Secrets: secrets}
	if _, err := s.OpenSession(context.Background(), request); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
}

// callExecute runs one step over session "s1" and captures the events.
func callExecute(t *testing.T, s *decisionService, input, secretInputs map[string]string, allowed []string) (*captureSink, error) {
	t.Helper()
	req := &v2.ExecuteRequest{SessionId: "s1", Input: input, SecretInputs: secretInputs, AllowedOutcomes: allowed}
	sink := &captureSink{}
	err := s.Execute(context.Background(), req, sink)
	return sink, err
}

// validInput builds the minimal valid input with the given state JSON.
func validInput(state string) map[string]string {
	return validInputWithQuestions(executeValidQuestionsJSON, state)
}

// validInputWithQuestions builds a valid Execute input from the given
// questions and state JSON.
func validInputWithQuestions(questions, state string) map[string]string {
	return map[string]string{"questions": questions, "state": state}
}

// TestExecuteInputRequired pins that the questions and state input keys are
// mandatory (no defaults).
func TestExecuteInputRequired(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]string
		wantErr string
	}{
		{name: "no input at all", input: nil, wantErr: `input must carry a "questions" key`},
		{name: "questions only", input: map[string]string{"questions": executeValidQuestionsJSON}, wantErr: `input must carry a "state" key`},
		{name: "state only", input: map[string]string{"state": `"ready"`}, wantErr: `input must carry a "questions" key`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := stubBackend(t, stubExchange{body: successResponse()})
			sink, err := callExecute(t, s, tc.input, nil, nil)
			if err == nil {
				t.Fatalf("Execute(...) err = nil; want containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute(...) err = %q; want containing %q", err.Error(), tc.wantErr)
			}
			if got := len(sink.events); got != 0 {
				t.Fatalf("Execute emitted %d events; want 0 (fail-closed)", got)
			}
		})
	}
}

// TestExecuteInputRejectMatrix is the Execute-level reject matrix: 100% of
// the question and state reject cases fail the step (before any decision
// work — the backend sees zero requests), never default or pass through.
func TestExecuteInputRejectMatrix(t *testing.T) {
	const st = `"s"`
	tests := []struct {
		name                     string
		questions                string
		state                    string
		wantErr                  string
		wantDecisionclientPrefix bool
	}{
		// questions rejects.
		{name: "questions not an array", questions: `{"id":"q1"}`, state: st, wantErr: "questions must be a JSON array of question objects", wantDecisionclientPrefix: true},
		{name: "questions empty array", questions: `[]`, state: st, wantErr: "at least one question", wantDecisionclientPrefix: true},
		{name: "questions empty string", questions: ``, state: st, wantErr: "questions must be a JSON array", wantDecisionclientPrefix: true},
		{name: "bad question type", questions: `[{"id":"q1","type":"emoji","instructions":"g"}]`, state: st, wantErr: `type must be "choice", "score", "noul"`, wantDecisionclientPrefix: true},
		{name: "empty instructions", questions: `[{"id":"q1","type":"noul","instructions":""}]`, state: st, wantErr: "instructions is required", wantDecisionclientPrefix: true},
		{name: "choice without criteria", questions: `[{"id":"q1","type":"choice","instructions":"g"}]`, state: st, wantErr: "choice questions require criteria", wantDecisionclientPrefix: true},
		{name: "score criteria as map (wrong shape)", questions: `[{"id":"q1","type":"score","instructions":"g","criteria":{"low":1}}]`, state: st, wantErr: "not an object", wantDecisionclientPrefix: true},
		{name: "unknown key in question object", questions: `[{"id":"q1","type":"noul","instructions":"g","priority":1}]`, state: st, wantErr: `unknown field "priority"`, wantDecisionclientPrefix: true},
		{name: "non-bareword id", questions: `[{"id":"does-run-pass","type":"noul","instructions":"g"}]`, state: st, wantErr: "id must be a bareword", wantDecisionclientPrefix: true},
		{name: "duplicate question ids", questions: `[{"id":"q1","type":"noul","instructions":"a"},{"id":"q1","type":"noul","instructions":"b"}]`, state: st, wantErr: `duplicate question id "q1"`, wantDecisionclientPrefix: true},
		{name: "questions trailing data", questions: `[] {"x":1}`, state: st, wantErr: "questions must be a JSON array", wantDecisionclientPrefix: true},

		// state rejects.
		{name: "state is null", questions: executeValidQuestionsJSON, state: `null`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state is a number", questions: executeValidQuestionsJSON, state: `3`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state is a boolean", questions: executeValidQuestionsJSON, state: `true`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state malformed", questions: executeValidQuestionsJSON, state: `{"a":`, wantErr: "invalid state JSON", wantDecisionclientPrefix: true},
		{name: "state trailing data", questions: executeValidQuestionsJSON, state: `"ready" ""`, wantErr: "trailing data", wantDecisionclientPrefix: true},
		{name: "state empty", questions: executeValidQuestionsJSON, state: ``, wantErr: "state is required", wantDecisionclientPrefix: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, calls := stubBackend(t, stubExchange{body: successResponse()})
			sink, err := callExecute(t, svc, map[string]string{"questions": tc.questions, "state": tc.state}, nil, nil)
			if err == nil {
				t.Fatalf("Execute(...) err = nil; want containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute(...) err = %q; want containing %q", err.Error(), tc.wantErr)
			}
			if tc.wantDecisionclientPrefix && !strings.HasPrefix(err.Error(), "decisionclient: ") {
				t.Fatalf("Execute(...) err = %q; want decisionclient-prefixed validation error", err.Error())
			}
			if got := len(sink.events); got != 0 {
				t.Fatalf("Execute emitted %d events; want 0 (fail-closed)", got)
			}
			if got := atomic.LoadInt32(calls); got != 0 {
				t.Fatalf("backend saw %d requests; want 0 (validation precedes any HTTP)", got)
			}
		})
	}
}

// captureRun asserts the terminal shape of an Execute run: exactly one
// ExecuteResult as the last event and exactly wantAdapter adapter events,
// and returns (adapterEvent, result).
func captureRun(t *testing.T, sink *captureSink, wantAdapter int) (*v2.AdapterEvent, *v2.ExecuteResult) {
	t.Helper()
	var adapterEvents []*v2.ExecuteEvent
	var results []*v2.ExecuteResult
	for _, ev := range sink.events {
		switch ev.GetEvent().(type) {
		case *v2.ExecuteEvent_Adapter:
			adapterEvents = append(adapterEvents, ev)
		case *v2.ExecuteEvent_Result:
			results = append(results, ev.GetResult())
		default:
			t.Fatalf("unexpected event type %T", ev.GetEvent())
		}
	}
	if len(results) != 1 {
		t.Fatalf("Execute emitted %d ExecuteResult events; want exactly 1", len(results))
	}
	if len(adapterEvents) != wantAdapter {
		t.Fatalf("Execute emitted %d adapter events; want exactly %d (one per call)", len(adapterEvents), wantAdapter)
	}
	if _, ok := sink.events[len(sink.events)-1].GetEvent().(*v2.ExecuteEvent_Result); !ok {
		t.Fatal("the ExecuteResult must be the terminal event")
	}
	return adapterEvents[0].GetAdapter(), results[0]
}

// failurePayloadOf is the typed error payload carried by a failure result.
type failurePayloadOf struct {
	Error decisionclient.DecisionError `json:"error"`
}

// TestExecuteSuccessPassthroughVerbatim pins the success contract: outcome
// "success" (always), the outputs are exactly {"answers":A,"usage":U} with
// A and U byte-verbatim from the backend, and exactly one adapter_event
// carries the model id, per-question answers, usage, and latency — never
// the step state.
func TestExecuteSuccessPassthroughVerbatim(t *testing.T) {
	s, calls := stubBackend(t, stubExchange{body: successResponse()})
	sink, err := callExecute(t, s, validInput(`"ready"`), nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	adapter, result := captureRun(t, sink, 1)
	if result.GetOutcome() != outcomeSuccess {
		t.Errorf("outcome = %q; want %q (always success)", result.GetOutcome(), outcomeSuccess)
	}
	wantOutputs := `{"answers":` + answerVerbatim + `,"usage":` + usageVerbatim + `}`
	if got := string(result.GetOutputsJson()); got != wantOutputs {
		t.Errorf("outputs =\n%s\nwant byte-verbatim\n%s", got, wantOutputs)
	}

	if adapter.GetEventKind() != eventDecisionCompleted {
		t.Errorf("adapter event kind = %q; want %q", adapter.GetEventKind(), eventDecisionCompleted)
	}
	payload := adapter.GetPayload().AsMap()
	if got := payload["model"]; got != "sysone-1" {
		t.Errorf("event model = %v; want sysone-1", got)
	}
	// The answers ride the event as the native per-question view.
	answers, ok := payload["answers"].([]any)
	if !ok || len(answers) != 1 {
		t.Fatalf("event answers = %#v; want one native answer object", payload["answers"])
	}
	answer, ok := answers[0].(map[string]any)
	if !ok || answer["id"] != "verdict" || answer["noul"] != "yes" {
		t.Errorf("event answer = %#v; want the per-question answer", answers[0])
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(812) {
		t.Errorf("event usage = %#v; want the native usage object", payload["usage"])
	}
	if _, ok := payload["latency_ms"].(float64); !ok || payload["latency_ms"].(float64) < 0 {
		t.Errorf("event latency_ms = %#v; want a non-negative number", payload["latency_ms"])
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), "ready") {
		t.Errorf("event payload %s must not carry the step state", encoded)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("backend saw %d requests; want 1 (retry default OFF)", got)
	}
}

// TestExecuteFailureMappingMatrix is the routed-failure contract: every
// failure class the decisionclient classifies becomes outcome "failure"
// with the typed error payload {error.kind, error.status, error.message,
// error.retryable} — no engine-visible error in any routed path.
func TestExecuteFailureMappingMatrix(t *testing.T) {
	tests := []struct {
		name        string
		exchanges   []stubExchange
		wantKind    string
		wantStatus  int
		wantRetry   bool
		wantMsgPart string
	}{
		{
			name:        "429 is http retryable",
			exchanges:   []stubExchange{{status: http.StatusTooManyRequests, body: "slow down", header: map[string]string{"Retry-After": "0"}}},
			wantKind:    "http",
			wantStatus:  429,
			wantRetry:   true,
			wantMsgPart: "unexpected status 429",
		},
		{
			name:        "503 is http retryable",
			exchanges:   []stubExchange{{status: http.StatusServiceUnavailable, body: "down"}},
			wantKind:    "http",
			wantStatus:  503,
			wantRetry:   true,
			wantMsgPart: "unexpected status 503",
		},
		{
			name:        "400 is http not retryable",
			exchanges:   []stubExchange{{status: http.StatusBadRequest, body: "bad"}},
			wantKind:    "http",
			wantStatus:  400,
			wantRetry:   false,
			wantMsgPart: "unexpected status 400",
		},
		{
			name:        "401 is auth never retryable",
			exchanges:   []stubExchange{{status: http.StatusUnauthorized, body: "who are you"}},
			wantKind:    "auth",
			wantStatus:  401,
			wantRetry:   false,
			wantMsgPart: "unexpected status 401",
		},
		{
			name:        "403 is auth never retryable",
			exchanges:   []stubExchange{{status: http.StatusForbidden, body: "denied"}},
			wantKind:    "auth",
			wantStatus:  403,
			wantRetry:   false,
			wantMsgPart: "unexpected status 403",
		},
		{
			name:        "malformed response body is decode",
			exchanges:   []stubExchange{{body: `{"model":"sysone-1","answers":[{"id":"verdict","noul":"hmm"}]}`}},
			wantKind:    "decode",
			wantStatus:  0,
			wantRetry:   false,
			wantMsgPart: `decode response: response "usage" is required`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := stubBackend(t, tc.exchanges...)
			sink, err := callExecute(t, s, validInput(`"ready"`), nil, nil)
			if err != nil {
				t.Fatalf("Execute: %v (routed paths must not return engine-visible errors)", err)
			}
			adapter, result := captureRun(t, sink, 1)
			if adapter.GetEventKind() != eventDecisionFailed {
				t.Errorf("adapter event kind = %q; want %q", adapter.GetEventKind(), eventDecisionFailed)
			}
			if result.GetOutcome() != outcomeFailure {
				t.Fatalf("outcome = %q; want %q", result.GetOutcome(), outcomeFailure)
			}
			var payload failurePayloadOf
			if err := json.Unmarshal(result.GetOutputsJson(), &payload); err != nil {
				t.Fatalf("failure payload is not the typed error object: %v (%s)", err, result.GetOutputsJson())
			}
			got := payload.Error
			if string(got.Kind) != tc.wantKind {
				t.Errorf("error.kind = %q; want %q", got.Kind, tc.wantKind)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("error.status = %d; want %d", got.Status, tc.wantStatus)
			}
			if got.Retryable != tc.wantRetry {
				t.Errorf("error.retryable = %v; want %v", got.Retryable, tc.wantRetry)
			}
			if !strings.Contains(got.Message, tc.wantMsgPart) {
				t.Errorf("error.message = %q; want containing %q", got.Message, tc.wantMsgPart)
			}
			if !strings.HasPrefix(got.Message, "decisionclient: ") {
				t.Errorf("error.message = %q; want the decisionclient prefix", got.Message)
			}
		})
	}
}

// TestExecuteTimeoutFailure pins the timeout row of the failure mapping: a
// call whose session timeout expires before the backend answers routes onto
// outcome "failure" with kind timeout, retryable, and no engine-visible
// error.
func TestExecuteTimeoutFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(successResponse()))
	}))
	t.Cleanup(srv.Close)
	s := newTestService()
	openDecisionSession(t, s, srv.URL, map[string]string{configKeyTimeout: "20ms"}, nil)

	sink, err := callExecute(t, s, validInput(`"ready"`), nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v (timeout is a routed failure, not an engine error)", err)
	}
	adapter, result := captureRun(t, sink, 1)
	if adapter.GetEventKind() != eventDecisionFailed {
		t.Errorf("adapter event kind = %q; want %q", adapter.GetEventKind(), eventDecisionFailed)
	}
	var payload failurePayloadOf
	if err := json.Unmarshal(result.GetOutputsJson(), &payload); err != nil {
		t.Fatalf("failure payload: %v", err)
	}
	if string(payload.Error.Kind) != "timeout" || payload.Error.Status != 0 || !payload.Error.Retryable {
		t.Errorf("failure payload = %+v; want kind timeout status 0 retryable true", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "context deadline exceeded") && !strings.Contains(payload.Error.Message, "Client.Timeout") {
		t.Errorf("error.message = %q; want the timeout signature", payload.Error.Message)
	}
}

// TestExecuteTransportFailure pins the transport row of the failure
// mapping: a connection refused below HTTP routes onto outcome "failure"
// with kind transport, retryable.
func TestExecuteTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // the port is closed: every dial is refused

	s := newTestService()
	openDecisionSession(t, s, srv.URL, nil, nil)

	sink, err := callExecute(t, s, validInput(`"ready"`), nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v (transport failures are routed, not engine errors)", err)
	}
	adapter, result := captureRun(t, sink, 1)
	if adapter.GetEventKind() != eventDecisionFailed {
		t.Errorf("adapter event kind = %q; want %q", adapter.GetEventKind(), eventDecisionFailed)
	}
	var payload failurePayloadOf
	if err := json.Unmarshal(result.GetOutputsJson(), &payload); err != nil {
		t.Fatalf("failure payload: %v", err)
	}
	if string(payload.Error.Kind) != "transport" || !payload.Error.Retryable {
		t.Errorf("failure payload = %+v; want kind transport retryable true", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "POST "+srv.URL+"/v1/systemone") {
		t.Errorf("error.message = %q; want the POST endpoint signature", payload.Error.Message)
	}
}

// TestExecuteRetryKnob pins the transport retry knob end-to-end: disabled
// by default (one request even on retryable statuses), opt-in per retries
// config, honoring 429/5xx only, and the retry-then-success path lands as
// outcome success with the knob carrying the session's config.
func TestExecuteRetryKnob(t *testing.T) {
	always429 := []stubExchange{
		{status: http.StatusTooManyRequests, body: "slow down", header: map[string]string{"Retry-After": "0"}},
	}
	tests := []struct {
		name        string
		extraConfig map[string]string
		exchanges   []stubExchange
		wantCalls   int
		wantOutcome string
	}{
		{
			name:        "default retries off: exactly one request",
			exchanges:   always429,
			wantCalls:   1,
			wantOutcome: outcomeFailure,
		},
		{
			name:        "explicit retries 0: exactly one request",
			extraConfig: map[string]string{configKeyRetries: "0"},
			exchanges:   always429,
			wantCalls:   1,
			wantOutcome: outcomeFailure,
		},
		{
			name:        "retries 2: three requests, routed failure at exhaustion",
			extraConfig: map[string]string{configKeyRetries: "2"},
			exchanges:   always429,
			wantCalls:   3,
			wantOutcome: outcomeFailure,
		},
		{
			name:        "retry-then-success: 429 retried once, success result",
			extraConfig: map[string]string{configKeyRetries: "1"},
			exchanges: []stubExchange{
				{status: http.StatusTooManyRequests, body: "slow down", header: map[string]string{"Retry-After": "0"}},
				{body: successResponse()},
			},
			wantCalls:   2,
			wantOutcome: outcomeSuccess,
		},
		{
			name:        "auth failures are never retried",
			extraConfig: map[string]string{configKeyRetries: "3"},
			exchanges:   []stubExchange{{status: http.StatusUnauthorized, body: "who are you"}},
			wantCalls:   1,
			wantOutcome: outcomeFailure,
		},
		{
			name:        "404 is not retried",
			extraConfig: map[string]string{configKeyRetries: "3"},
			exchanges:   []stubExchange{{status: http.StatusNotFound, body: "no such path"}},
			wantCalls:   1,
			wantOutcome: outcomeFailure,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := stubBackend(t, tc.exchanges...)
			if tc.extraConfig != nil {
				// Re-open the session with the knob set for this case.
				baseURL := s.sessions["s1"].baseURL
				if _, err := s.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "s1"}); err != nil {
					t.Fatalf("CloseSession: %v", err)
				}
				openDecisionSession(t, s, baseURL, tc.extraConfig, nil)
			}
			sink, err := callExecute(t, s, validInput(`"ready"`), nil, nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			_, result := captureRun(t, sink, 1)
			if result.GetOutcome() != tc.wantOutcome {
				t.Errorf("outcome = %q; want %q", result.GetOutcome(), tc.wantOutcome)
			}
			if got := atomic.LoadInt32(calls); int(got) != tc.wantCalls {
				t.Errorf("backend saw %d requests; want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestExecuteConcurrentExecutes pins concurrency safety (workstream step 6):
// parallel Executes over ONE session run isolated — every call succeeds
// against the shared session and every call that must see its own state
// does (the usage echo proves no cross-call state bleed).
func TestExecuteConcurrentExecutes(t *testing.T) {
	const parallel = 16
	var calls int32
	var mu sync.Mutex
	seen := make([]string, 0, parallel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Echo the request state back in the usage object so cross-call
		// state bleed would be observable.
		var wire struct {
			State json.RawMessage `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wire)
		mu.Lock()
		seen = append(seen, string(wire.State))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"sysone-1","answers":` + answerVerbatim + `,"usage":{"echo":` + string(wire.State) + `}}`))
	}))
	t.Cleanup(srv.Close)
	s := newTestService()
	openDecisionSession(t, s, srv.URL, nil, nil)

	var wg sync.WaitGroup
	errs := make([]error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state := fmt.Sprintf(`"call-%d"`, i)
			sink, err := callExecute(t, s, validInput(state), nil, nil)
			if err != nil {
				errs[i] = err
				return
			}
			if _, result := captureRun(t, sink, 1); result.GetOutcome() != outcomeSuccess {
				errs[i] = fmt.Errorf("outcome %q, want success", result.GetOutcome())
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("parallel call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != parallel {
		t.Errorf("backend saw %d requests; want %d", got, parallel)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != parallel {
		t.Errorf("backend saw %d distinct state payloads; want %d (state must leak to HTTP exactly once per call)", len(seen), parallel)
	}
}

// TestOpenSessionValidationMatrix pins the strict OpenSession contract:
// unknown config keys, missing base_url/model, bad timeout/retries, unknown
// secrets, and allowed_outcomes outside success|failure are all rejected.
func TestOpenSessionValidationMatrix(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]string
		secrets map[string]string
		allowed []string
		wantErr string
		valid   bool
	}{
		{name: "unknown config key", config: map[string]string{"temperature": "0.7", configKeyBaseURL: "http://x", configKeyModel: "m"}, wantErr: `unknown config key "temperature"`},
		{name: "missing base_url", config: map[string]string{configKeyModel: "m"}, wantErr: "config must carry base_url"},
		{name: "missing model", config: map[string]string{configKeyBaseURL: "http://x"}, wantErr: "config must carry model"},
		{name: "base_url requires http(s)", config: map[string]string{configKeyBaseURL: "ftp://x", configKeyModel: "m"}, wantErr: "invalid base_url"},
		{name: "bad timeout", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyTimeout: "soon"}, wantErr: "timeout must be a positive Go duration"},
		{name: "zero timeout", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyTimeout: "0s"}, wantErr: "timeout must be a positive Go duration"},
		{name: "negative timeout", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyTimeout: "-5s"}, wantErr: "timeout must be a positive Go duration"},
		{name: "bad retries", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyRetries: "many"}, wantErr: "retries must be a non-negative whole number"},
		{name: "negative retries", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyRetries: "-1"}, wantErr: "retries must be a non-negative whole number"},
		{name: "unknown secret", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m"}, secrets: map[string]string{"github_token": "ghp-x"}, wantErr: `unknown secret "github_token"`},
		{name: "bad allowed outcome", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m"}, allowed: []string{outcomeSuccess, "continue"}, wantErr: `unknown outcome "continue"`},
		{name: "duplicate allowed outcome", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m"}, allowed: []string{outcomeSuccess, outcomeSuccess}, wantErr: `duplicate outcome "success"`},
		{name: "non-bareword outcome_question", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyOutcomeQuestion: "does-run-pass"}, wantErr: "config outcome_question must be a bareword question id"},
		{name: "outcome_question naming a bareword id is valid", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyOutcomeQuestion: "route"}, valid: true},
		{name: "empty outcome_question means absent (outcome_pure)", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m", configKeyOutcomeQuestion: ""}, valid: true},
		{name: "no secrets and no allowed outcomes is valid", config: map[string]string{configKeyBaseURL: "http://x", configKeyModel: "m"}, valid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newTestService().OpenSession(context.Background(), &v2.OpenSessionRequest{
				SessionId:       "s1",
				Config:          tc.config,
				Secrets:         tc.secrets,
				AllowedOutcomes: tc.allowed,
			})
			if tc.valid {
				if err != nil {
					t.Fatalf("OpenSession err = %q; want success", err.Error())
				}
				return
			}
			if err == nil {
				t.Fatal("OpenSession err = nil; want a reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("OpenSession err = %q; want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestOpenSessionDefaults pins the default session wiring: retries 0, no
// per-call timeout, no secrets, and the full success|failure allowed set.
func TestOpenSessionDefaults(t *testing.T) {
	s, _, _ := stubBackendWithAuth(t, stubExchange{body: successResponse()})
	sess := s.sessions["s1"]
	if sess.retries != 0 || sess.timeout != 0 || len(sess.secrets) != 0 || len(sess.allowed) != 2 {
		t.Fatalf("session = retries %d timeout %v secrets %v allowed %v; want retries/timeout 0 (default OFF), no secrets, the full vocabulary",
			sess.retries, sess.timeout, sess.secrets, sess.allowed)
	}
	if _, ok := sess.allowed[outcomeSuccess]; !ok {
		t.Errorf("allowed = %v; want success present by default", sess.allowed)
	}
	if _, ok := sess.allowed[outcomeFailure]; !ok {
		t.Errorf("allowed = %v; want failure present by default", sess.allowed)
	}
}

// TestExecuteApiKeyBearer pins the secret channel: an api_key session
// secret rides every request as the Authorization bearer header.
func TestExecuteApiKeyBearer(t *testing.T) {
	s, _, auth := stubBackendWithAuth(t, stubExchange{body: successResponse()})
	// Re-open with the api key (stubBackendWithAuth opened without one).
	baseURL := s.sessions["s1"].baseURL
	if _, err := s.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	openDecisionSession(t, s, baseURL, nil, map[string]string{apiKeyName: "sk-live-42"})
	if _, err := callExecute(t, s, validInput(`"ready"`), nil, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := auth.Load(); got == nil || got.(string) != "Bearer sk-live-42" {
		t.Errorf("Authorization = %v; want Bearer sk-live-42", got)
	}
}

// TestExecuteSecretsRedactedNeverLeak pins best-effort secret hygiene:
// secret values never appear in adapter events or failure outputs, even
// when the backend echoes them back.
func TestExecuteSecretsRedactedNeverLeak(t *testing.T) {
	const keyA = "sk-live-SHORT"
	const keyB = "sk-live-MUCH-LONGER-TOKEN"

	// A backend that fails auth with a message carrying BOTH keys.
	s := newTestService()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(fmt.Sprintf("credentials %s and %s rejected", keyA, keyB)))
	}))
	t.Cleanup(srv.Close)
	openDecisionSession(t, s, srv.URL, nil, map[string]string{apiKeyName: keyA})

	// The step overlays a second secret value.
	sink, err := callExecute(t, s, validInput(`"ready"`), map[string]string{apiKeyName: keyB}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	adapter, result := captureRun(t, sink, 1)
	payloadJSON, err := json.Marshal(adapter.GetPayload().AsMap())
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	resultPayload := string(result.GetOutputsJson())
	for _, secret := range []string{keyA, keyB} {
		if strings.Contains(string(payloadJSON), secret) {
			t.Errorf("adapter event payload leaks %s: %s", secret, payloadJSON)
		}
		if strings.Contains(resultPayload, secret) {
			t.Errorf("failure outputs leak %s: %s", secret, resultPayload)
		}
	}
	if !strings.Contains(string(payloadJSON), redactedPlaceholder) {
		t.Errorf("event payload %s; want the %s placeholder", payloadJSON, redactedPlaceholder)
	}
	if !strings.Contains(resultPayload, redactedPlaceholder) {
		t.Errorf("failure outputs %s; want the %s placeholder", resultPayload, redactedPlaceholder)
	}
}

// TestExecuteStepSecretValidation pins the per-step secret contract: only
// api_key is accepted, and a step-level api_key overrides the session key
// for that call while keeping the session config.
func TestExecuteStepSecretValidation(t *testing.T) {
	s, _ := stubBackend(t, stubExchange{body: successResponse()})
	if _, err := callExecute(t, s, validInput(`"ready"`), map[string]string{"mystery": "x"}, nil); err == nil {
		t.Fatal("Execute with an unknown secret input err = nil; want a reject")
	} else if !strings.Contains(err.Error(), `unknown secret input "mystery"`) {
		t.Fatalf("err = %q; want unknown-secret-input reject", err.Error())
	}

	// Step overlay: the request carries its own api key.
	s2, _, auth := stubBackendWithAuth(t, stubExchange{body: successResponse()})
	if _, err := callExecute(t, s2, validInput(`"ready"`), map[string]string{apiKeyName: "sk-step"}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := auth.Load(); got == nil || got.(string) != "Bearer sk-step" {
		t.Errorf("Authorization = %v; want the step overlay Bearer sk-step", got)
	}
}

// TestExecuteAllowedOutcomesValidation pins the step-level outcome
// contract: allowed_outcomes may only narrow to success|failure, and a bad
// list fails the step before any backend traffic.
func TestExecuteAllowedOutcomesValidation(t *testing.T) {
	s, calls := stubBackend(t, stubExchange{body: successResponse()})
	if _, err := callExecute(t, s, validInput(`"ready"`), nil, []string{outcomeSuccess, "skip"}); err == nil {
		t.Fatal("Execute with a bogus outcome err = nil; want a reject")
	} else if !strings.Contains(err.Error(), `unknown outcome "skip"`) {
		t.Fatalf("err = %q; want unknown-outcome reject", err.Error())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("backend saw %d requests; want 0 (outcome validation precedes HTTP)", got)
	}

	// The empty list (host default) and the full vocabulary are both fine.
	for _, allowed := range [][]string{nil, {outcomeSuccess}, {outcomeSuccess, outcomeFailure}} {
		if _, err := callExecute(t, s, validInput(`"ready"`), nil, allowed); err != nil {
			t.Errorf("Execute with allowed %v: %v", allowed, err)
		}
	}
}

// TestExecuteUnknownSession pins that Execute without an open session fails
// loud with an engine-visible error (session bookkeeping is not a routed
// decision failure).
func TestExecuteUnknownSession(t *testing.T) {
	s := newTestService()
	err := s.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "missing"}, &captureSink{})
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if !strings.Contains(err.Error(), `unknown session "missing"`) {
		t.Errorf("unknown-session error = %q", err.Error())
	}
}

// TestCloseSessionRemovesSession pins CloseSession bookkeeping.
func TestCloseSessionRemovesSession(t *testing.T) {
	s, _ := stubBackend(t, stubExchange{body: successResponse()})
	ctx := context.Background()
	if _, err := s.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := s.Execute(ctx, &v2.ExecuteRequest{SessionId: "s1"}, &captureSink{}); err == nil {
		t.Fatal("Execute after CloseSession err = nil; want unknown-session error")
	}
}

// TestLogReturnsPromptly pins the SDK-owned log-stream contract: the
// adapter's Log returns immediately and never emits log events; the SDK
// keeps the stream and its heartbeat alive for the full session lifetime.
func TestLogReturnsPromptly(t *testing.T) {
	sender := &captureLogSender{}
	if err := newTestService().Log(
		context.Background(), &v2.LogRequest{SessionId: "s1"}, sender,
	); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(sender.events) != 0 {
		t.Errorf("Log emitted %d events, want 0 (the SDK owns heartbeats)", len(sender.events))
	}
}

// --- outcome_question mapping (ADR-0013 D3, Kanboard 203) --------------------

// mappingQuestionsJSON is a valid choice+noul question pair for the
// outcome_question mapping tests: "route" is the choice question the mapping
// reads, and its options deliberately include "abort" — a name that is NOT an
// adapter outcome — so an answer that decodes cleanly can still be
// out-of-set for the mapping.
const mappingQuestionsJSON = `[{"id":"route","type":"choice","instructions":"Route the run.","criteria":{"success":"take the success branch","failure":"take the failure branch","abort":"abandon the run"}},{"id":"verdict","type":"noul","instructions":"Is the run safe?"}]`

// mappingAnswer returns the verbatim answers array for mappingQuestionsJSON
// with the given choice selected for "route".
func mappingAnswer(choice string) string {
	return fmt.Sprintf(`[{"id":"route","choice":%q,"probabilities":{"abort":0.1,"failure":0.2,"success":0.7},"confidence":0.9},{"id":"verdict","noul":"yes"}]`, choice)
}

// mappingResponse returns the full success response body with the given
// choice selected for the mapping question.
func mappingResponse(choice string) string {
	return `{"model":"sysone-1","answers":` + mappingAnswer(choice) + `,"usage":` + usageVerbatim + `}`
}

// mappingFailurePayloadOf is the typed payload of the mapping's routed
// failure; failurePayloadOf cannot carry the extra "allowed" member.
type mappingFailurePayloadOf struct {
	Error struct {
		Kind      string   `json:"kind"`
		Status    int      `json:"status"`
		Message   string   `json:"message"`
		Retryable bool     `json:"retryable"`
		Allowed   []string `json:"allowed"`
	} `json:"error"`
}

// TestExecuteOutcomeMappingHappyPath pins the strict set-member contract:
// on a mapped step the selected choice maps VERBATIM onto the step outcome
// (both members), the outputs stay byte-verbatim answers+usage on both
// mapped outcomes (a failure-mapped outcome invents no error payload), and
// the call still emits decision.completed — the mapping changes the outcome
// name, not the event posture.
func TestExecuteOutcomeMappingHappyPath(t *testing.T) {
	tests := []struct {
		name        string
		choice      string
		allowed     []string
		wantOutcome string
	}{
		{name: "choice success maps onto outcome success", choice: "success", allowed: []string{outcomeSuccess, outcomeFailure}, wantOutcome: outcomeSuccess},
		{name: "choice failure maps onto outcome failure", choice: "failure", allowed: []string{outcomeSuccess, outcomeFailure}, wantOutcome: outcomeFailure},
		{name: "empty step allowed_outcomes means the full vocabulary", choice: "failure", allowed: nil, wantOutcome: outcomeFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := stubBackendWithConfig(t, map[string]string{configKeyOutcomeQuestion: "route"}, stubExchange{body: mappingResponse(tc.choice)})
			sink, err := callExecute(t, s, validInputWithQuestions(mappingQuestionsJSON, `"ready"`), nil, tc.allowed)
			if err != nil {
				t.Fatalf("Execute: %v (in-set mapping is a routed success/failure, never an engine error)", err)
			}
			adapter, result := captureRun(t, sink, 1)
			if result.GetOutcome() != tc.wantOutcome {
				t.Errorf("outcome = %q; want the verbatim choice %q", result.GetOutcome(), tc.wantOutcome)
			}
			wantOutputs := `{"answers":` + mappingAnswer(tc.choice) + `,"usage":` + usageVerbatim + `}`
			if got := string(result.GetOutputsJson()); got != wantOutputs {
				t.Errorf("outputs =\n%s\nwant byte-verbatim (answers+usage, unchanged by the mapping)\n%s", got, wantOutputs)
			}
			if adapter.GetEventKind() != eventDecisionCompleted {
				t.Errorf("adapter event kind = %q; want %q even for a failure-mapped outcome", adapter.GetEventKind(), eventDecisionCompleted)
			}
			if got := atomic.LoadInt32(calls); got != 1 {
				t.Errorf("backend saw %d requests; want 1", got)
			}
		})
	}
}

// TestExecuteOutcomeMappingOutOfSet pins the fail-closed membership rule:
// a selected choice that is not a member of the step's allowed_outcomes set
// routes onto outcome "failure" with the typed payload
// {error: {kind: "outcome_out_of_set", allowed: [...], message,
// status, retryable}} — never an engine-visible error, never the choice
// passing through as an outcome.
func TestExecuteOutcomeMappingOutOfSet(t *testing.T) {
	tests := []struct {
		name        string
		choice      string
		allowed     []string
		wantAllowed []string
	}{
		{name: "a valid question option that names no outcome", choice: "abort", allowed: []string{outcomeSuccess, outcomeFailure}, wantAllowed: []string{outcomeSuccess, outcomeFailure}},
		{name: "an in-vocabulary choice outside the step narrowing", choice: "success", allowed: []string{outcomeFailure}, wantAllowed: []string{outcomeFailure}},
		{name: "empty step narrowing expands to the full vocabulary", choice: "abort", allowed: nil, wantAllowed: []string{outcomeSuccess, outcomeFailure}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := stubBackendWithConfig(t, map[string]string{configKeyOutcomeQuestion: "route"}, stubExchange{body: mappingResponse(tc.choice)})
			sink, err := callExecute(t, s, validInputWithQuestions(mappingQuestionsJSON, `"ready"`), nil, tc.allowed)
			if err != nil {
				t.Fatalf("Execute: %v (out-of-set is a routed failure, not an engine error)", err)
			}
			adapter, result := captureRun(t, sink, 1)
			if result.GetOutcome() != outcomeFailure {
				t.Errorf("outcome = %q; want %q (fail closed)", result.GetOutcome(), outcomeFailure)
			}
			var payload mappingFailurePayloadOf
			if err := json.Unmarshal(result.GetOutputsJson(), &payload); err != nil {
				t.Fatalf("failure payload: %v", err)
			}
			if payload.Error.Kind != outcomeOutOfSetKind {
				t.Errorf("error.kind = %q; want %q", payload.Error.Kind, outcomeOutOfSetKind)
			}
			if payload.Error.Status != 0 || payload.Error.Retryable {
				t.Errorf("error.status = %d, error.retryable = %v; want 0/false (a mapping contract breach is deterministic)", payload.Error.Status, payload.Error.Retryable)
			}
			if !slices.Equal(payload.Error.Allowed, tc.wantAllowed) {
				t.Errorf("error.allowed = %v; want %v", payload.Error.Allowed, tc.wantAllowed)
			}
			if !strings.Contains(payload.Error.Message, `"route"`) || !strings.Contains(payload.Error.Message, tc.choice) {
				t.Errorf("error.message = %q; want it to name the question and the rejected choice", payload.Error.Message)
			}
			if adapter.GetEventKind() != eventDecisionFailed {
				t.Errorf("adapter event kind = %q; want %q", adapter.GetEventKind(), eventDecisionFailed)
			}
			eventError, ok := adapter.GetPayload().AsMap()["error"].(map[string]any)
			if !ok {
				t.Fatalf("event payload = %#v; want an error object", adapter.GetPayload().AsMap())
			}
			eventAllowed := []string{}
			for _, name := range eventError["allowed"].([]any) {
				eventAllowed = append(eventAllowed, name.(string))
			}
			if !slices.Equal(eventAllowed, tc.wantAllowed) {
				t.Errorf("event error.allowed = %v; want %v (event and result carry the same typed payload)", eventAllowed, tc.wantAllowed)
			}
		})
	}
}

// TestExecuteOutcomeMappingConfigErrors pins the phase-1 config checks at
// Execute (questions are per-step inputs, never on the session's contract):
// a step carrying the configured id with a type that cannot produce a
// choice fails closed — an engine-visible config error, BEFORE any HTTP
// traffic and without emitting any event.
func TestExecuteOutcomeMappingConfigErrors(t *testing.T) {
	tests := []struct {
		name      string
		questions string
		wantErr   string
	}{
		{name: "mapped id carried as noul", questions: `[{"id":"route","type":"noul","instructions":"Route the run?"}]`, wantErr: `config outcome_question names question "route" with type "noul"; the strict Choice-to-outcome mapping requires type "choice"`},
		{name: "mapped id carried as score", questions: `[{"id":"route","type":"score","instructions":"Route the run.","criteria":["low","high"]}]`, wantErr: `config outcome_question names question "route" with type "score"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := stubBackendWithConfig(t, map[string]string{configKeyOutcomeQuestion: "route"}, stubExchange{body: mappingResponse("success")})
			sink, err := callExecute(t, s, validInputWithQuestions(tc.questions, `"ready"`), nil, nil)
			if err == nil {
				t.Fatalf("Execute(...) err = nil; want the config error %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute(...) err = %q; want containing %q", err.Error(), tc.wantErr)
			}
			if got := len(sink.events); got != 0 {
				t.Fatalf("Execute emitted %d events; want 0 (config errors are not routed)", got)
			}
			if got := atomic.LoadInt32(calls); got != 0 {
				t.Fatalf("backend saw %d requests; want 0 (fail closed before any HTTP traffic)", got)
			}
		})
	}
}

// TestExecuteOutcomeMappingMixedUsage pins mixed usage (workstream step 4):
// ONE opened session whose config names the outcome question, serving steps
// with and without it. A step whose questions lack the configured id is an
// outcome_pure step — ALWAYS outcome success regardless of the step's
// allowed_outcomes — while a step carrying the id maps strictly. This is
// also where the default-absent posture (no outcome_question configured)
// pins outcome_pure success despite a narrowed step set.
func TestExecuteOutcomeMappingMixedUsage(t *testing.T) {
	s, calls := stubBackendWithConfig(t,
		map[string]string{configKeyOutcomeQuestion: "route"},
		stubExchange{body: successResponse()},          // step without the question
		stubExchange{body: mappingResponse("success")}, // mapped step
	)

	// Step 1: the outcome question is absent from the step's questions —
	// not a config error; the step stays outcome_pure and always succeeds,
	// even with a narrowing that does not declare success.
	sink, err := callExecute(t, s, validInput(`"ready"`), nil, []string{outcomeFailure})
	if err != nil {
		t.Fatalf("Execute (step without the question): %v", err)
	}
	adapter, result := captureRun(t, sink, 1)
	if result.GetOutcome() != outcomeSuccess {
		t.Errorf("outcome_pure step outcome = %q; want %q regardless of allowed_outcomes", result.GetOutcome(), outcomeSuccess)
	}
	if adapter.GetEventKind() != eventDecisionCompleted {
		t.Errorf("outcome_pure step event kind = %q; want %q", adapter.GetEventKind(), eventDecisionCompleted)
	}

	// Step 2: the outcome question rides the step's questions and maps
	// strictly.
	sink, err = callExecute(t, s, validInputWithQuestions(mappingQuestionsJSON, `"ready"`), nil, []string{outcomeSuccess, outcomeFailure})
	if err != nil {
		t.Fatalf("Execute (mapped step): %v", err)
	}
	_, result = captureRun(t, sink, 1)
	if result.GetOutcome() != outcomeSuccess {
		t.Errorf("mapped step outcome = %q; want the verbatim choice %q", result.GetOutcome(), outcomeSuccess)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("backend saw %d requests; want 2 (one per step)", got)
	}
}

// TestExecuteOutcomePureIgnoresAllowedOutcomes pins the default posture over
// a narrowed step set: with no outcome_question configured, the outcome is
// ALWAYS success regardless of allowed_outcomes (routing is graph-side).
func TestExecuteOutcomePureIgnoresAllowedOutcomes(t *testing.T) {
	s, _ := stubBackend(t, stubExchange{body: successResponse()})
	sink, err := callExecute(t, s, validInput(`"ready"`), nil, []string{outcomeFailure})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, result := captureRun(t, sink, 1)
	if result.GetOutcome() != outcomeSuccess {
		t.Errorf("outcome = %q; want %q regardless of allowed_outcomes", result.GetOutcome(), outcomeSuccess)
	}
}

// TestExecuteOutcomeMappingKeepsDecisionFailures pins that the mapping never
// hijacks a failing backend call: on a mapped step a 5xx decision still
// routes the D6 http-kind failure payload, not the out-of-set one.
func TestExecuteOutcomeMappingKeepsDecisionFailures(t *testing.T) {
	s, _ := stubBackendWithConfig(t,
		map[string]string{configKeyOutcomeQuestion: "route"},
		stubExchange{status: http.StatusInternalServerError, body: "boom"},
	)
	sink, err := callExecute(t, s, validInputWithQuestions(mappingQuestionsJSON, `"ready"`), nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, result := captureRun(t, sink, 1)
	if result.GetOutcome() != outcomeFailure {
		t.Errorf("outcome = %q; want %q", result.GetOutcome(), outcomeFailure)
	}
	var payload failurePayloadOf
	if err := json.Unmarshal(result.GetOutputsJson(), &payload); err != nil {
		t.Fatalf("failure payload: %v", err)
	}
	if payload.Error.Kind != decisionclient.DecisionErrorKindHTTP || payload.Error.Status != http.StatusInternalServerError {
		t.Errorf("failure payload kind = %q status = %d; want http/500 (D6 routing is untouched by the mapping)", payload.Error.Kind, payload.Error.Status)
	}
}
