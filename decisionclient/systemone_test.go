package decisionclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// decisionStub is a httptest System One stub: it records the incoming request
// and replies with the given body.
type decisionStub struct {
	server   *httptest.Server
	mu       sync.Mutex
	recorder *recordedRequest
}

// recordedRequest captures the exact wire shape seen by the stub.
type recordedRequest struct {
	Request *http.Request
	Body    []byte
}

func newDecisionStub(t *testing.T, responseBody string) *decisionStub {
	t.Helper()
	stub := &decisionStub{recorder: &recordedRequest{}}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("stub read body: %v", err)
			http.Error(w, "stub read failure", http.StatusInternalServerError)
			return
		}
		stub.mu.Lock()
		stub.recorder.Request = r
		stub.recorder.Body = b
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func newTestClient(t *testing.T, serverURL, model string, apiKey ...string) *SystemOneClient {
	t.Helper()
	c, err := New(serverURL, model, apiKey...)
	if err != nil {
		t.Fatalf("New(%q, %q, %v) err = %v; want nil", serverURL, model, apiKey, err)
	}
	return c
}

// TestDecisionValidatesBeforeHTTP proves the reject list fails the call
// without any HTTP traffic: the stub recorder stays empty for every
// invalid request.
func TestDecisionValidatesBeforeHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		state   State
		input   []Question
		wantErr string
	}{
		{name: "bad question type", state: "ready", input: []Question{{ID: "q1", Type: QuestionType("emoji"), Instructions: "Decide."}}, wantErr: "type must be"},
		{name: "empty instructions", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: ""}}, wantErr: "instructions is required"},
		{name: "choice without criteria", state: "ready", input: []Question{{ID: "q1", Type: TypeChoice, Instructions: "Pick."}}, wantErr: "choice question"},
		{name: "score criteria wrong shape", state: "ready", input: []Question{{ID: "q1", Type: TypeScore, Instructions: "Grade.", Levels: []string{}}}, wantErr: "score question"},
		{name: "non-bareword id", state: "ready", input: []Question{{ID: "does-run-pass", Type: TypeNoul, Instructions: "Decide."}}, wantErr: "bareword"},
		{name: "cross-type criteria", state: "ready", input: []Question{{ID: "q1", Type: TypeChoice, Instructions: "Pick.", Levels: []string{"low", "high"}}}, wantErr: "must not carry score criteria"},
		{name: "duplicate question ids", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: "One."}, {ID: "q1", Type: TypeNoul, Instructions: "Two."}}, wantErr: "duplicate question id"},
		{name: "no questions", state: "ready", input: nil, wantErr: "at least one question"},
		{name: "nil state", state: nil, input: testQuestions(), wantErr: "state must be a JSON string, object, or array"},
		{name: "number state", state: json.Number("3"), input: testQuestions(), wantErr: "state must be a JSON string, object, or array"},
		{name: "boolean state", state: true, input: testQuestions(), wantErr: "state must be a JSON string, object, or array"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newDecisionStub(t, "{}")
			client := newTestClient(t, stub.server.URL, "clef")

			_, err := client.Decision(context.Background(), tc.state, tc.input)
			if err == nil {
				t.Fatalf("Decision() err = nil; want containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Decision() err = %q; want containing %q", err.Error(), tc.wantErr)
			}
			if stub.recorder.Request != nil {
				t.Fatalf("Decision() hit the backend for an invalid request; want validation before any HTTP traffic")
			}
		})
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		model   string
		apiKey  []string
		wantErr string
	}{
		{name: "empty base_url", baseURL: "", model: "clef", wantErr: "base_url is required"},
		{name: "empty model", baseURL: "http://s1.local", model: "", wantErr: "model is required"},
		{name: "missing scheme", baseURL: "s1.local/api", model: "clef", wantErr: "want an http or https URL"},
		{name: "missing host", baseURL: "http:///v1", model: "clef", wantErr: "missing host"},
		{name: "embedded userinfo", baseURL: "http://user:secret@s1.local", model: "clef", wantErr: "must not embed credentials"},
		{name: "multiple api keys", baseURL: "http://s1.local", model: "clef", apiKey: []string{"a", "b"}, wantErr: "single optional api key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.baseURL, tc.model, tc.apiKey...)
			if err == nil {
				t.Fatalf("New(%q, %q, %v) err = nil; want %q", tc.baseURL, tc.model, tc.apiKey, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New(%q, %q, %v) err = %q; want containing %q", tc.baseURL, tc.model, tc.apiKey, err.Error(), tc.wantErr)
			}
		})
	}
}

// testQuestions returns a minimal valid question set for stub-based tests.
func testQuestions() []Question {
	return []Question{
		{ID: "verdict", Type: TypeNoul, Instructions: "Say whether the run is acceptable."},
	}
}

func TestDecisionWireShape(t *testing.T) {
	t.Parallel()

	stub := newDecisionStub(t, "{}")
	client := newTestClient(t, stub.server.URL, "m1")
	questions := []Question{
		{ID: "does_run_pass", Type: TypeChoice, Instructions: "Pick the run outcome.", Options: map[string]string{"pass": "The run passed.", "fail": "The run failed."}},
		{ID: "quality", Type: TypeScore, Instructions: "Grade the run quality.", Levels: []string{"low", "high"}},
		{ID: "safe", Type: TypeNoul, Instructions: "Is the run safe?"},
	}

	raw, err := client.Decision(context.Background(), "ready", questions)
	if err != nil {
		t.Fatalf("Decision() err = %v; want nil", err)
	}
	if len(raw) != 2 || string(raw) != "{}" {
		t.Fatalf("Decision() raw = %q; want %q", raw, "{}")
	}

	rec := stub.recorder
	if rec.Request == nil {
		t.Fatal("stub saw no request")
	}
	if rec.Request.Method != http.MethodPost {
		t.Fatalf("request method = %q; want POST", rec.Request.Method)
	}
	if rec.Request.URL.Path != "/v1/systemone" {
		t.Fatalf("request path = %q; want /v1/systemone", rec.Request.URL.Path)
	}
	if got := rec.Request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", got)
	}
	if got := rec.Request.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q; want application/json", got)
	}
	wantBody := `{"model":"m1","state":"ready","questions":{` +
		`"does_run_pass":{"type":"choice","instructions":"Pick the run outcome.","criteria":{"fail":"The run failed.","pass":"The run passed."}},` +
		`"quality":{"type":"score","instructions":"Grade the run quality.","criteria":["low","high"]},` +
		`"safe":{"type":"noul","instructions":"Is the run safe?"}}}`
	if string(rec.Body) != wantBody {
		t.Fatalf("request body = %s; want %s", rec.Body, wantBody)
	}
}

func TestDecisionRequestPathFromBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{name: "bare host", basePath: "", wantPath: "/v1/systemone"},
		{name: "base path prefix", basePath: "/api", wantPath: "/api/v1/systemone"},
		{name: "base path with trailing slash", basePath: "/api/", wantPath: "/api/v1/systemone"},
		{name: "nested base path", basePath: "/gateway/s1", wantPath: "/gateway/s1/v1/systemone"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newDecisionStub(t, "{}")
			client := newTestClient(t, stub.server.URL+tc.basePath, "clef")
			if _, err := client.Decision(context.Background(), "ready", testQuestions()); err != nil {
				t.Fatalf("Decision() err = %v; want nil", err)
			}
			if got := stub.recorder.Request.URL.Path; got != tc.wantPath {
				t.Fatalf("request path = %q; want %q", got, tc.wantPath)
			}
		})
	}
}

func TestDecisionAuthHeaderOnlyWithKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apiKey     []string
		wantHeader string
	}{
		{name: "no key argument", apiKey: nil, wantHeader: ""},
		{name: "empty key argument", apiKey: []string{""}, wantHeader: ""},
		{name: "key set", apiKey: []string{"s3cr3t"}, wantHeader: "Bearer s3cr3t"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newDecisionStub(t, "{}")
			client := newTestClient(t, stub.server.URL, "clef", tc.apiKey...)
			if _, err := client.Decision(context.Background(), "ready", testQuestions()); err != nil {
				t.Fatalf("Decision() err = %v; want nil", err)
			}
			got := stub.recorder.Request.Header.Get("Authorization")
			if got != tc.wantHeader {
				t.Fatalf("Authorization = %q; want %q", got, tc.wantHeader)
			}
		})
	}
}

func TestDecisionPassthroughByteIdentical(t *testing.T) {
	t.Parallel()

	// Deliberately asymmetric whitespace and key order: the client must not
	// decode and re-encode the response.
	backendBody := "{\n  \"state\": \"ready\",\n\t\"scores\" : [ 1 , null, true ],\n \"outcome\":\"go\"\n}"
	stub := newDecisionStub(t, backendBody)
	client := newTestClient(t, stub.server.URL, "clef")

	raw, err := client.Decision(context.Background(), "ready", testQuestions())
	if err != nil {
		t.Fatalf("Decision() err = %v; want nil", err)
	}
	if string(raw) != backendBody {
		t.Fatalf("Decision() raw = %q; want byte-identical passthrough %q", raw, backendBody)
	}
}

func TestDecisionErrorsOnServerStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		wantSubs []string
	}{
		{name: "internal error", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantSubs: []string{"500", `{"error":"boom"}`}},
		{name: "not found", status: http.StatusNotFound, body: "nope", wantSubs: []string{"404", "nope"}},
		{name: "empty body", status: http.StatusBadGateway, body: "", wantSubs: []string{"502", "(empty body)"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server.URL, "clef")
			_, err := client.Decision(context.Background(), "ready", testQuestions())
			if err == nil {
				t.Fatalf("Decision() err = nil; want status %d error", tc.status)
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Decision() err = %q; want containing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestDecisionRejectsInvalidJSONResponse(t *testing.T) {
	t.Parallel()

	stub := newDecisionStub(t, `{"state": unquoted`)
	client := newTestClient(t, stub.server.URL, "clef")
	_, err := client.Decision(context.Background(), "ready", testQuestions())
	if err == nil {
		t.Fatal("Decision() err = nil; want invalid-JSON error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("Decision() err = %q; want containing %q", err.Error(), "not valid JSON")
	}
}

func TestDecisionStateWireVariants(t *testing.T) {
	t.Parallel()

	stateQuestions := []Question{{ID: "q1", Type: TypeNoul, Instructions: "Decide."}}
	tests := []struct {
		name     string
		input    State
		wantBody string
	}{
		{name: "string state", input: "ready", wantBody: `{"model":"clef","state":"ready","questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
		{name: "object state", input: map[string]any{"branch": "main", "reps": json.Number("3")}, wantBody: `{"model":"clef","state":{"branch":"main","reps":3},"questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
		{name: "array state", input: []any{"a", json.Number("0.5")}, wantBody: `{"model":"clef","state":["a",0.5],"questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
		{name: "empty object state", input: map[string]any{}, wantBody: `{"model":"clef","state":{},"questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
		{name: "empty array state", input: []any{}, wantBody: `{"model":"clef","state":[],"questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
		{name: "empty string state", input: "", wantBody: `{"model":"clef","state":"","questions":{"q1":{"type":"noul","instructions":"Decide."}}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newDecisionStub(t, "{}")
			client := newTestClient(t, stub.server.URL, "clef")
			if _, err := client.Decision(context.Background(), tc.input, stateQuestions); err != nil {
				t.Fatalf("Decision() err = %v; want nil", err)
			}
			if string(stub.recorder.Body) != tc.wantBody {
				t.Fatalf("request body = %s; want %s", stub.recorder.Body, tc.wantBody)
			}
		})
	}
}

// TestDecisionQuestionMapSortedKeys pins the questions map to canonical
// wire bytes: id-less entries keyed by question id, keys sorted so the same
// call produces identical bytes every time.
func TestDecisionQuestionMapSortedKeys(t *testing.T) {
	t.Parallel()

	stub := newDecisionStub(t, "{}")
	client := newTestClient(t, stub.server.URL, "clef")
	questions := []Question{
		{ID: "b_second", Type: TypeNoul, Instructions: "Second."},
		{ID: "a_first", Type: TypeNoul, Instructions: "First."},
	}
	if _, err := client.Decision(context.Background(), "ready", questions); err != nil {
		t.Fatalf("Decision() err = %v; want nil", err)
	}
	wantBody := `{"model":"clef","state":"ready","questions":{` +
		`"a_first":{"type":"noul","instructions":"First."},` +
		`"b_second":{"type":"noul","instructions":"Second."}}}`
	if string(stub.recorder.Body) != wantBody {
		t.Fatalf("request body = %s; want %s", stub.recorder.Body, wantBody)
	}
}

func TestDecisionCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stub := newDecisionStub(t, "{}")
	client := newTestClient(t, stub.server.URL, "clef")
	_, err := client.Decision(ctx, "ready", testQuestions())
	if err == nil {
		t.Fatal("Decision() err = nil with canceled context; want context error")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("Decision() err = %q; want containing %q", err.Error(), context.Canceled.Error())
	}
}

func TestDecisionNilContext(t *testing.T) {
	t.Parallel()

	stub := newDecisionStub(t, "{}")
	client := newTestClient(t, stub.server.URL, "clef")
	if _, err := client.Decision(nil, "ready", testQuestions()); err == nil {
		t.Fatal("Decision(nil ctx) err = nil; want error")
	}
}

func TestDecisionRejectsOversizedResponse(t *testing.T) {
	t.Parallel()

	t.Run("within cap is accepted", func(t *testing.T) {
		t.Parallel()
		body := "{\"x\":\"" + strings.Repeat("a", maxResponseBytes-8) + "\"}" // exactly maxResponseBytes
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, server.URL, "clef")
		raw, err := client.Decision(context.Background(), "ready", testQuestions())
		if err != nil {
			t.Fatalf("Decision() err = %v; want nil", err)
		}
		if len(raw) != maxResponseBytes {
			t.Fatalf("len(raw) = %d; want %d", len(raw), maxResponseBytes)
		}
	})

	t.Run("beyond cap is rejected", func(t *testing.T) {
		t.Parallel()
		oversized := "{" + strings.Repeat("a", maxResponseBytes+1) + "}" // cap + 3 bytes
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, oversized)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, server.URL, "clef")
		_, err := client.Decision(context.Background(), "ready", testQuestions())
		if err == nil {
			t.Fatal("Decision() err = nil; want oversized-response error")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("Decision() err = %q; want containing %q", err.Error(), "exceeds")
		}
	})
}

func TestDecisionSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	const workers = 8
	stub := newDecisionStub(t, `{"verdict":"ok"}`)
	client := newTestClient(t, stub.server.URL, "clef")

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			raw, err := client.Decision(context.Background(), "ready", testQuestions())
			if err != nil {
				errs <- err
				return
			}
			if string(raw) != `{"verdict":"ok"}` {
				errs <- fmt.Errorf("worker %d: unexpected response %s", n, raw)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
