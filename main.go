// Command criteria-adapter-decision is the standalone out-of-process decision
// adapter binary. This scaffold serves the protocol-v2 decision adapter via
// the public Go SDK: sessions are wired, Execute validates its questions and
// state inputs strictly and fail-closed (ADR-0013 M2, Kanboard 201), and
// decision execution returns an error until the System One decision path
// lands (Kanboard 202-206).
//
// The System One client itself (the isolated HTTP client for the ADR-0013
// decision wire format) lives in decisionclient and is ready to be wired into
// Execute by the follow-up workstreams.
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/brokenbots/criteria-adapter-decision/decisionclient"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// Version is the adapter's protocol-v2 version string.
const Version = "2.0.0"

// decisionNotImplemented is the fail-closed Execute error: the scaffold ships
// no decision logic, and guessing an outcome would be worse than failing the
// step. Kanboard 202-206 replace this with the real System One path.
const decisionNotImplemented = "decision execution is not implemented in this scaffold (Kanboard 202-206)"

type decisionService struct {
	adapterhost.UnimplementedPermissions

	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *decisionService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "decision",
		Version:            Version,
		SourceUrl:          "https://github.com/brokenbots/criteria-adapter-decision",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe"},
	}, nil
}

func (s *decisionService) OpenSession(_ context.Context, request *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[request.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *decisionService) Execute(_ context.Context, request *v2.ExecuteRequest, _ adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[request.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", request.GetSessionId())
	}
	// Strict fail-closed input validation (ADR-0013 M2, Kanboard 201): the
	// step request must carry the questions and state JSON, and everything
	// malformed — unknown keys, wrong shapes, missing criteria — fails the
	// step here, before any decision work or HTTP traffic happens. The
	// parsed values are re-derived by the System One path (202-206).
	input := request.GetInput()
	questionsJSON, ok := input["questions"]
	if !ok {
		return errors.New(`input must carry a "questions" key containing the questions JSON (see decisionclient.ParseQuestions)`)
	}
	stateJSON, ok := input["state"]
	if !ok {
		return errors.New(`input must carry a "state" key containing the state JSON (see decisionclient.ParseState)`)
	}
	if _, err := decisionclient.ParseQuestions([]byte(questionsJSON)); err != nil {
		return err
	}
	if _, err := decisionclient.ParseState([]byte(stateJSON)); err != nil {
		return err
	}
	// Fail-closed scaffold: the System One decision path lands in Kanboard
	// 202-206; until then Execute must err rather than guess an outcome.
	return fmt.Errorf(decisionNotImplemented)
}

func (s *decisionService) Log(context.Context, *v2.LogRequest, adapterhost.LogEventSender) error {
	// The SDK owns the log stream and its heartbeat for the full session
	// lifetime, so Log returns immediately instead of parking on the context
	// (pre-CRI-164 SDKs needed the adapter to hold the stream open).
	return nil
}

func (s *decisionService) CloseSession(_ context.Context, request *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, request.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&decisionService{sessions: map[string]struct{}{}})
}
