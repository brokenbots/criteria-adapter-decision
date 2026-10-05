// Command criteria-adapter-decision is the standalone out-of-process decision
// adapter binary. It serves the protocol-v2 decision adapter via the public
// Go SDK: OpenSession validates the System One config and secret channel
// strictly, Execute validates its questions and state inputs strictly and
// fail-closed (ADR-0013 M2, Kanboard 201) before any HTTP traffic, then calls
// the System One backend through the decisionclient and routes the result
// onto the engine surface (ADR-0013 D3/D5/D6, Kanboard 202): outcome
// "success" carries the answers and usage verbatim as outputs, every other
// failure class maps onto outcome "failure" with a typed error payload, and
// one adapter_event is emitted per Execute call.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brokenbots/criteria-adapter-decision/decisionclient"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// Version is the adapter's protocol-v2 version string.
const Version = "2.0.0"

// OpenSession config keys; base_url and model are required (decisionclient
// applies no defaults), timeout and retries are optional.
const (
	configKeyBaseURL = "base_url"
	configKeyModel   = "model"
	configKeyTimeout = "timeout"
	configKeyRetries = "retries"
)

// apiKeyName is the only secret this adapter consumes: the bearer credential
// for the System One endpoint. Declared in InfoResponse.Secrets so hosts
// resolve it over the secret channel; an absent key means unauthenticated
// access.
const apiKeyName = "api_key"

// outcomeSuccess and outcomeFailure are the adapter's entire outcome
// vocabulary (ADR-0013 outcome_pure): decisions either succeed with outputs
// or fail with a typed error payload.
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// Adapter event kinds; one adapter_event is emitted per Execute call.
const (
	eventDecisionCompleted = "decision.completed"
	eventDecisionFailed    = "decision.failed"
)

// decisionSession is the immutable per-session state: the wired System One
// client, the per-call timeout, the redaction values, and the allowed
// outcome set. It carries no execution state — Execute is stateless and safe
// for concurrent use over one session.
type decisionSession struct {
	client  *decisionclient.SystemOneClient
	baseURL string
	model   string
	retries int
	timeout time.Duration // 0 = no per-call timeout
	secrets []string      // redaction values (api key; per-step overlays at Execute)
	allowed map[string]struct{}
}

// decisionService implements adapterhost.Service. Only the session registry
// is mutable, guarded by mu; sessions themselves are immutable after open.
type decisionService struct {
	adapterhost.UnimplementedPermissions

	mu       sync.Mutex
	sessions map[string]*decisionSession
}

// Info declares the adapter identity, the api_key secret, and parallel
// safety: sessions are immutable after open, so concurrent Executes are
// safe.
func (s *decisionService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "decision",
		Version:            Version,
		SourceUrl:          "https://github.com/brokenbots/criteria-adapter-decision",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe"},
		Secrets:            map[string]string{apiKeyName: "Bearer credential for the System One endpoint (optional)"},
	}, nil
}

// OpenSession validates the session contract fail-closed: config carries
// exactly {base_url, model, timeout, retries} with base_url/model required,
// secrets carries at most api_key, and allowed_outcomes may only narrow to
// the success|failure vocabulary. An opened session is wired and validated —
// a bad base_url or model can never reach Execute.
func (s *decisionService) OpenSession(_ context.Context, request *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	config := request.GetConfig()
	for key := range config {
		switch key {
		case configKeyBaseURL, configKeyModel, configKeyTimeout, configKeyRetries:
		default:
			return nil, fmt.Errorf("unknown config key %q; allowed keys: base_url, model, timeout, retries", key)
		}
	}
	baseURL := config[configKeyBaseURL]
	if baseURL == "" {
		return nil, errors.New("config must carry base_url: the System One endpoint (no defaults are applied)")
	}
	model := config[configKeyModel]
	if model == "" {
		return nil, errors.New("config must carry model: the decision model name (no defaults are applied)")
	}
	timeout := time.Duration(0)
	if raw := config[configKeyTimeout]; raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("config timeout must be a positive Go duration (e.g. 45s), got %q", raw)
		}
		timeout = parsed
	}
	retries := 0
	if raw := config[configKeyRetries]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("config retries must be a non-negative whole number of extra attempts, got %q", raw)
		}
		retries = parsed
	}

	var apiKey string
	secrets := make([]string, 0, 1)
	for name, value := range request.GetSecrets() {
		if name != apiKeyName {
			return nil, fmt.Errorf("unknown secret %q; this adapter declares only %q", name, apiKeyName)
		}
		apiKey = value
		if value != "" {
			secrets = append(secrets, value)
		}
	}

	client, err := decisionclient.New(baseURL, model, apiKey)
	if err != nil {
		return nil, err
	}
	if retries > 0 {
		client = client.WithRetries(retries)
	}

	allowed, err := checkAllowedOutcomes(request.GetAllowedOutcomes())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[request.GetSessionId()] = &decisionSession{
		client:  client,
		baseURL: baseURL,
		model:   model,
		retries: retries,
		timeout: timeout,
		secrets: secrets,
		allowed: allowed,
	}
	return &v2.OpenSessionResponse{}, nil
}

// checkAllowedOutcomes validates an allowed-outcomes list against the
// adapter's success|failure vocabulary. An empty list means the full
// vocabulary; anything outside it (or duplicated) is rejected.
func checkAllowedOutcomes(allowed []string) (map[string]struct{}, error) {
	if len(allowed) == 0 {
		return map[string]struct{}{outcomeSuccess: {}, outcomeFailure: {}}, nil
	}
	vocabulary := map[string]struct{}{outcomeSuccess: {}, outcomeFailure: {}}
	out := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if _, known := vocabulary[name]; !known {
			return nil, fmt.Errorf("unknown outcome %q; this adapter defines only %q and %q", name, outcomeSuccess, outcomeFailure)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate outcome %q in allowed_outcomes", name)
		}
		out[name] = struct{}{}
	}
	return out, nil
}

// Execute runs one decision step: strict input validation (M2) before any
// HTTP traffic, the typed System One call, one adapter_event for the call,
// and exactly one ExecuteResult. Success carries the answers and usage
// verbatim as outputs; every routed failure class maps onto outcome
// "failure" with a typed error payload — nothing errors out of this method
// in routed paths (M2 input validation and unknown sessions remain
// engine-visible errors by design).
func (s *decisionService) Execute(ctx context.Context, request *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	sess := s.getSession(request.GetSessionId())
	if sess == nil {
		return fmt.Errorf("unknown session %q", request.GetSessionId())
	}
	// Strict fail-closed input validation (ADR-0013 M2, Kanboard 201): the
	// step request must carry the questions and state JSON, and everything
	// malformed — unknown keys, wrong shapes, missing criteria — fails the
	// step here, before any decision work or HTTP traffic happens.
	input := request.GetInput()
	questionsJSON, ok := input["questions"]
	if !ok {
		return errors.New(`input must carry a "questions" key containing the questions JSON (see decisionclient.ParseQuestions)`)
	}
	stateJSON, ok := input["state"]
	if !ok {
		return errors.New(`input must carry a "state" key containing the state JSON (see decisionclient.ParseState)`)
	}
	questions, err := decisionclient.ParseQuestions([]byte(questionsJSON))
	if err != nil {
		return err
	}
	state, err := decisionclient.ParseState([]byte(stateJSON))
	if err != nil {
		return err
	}
	if _, err := checkAllowedOutcomes(request.GetAllowedOutcomes()); err != nil {
		return err
	}

	// The step may overlay the api key per request; only api_key is a known
	// secret name. The rebuild keeps the session's config and retry knob.
	client := sess.client
	for name, value := range request.GetSecretInputs() {
		if name != apiKeyName {
			return fmt.Errorf("unknown secret input %q; this adapter declares only %q", name, apiKeyName)
		}
		if value != "" {
			rebuilt, err := decisionclient.New(sess.baseURL, sess.model, value)
			if err != nil {
				return err
			}
			if sess.retries > 0 {
				rebuilt = rebuilt.WithRetries(sess.retries)
			}
			client = rebuilt
		}
	}

	callCtx := ctx
	cancel := context.CancelFunc(nil)
	if sess.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, sess.timeout)
		defer cancel()
	}

	start := time.Now()
	raw, err := client.Decision(callCtx, state, questions)
	latency := time.Since(start)
	if err != nil {
		return executeFailure(sess, request, sink, err, latency.Milliseconds())
	}
	resp, err := decisionclient.DecodeDecisionResponse(raw, questions)
	if err != nil {
		return executeFailure(sess, request, sink, err, latency.Milliseconds())
	}

	// One adapter_event per call: the versioned model id from the response,
	// the per-question answers, the usage, and the latency. The step state
	// input never rides events. Best-effort secret hygiene over every
	// string in the payload.
	if err := sendEvent(sink, eventDecisionCompleted, redactionValues(sess, request), map[string]any{
		"model":      resp.Model,
		"answers":    nativeJSON(resp.AnswersJSON),
		"usage":      nativeJSON(resp.UsageJSON),
		"latency_ms": latency.Milliseconds(),
	}); err != nil {
		return err
	}

	// Success = outcome_pure: the outputs are exactly the upstream answers
	// and usage, byte-verbatim — no sugar projections, no re-encoding.
	outputs := fmt.Sprintf(`{"answers":%s,"usage":%s}`, resp.AnswersJSON, resp.UsageJSON)
	return sendResult(sink, &v2.ExecuteResult{
		Outcome:     outcomeSuccess,
		OutputsJson: []byte(outputs),
	})
}

// executeFailure routes a failed decision call onto the engine surface
// (ADR-0013 D6): the "decision.failed" adapter_event and the outcome
// "failure" ExecuteResult with the typed error payload
// {error: {kind, status, message, retryable}}. Routed paths never return
// engine-visible errors.
func executeFailure(sess *decisionSession, request *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender, err error, latencyMS int64) error {
	var de *decisionclient.DecisionError
	if !errors.As(err, &de) {
		de = &decisionclient.DecisionError{Kind: decisionclient.DecisionErrorKindDecode, Message: err.Error()}
	}
	secrets := redactionValues(sess, request)
	// json.Marshal of a map emits deterministic (sorted) keys, and the
	// values are plain strings/ints/bools, so the encodes cannot fail.
	errorData := map[string]any{
		"kind":      string(de.Kind),
		"status":    de.Status,
		"message":   redactSecrets(de.Message, secrets),
		"retryable": de.Retryable,
	}
	if err := sendEvent(sink, eventDecisionFailed, secrets, map[string]any{
		"error":      errorData,
		"latency_ms": latencyMS,
	}); err != nil {
		return err
	}
	errorJSON, err := json.Marshal(map[string]any{"error": errorData})
	if err != nil {
		return fmt.Errorf("decision: encode failure payload: %w", err)
	}
	return sendResult(sink, &v2.ExecuteResult{
		Outcome:     outcomeFailure,
		OutputsJson: errorJSON,
	})
}

// sendEvent emits one AdapterEvent whose string leaves are secret-redacted.
func sendEvent(sink adapterhost.ExecuteEventSender, kind string, secrets []string, data map[string]any) error {
	redacted := make(map[string]any, len(data))
	for key, value := range data {
		redacted[key] = redactJSON(value, secrets)
	}
	payload, err := structpb.NewStruct(redacted)
	if err != nil {
		// Encoding failed; emit a minimal struct so the event kind is
		// preserved and the encode error is diagnosable rather than
		// silently dropped.
		payload, _ = structpb.NewStruct(map[string]any{"_encode_error": err.Error()})
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: kind, Payload: payload},
		},
	})
}

// sendResult emits exactly one ExecuteResult: the terminal event of an
// Execute call.
func sendResult(sink adapterhost.ExecuteEventSender, result *v2.ExecuteResult) error {
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: result},
	})
}

// nativeJSON decodes raw JSON into the structpb-compatible value tree
// (string/float64/bool/nil/map/slice). raw was just strict-decoded, so the
// unmarshal cannot fail; a defect degrades to nil.
func nativeJSON(raw json.RawMessage) any {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

// redactionValues collects every secret value visible to a call: the
// session-held secrets plus the per-step secret inputs.
func redactionValues(sess *decisionSession, request *v2.ExecuteRequest) []string {
	values := append([]string(nil), sess.secrets...)
	for _, value := range request.GetSecretInputs() {
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

// redactJSON walks a decoded JSON value tree and replaces every secret
// occurrence inside string values with redactedPlaceholder.
func redactJSON(value any, secrets []string) any {
	if len(secrets) == 0 {
		return value
	}
	switch typed := value.(type) {
	case string:
		return redactSecrets(typed, secrets)
	case map[string]any:
		for key, inner := range typed {
			typed[key] = redactJSON(inner, secrets)
		}
		return typed
	case []any:
		for i, inner := range typed {
			typed[i] = redactJSON(inner, secrets)
		}
		return typed
	default:
		return value
	}
}

const redactedPlaceholder = "[REDACTED]"

// redactSecrets replaces every non-empty secret value that appears verbatim
// in s with redactedPlaceholder. Secrets are matched longest-first so a
// shorter token cannot corrupt a longer one. This is intentionally narrow
// best-effort hygiene for values the adapter received over the secret
// channel; it does not attempt general secret detection.
func redactSecrets(s string, secrets []string) string {
	if s == "" || len(secrets) == 0 {
		return s
	}
	ordered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if t := strings.TrimSpace(secret); t != "" {
			ordered = append(ordered, t)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	out := s
	for _, secret := range ordered {
		out = strings.ReplaceAll(out, secret, redactedPlaceholder)
	}
	return out
}

// Log returns immediately: the SDK owns the log stream and its heartbeat
// for the full session lifetime, so there is nothing to park on here.
func (s *decisionService) Log(context.Context, *v2.LogRequest, adapterhost.LogEventSender) error {
	return nil
}

// CloseSession drops the session. In-flight Executes keep the session value
// they captured; nothing else depends on the registry entry.
func (s *decisionService) CloseSession(_ context.Context, request *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, request.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

// getSession returns the session by id, or nil when unknown.
func (s *decisionService) getSession(sessionID string) *decisionSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sessionID]
}

func main() {
	adapterhost.Serve(&decisionService{sessions: map[string]*decisionSession{}})
}
