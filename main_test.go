package main

import (
	"context"
	"strings"
	"testing"

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

func TestInfo(t *testing.T) {
	resp, err := (&decisionService{sessions: map[string]struct{}{}}).Info(context.Background(), &v2.InfoRequest{})
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
}

// executeValidInput is a minimal valid Input for Execute: one noul question
// and a string state.
const (
	executeValidQuestionsJSON = `[{"id":"verdict","type":"noul","instructions":"Is the run safe?"}]`
	executeValidStateJSON     = `"ready"`
)

func executeValidInput() map[string]string {
	return map[string]string{"questions": executeValidQuestionsJSON, "state": executeValidStateJSON}
}

// TestExecuteFailClosed pins the scaffold contract: Execute with strictly
// valid input errors out and emits no result events, so a wired-up pipeline
// fails the step loudly rather than reporting a guessed outcome.
func TestExecuteFailClosed(t *testing.T) {
	s := &decisionService{sessions: map[string]struct{}{}}
	ctx := context.Background()
	const sid = "s1"
	if _, err := s.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	sink := &captureSink{}
	err := s.Execute(ctx, &v2.ExecuteRequest{SessionId: sid, Input: executeValidInput()}, sink)
	if err == nil {
		t.Fatal("Execute err = nil; want the fail-closed not-implemented error")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Execute err = %q; want containing %q", err.Error(), "not implemented")
	}
	if len(sink.events) != 0 {
		t.Errorf("Execute emitted %d events; want 0 (fail-closed, no guessed result)", len(sink.events))
	}
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
		{name: "state only", input: map[string]string{"state": executeValidStateJSON}, wantErr: `input must carry a "questions" key`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newExecutedSession(t)
			sink := &captureSink{}
			err := s.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "s1", Input: tc.input}, sink)
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

// newExecutedSession opens a session on a fresh service so Execute passes
// the session check.
func newExecutedSession(t *testing.T) *decisionService {
	t.Helper()
	s := &decisionService{sessions: map[string]struct{}{}}
	if _, err := s.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return s
}

// TestExecuteInputRejectMatrix is the Execute-level reject matrix: 100% of
// the question and state reject cases fail the step (before any decision
// work), never default or pass through.
func TestExecuteInputRejectMatrix(t *testing.T) {
	tests := []struct {
		name                     string
		questions                string
		state                    string
		wantErr                  string
		wantDecisionclientPrefix bool
	}{
		// questions rejects.
		{name: "questions not an array", questions: `{"id":"q1"}`, state: executeValidStateJSON, wantErr: "questions must be a JSON array of question objects", wantDecisionclientPrefix: true},
		{name: "questions empty array", questions: `[]`, state: executeValidStateJSON, wantErr: "at least one question", wantDecisionclientPrefix: true},
		{name: "questions empty string", questions: ``, state: executeValidStateJSON, wantErr: "questions must be a JSON array", wantDecisionclientPrefix: true},
		{name: "bad question type", questions: `[{"id":"q1","type":"emoji","instructions":"g"}]`, state: executeValidStateJSON, wantErr: `type must be "choice", "score", "noul"`, wantDecisionclientPrefix: true},
		{name: "empty instructions", questions: `[{"id":"q1","type":"noul","instructions":""}]`, state: executeValidStateJSON, wantErr: "instructions is required", wantDecisionclientPrefix: true},
		{name: "choice without criteria", questions: `[{"id":"q1","type":"choice","instructions":"g"}]`, state: executeValidStateJSON, wantErr: "choice questions require criteria", wantDecisionclientPrefix: true},
		{name: "score criteria as map (wrong shape)", questions: `[{"id":"q1","type":"score","instructions":"g","criteria":{"low":1}}]`, state: executeValidStateJSON, wantErr: "not an object", wantDecisionclientPrefix: true},
		{name: "unknown key in question object", questions: `[{"id":"q1","type":"noul","instructions":"g","priority":1}]`, state: executeValidStateJSON, wantErr: `unknown field "priority"`, wantDecisionclientPrefix: true},
		{name: "non-bareword id", questions: `[{"id":"does-run-pass","type":"noul","instructions":"g"}]`, state: executeValidStateJSON, wantErr: "id must be a bareword", wantDecisionclientPrefix: true},
		{name: "duplicate question ids", questions: `[{"id":"q1","type":"noul","instructions":"a"},{"id":"q1","type":"noul","instructions":"b"}]`, state: executeValidStateJSON, wantErr: `duplicate question id "q1"`, wantDecisionclientPrefix: true},
		{name: "questions trailing data", questions: `[] {"x":1}`, state: executeValidStateJSON, wantErr: "questions must be a JSON array", wantDecisionclientPrefix: true},

		// state rejects.
		{name: "state is null", questions: executeValidQuestionsJSON, state: `null`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state is a number", questions: executeValidQuestionsJSON, state: `3`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state is a boolean", questions: executeValidQuestionsJSON, state: `true`, wantErr: "state must be a JSON string, object, or array", wantDecisionclientPrefix: true},
		{name: "state malformed", questions: executeValidQuestionsJSON, state: `{"a":`, wantErr: "invalid state JSON", wantDecisionclientPrefix: true},
		{name: "state trailing data", questions: executeValidQuestionsJSON, state: `"ready" ""`, wantErr: "trailing data", wantDecisionclientPrefix: true},
		{name: "state empty", questions: executeValidQuestionsJSON, state: ``, wantErr: "state is required", wantDecisionclientPrefix: true},

		// Valid inputs still fail closed at the not-implemented boundary;
		// included to prove the matrix orders validation before it.
		{name: "object state and full question set pass validation", questions: `[{"id":"pick","type":"choice","instructions":"g","criteria":{"yes":"y"}},{"id":"grade","type":"score","instructions":"g","criteria":["low"]}]`, state: `{"branch":"main"}`, wantErr: "not implemented"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newExecutedSession(t)
			sink := &captureSink{}
			err := s.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "s1", Input: map[string]string{"questions": tc.questions, "state": tc.state}}, sink)
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
		})
	}
}

func TestExecuteUnknownSession(t *testing.T) {
	s := &decisionService{sessions: map[string]struct{}{}}
	err := s.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "missing"}, &captureSink{})
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if strings.Contains(err.Error(), "not implemented") {
		t.Errorf("unknown-session error = %q; the session check must precede the stub error", err.Error())
	}
}

func TestCloseSessionRemovesSession(t *testing.T) {
	s := &decisionService{sessions: map[string]struct{}{}}
	ctx := context.Background()
	const sid = "s1"
	if _, err := s.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, err := s.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := s.Execute(ctx, &v2.ExecuteRequest{SessionId: sid}, &captureSink{}); err == nil {
		t.Fatal("Execute after CloseSession err = nil; want unknown-session error")
	}
}

// TestLogReturnsPromptly pins the SDK-owned log-stream contract: the adapter's
// Log returns immediately and never emits log events; the SDK keeps the
// stream and its heartbeat alive for the full session lifetime.
func TestLogReturnsPromptly(t *testing.T) {
	sender := &captureLogSender{}
	if err := (&decisionService{sessions: map[string]struct{}{}}).Log(
		context.Background(), &v2.LogRequest{SessionId: "s1"}, sender,
	); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(sender.events) != 0 {
		t.Errorf("Log emitted %d events, want 0 (the SDK owns heartbeats)", len(sender.events))
	}
}
