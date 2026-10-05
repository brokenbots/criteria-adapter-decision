package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	openDecisionSession(t, s, srv.URL, nil, nil)
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
	return map[string]string{"questions": executeValidQuestionsJSON, "state": state}
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
