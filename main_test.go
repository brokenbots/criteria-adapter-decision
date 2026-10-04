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

// TestExecuteFailClosed pins the scaffold contract: Execute errors out and
// emits no result events, so a wired-up pipeline fails the step loudly rather
// than reporting a guessed outcome.
func TestExecuteFailClosed(t *testing.T) {
	s := &decisionService{sessions: map[string]struct{}{}}
	ctx := context.Background()
	const sid = "s1"
	if _, err := s.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	sink := &captureSink{}
	err := s.Execute(ctx, &v2.ExecuteRequest{SessionId: sid}, sink)
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
