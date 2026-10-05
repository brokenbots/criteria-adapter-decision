// Command criteria-adapter-decision is the standalone out-of-process decision
// adapter binary. It serves the protocol-v2 decision adapter via the public
// Go SDK: OpenSession validates the System One config and secret channel
// strictly, Execute validates its questions and state inputs strictly and
// fail-closed (ADR-0013 M2, Kanboard 201) before any HTTP traffic, then calls
// the System One backend through the decisionclient and routes the result
// onto the engine surface (ADR-0013 D3/D5/D6, Kanboard 202/203): outcome
// "success" carries the answers and usage verbatim as outputs, every other
// failure class maps onto outcome "failure" with a typed error payload, and
// one adapter_event is emitted per Execute call. When the session config
// names an outcome_question (ADR-0013 D3, Kanboard 203), the selected choice
// of that one choice question maps verbatim onto the step outcome — the only
// semantic crossing the adapter boundary; everything else routes graph-side.
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
// applies no defaults), timeout, retries, and outcome_question are optional.
const (
	configKeyBaseURL         = "base_url"
	configKeyModel           = "model"
	configKeyTimeout         = "timeout"
	configKeyRetries         = "retries"
	configKeyOutcomeQuestion = "outcome_question"
)

// apiKeyName is the only secret this adapter consumes: the bearer credential
// for the System One endpoint. Declared in InfoResponse.Secrets so hosts
// resolve it over the secret channel; an absent key means unauthenticated
// access.
const apiKeyName = "api_key"

// outcomeSuccess and outcomeFailure are the adapter's entire outcome
// vocabulary (ADR-0013 outcome_pure): decisions either succeed with outputs
// or fail with a typed error payload. It is also the only set a step's
// allowed_outcomes may narrow to, so a mapped outcome_question choice lands
// on one of these two outcomes.
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// outcomeOutOfSetKind is the error.kind of the strict outcome_question
// mapping's routed failure: the selected choice is not a member of the
// step's allowed_outcomes set (same posture as the copilot adapter's
// submit_outcome rejection).
const outcomeOutOfSetKind = "outcome_out_of_set"

// Adapter event kinds; one adapter_event is emitted per Execute call.
const (
	eventDecisionCompleted = "decision.completed"
	eventDecisionFailed    = "decision.failed"
)

// decisionSession is the immutable per-session state: the wired System One
// client, the per-call timeout, the redaction values, the allowed outcome
// set, and the optional outcome_question (ADR-0013 D3: the one choice
// question whose selected choice maps verbatim onto the step outcome; ""
// = absent, every step stays outcome_pure). It carries no execution state —
// Execute is stateless and safe for concurrent use over one session.
type decisionSession struct {
	client          *decisionclient.SystemOneClient
	baseURL         string
	model           string
	retries         int
	timeout         time.Duration // 0 = no per-call timeout
	secrets         []string      // redaction values (api key; per-step overlays at Execute)
	allowed         map[string]struct{}
	outcomeQuestion string // configured outcome question id ("" = outcome_pure)
}

// decisionService implements adapterhost.Service. Only the session registry
// is mutable, guarded by mu; sessions themselves are immutable after open.
type decisionService struct {
	adapterhost.UnimplementedPermissions

	mu       sync.Mutex
	sessions map[string]*decisionSession
}

// adapterDescription is the one-line identity the manifest and
// `criteria adapter list` carry. It describes the boundary contract only —
// never configuration values, which are secret-hygiene risk.
const adapterDescription = "System One decision adapter: validates the step's questions and state strictly (fail-closed), sends them verbatim to a System One decision-model backend, and routes the answers and usage through as outputs — or, when the session config names an outcome_question, maps that one choice's selected choice onto the step outcome (ADR-0013 D2/D3/D7)."

// infoSourceURL is the publishing source_url (D13): the manifest and the
// publish pipeline both carry it.
const infoSourceURL = "https://github.com/brokenbots/criteria-adapter-decision"

// adapterPlatforms are the GOOS/GOARCH pairs the publish pipeline
// cross-compiles and the manifest declares.
func adapterPlatforms() []string {
	return []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}
}

// infoConfigSchema is the compile-time config contract (ADR-0013 D2): a
// non-empty schema makes the compiler strict — only these keys are accepted
// in the adapter config{} block, values are type-checked, and required
// fields (base_url, model) are enforced at compile time, before any session
// handshake.
//
// Field types use only the host manifest's well-known set
// {string, number, boolean, object, array}: the SDK emits them verbatim into
// adapter.yaml, which the host parses strictly.
func infoConfigSchema() *v2.AdapterSchemaProto {
	return &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
		// Required (OpenSession fails the handshake without them): no
		// default is applied for the endpoint or the model.
		configKeyBaseURL: {
			Type: "string", Required: true,
			Description: "Required. Fully-qualified base URL of the System One endpoint; the adapter appends /v1/systemone (e.g. https://s1.typesafe.ai cloud, or a local System One-compatible endpoint such as http://localhost:8080). No default is applied.",
		},
		configKeyModel: {
			Type: "string", Required: true,
			Description: "Required. The System One decision model to invoke (e.g. jev, clef, clef-flash, or a versioned model id). No default is applied.",
		},
		// Optional; absent means no per-call deadline — never "0s"
		// (OpenSession rejects a zero duration), so the schema carries no
		// default and absence IS the default.
		configKeyTimeout: {
			Type:        "string",
			Description: "Optional. Per-call HTTP deadline as a positive Go duration (e.g. 45s, 2m); a present but invalid or zero value fails the session open. Absent = no per-call deadline: the call waits until the HTTP transport or the step itself fails.",
		},
		// Optional with the default spelled out (DEFAULTS EXPLICIT):
		// retries default 0 — the first non-retryable error fails the step.
		configKeyRetries: {
			Type: "number", DefaultStr: "0",
			Description: "Optional. Extra attempts for retryable failures (HTTP 429 and 5xx; Retry-After is honored). Default: 0 — the first error fails the step.",
		},
		configKeyOutcomeQuestion: {
			Type:        "string",
			Description: "Optional. Bareword id of the ONE choice question whose selected choice maps verbatim onto the step outcome (ADR-0013 D3). Absent or empty = no mapping: every step is outcome_pure and succeeds regardless of allowed_outcomes. The named question must appear in the step's questions with type choice, and its selected choice must be a member of the step's declared outcomes, or the step fails with the typed outcome_out_of_set payload.",
		},
	}}
}

// infoInputSchema is the compile-time input contract: steps may pass exactly
// these keys, across BOTH the per-step input{} and secret_input{} blocks.
//
// No field is Required here even though Execute fails closed without
// questions/state: the compiler validates secret_input{} against the same
// schema, so a Required questions/state would falsely fail every step that
// carries a secret binding. Requiredness is the runtime contract instead —
// Execute fails closed before any HTTP traffic when they are absent — and
// the descriptions say so.
func infoInputSchema() *v2.AdapterSchemaProto {
	return &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
		"questions": {
			Type:        "string",
			Description: "Required at runtime (Execute fails closed before any HTTP traffic when absent or malformed): the questions JSON array, decoded strictly. Each question object carries {id, type, instructions, criteria}: id is a bareword (ASCII letters, digits, or underscores, not starting with a digit, unique per array), instructions is a non-empty string, and criteria matches the type — choice: a non-empty {option name: description} object; score: a non-empty ordered level list (e.g. [\"low\",\"medium\",\"high\"]); noul: an optional yes/no gloss string.",
		},
		"state": {
			Type:        "string",
			Description: "Required at runtime (Execute fails closed before any HTTP traffic when absent or malformed): the state JSON — a JSON string, object, or array — decoded strictly.",
		},
		apiKeyName: {
			Type: "string", Sensitive: true,
			Description: "Optional. The System One api credential (sent as Authorization: Bearer <key>; absent = unauthenticated access). Sensitive: bind it only through secret_input (a secret-tainted value) or the adapter's secrets{} block; Execute rejects it in the plaintext input channel.",
		},
	}}
}

// infoOutputSchema is the compile-time output contract: steps may reference
// exactly these keys as steps.<step>.<key> (and in outcome schema subsets).
// answers and usage are the success payload — byte-verbatim from the backend;
// error is the typed failure payload every routed failure class emits. None
// of them is Required: the payload produced depends on the step outcome
// (success carries answers+usage, failure carries error), and undeclared or
// missing keys are tolerated by the host's typed-output decoder.
func infoOutputSchema() *v2.AdapterSchemaProto {
	return &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
		"answers": {
			Type:        "array",
			Description: "The per-question answers JSON array, byte-verbatim from the backend (present on outcome success). Entries are positionally aligned with the input questions and each answers[i] carries {id, type, ...value fields}: choice = {choice, [legend], [probabilities]}, score = {score}, noul = {noul}; every entry may carry a numeric confidence in [0, 1] for graph-side gating.",
		},
		"usage": {
			Type:        "object",
			Description: "The backend usage object, byte-verbatim as returned by the System One response envelope's usage (present on outcome success; may be empty).",
		},
		"error": {
			Type:        "object",
			Description: "The typed failure payload (present on outcome failure): {kind, status, message, retryable[, allowed]}. kind is one of http, auth, timeout, decode, transport, canceled, or outcome_out_of_set (the strict outcome_question mapping); status is the HTTP status when the failure came from an HTTP response; retryable marks whether another attempt would help; allowed lists the step's declared outcomes when kind is outcome_out_of_set.",
		},
	}}
}

// declaredSecrets is the secrets declaration (name → description). The wire
// form carries no per-secret required flag — hosts resolve by name, and
// api_key is optional by contract: absent means unauthenticated access.
func declaredSecrets() map[string]string {
	return map[string]string{
		apiKeyName: "Optional bearer credential for the System One endpoint (Authorization: Bearer <key>; absent = unauthenticated access, never an open error).",
	}
}

// Info declares the adapter identity, the compile-time config/input/output
// contracts (ADR-0013 D2/D7), the api_key secret, and parallel safety:
// session state is immutable after open, so concurrent Executes are safe.
func (s *decisionService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "decision",
		Description:        adapterDescription,
		Version:            Version,
		SourceUrl:          infoSourceURL,
		SdkProtocolVersion: "2",
		Platforms:          adapterPlatforms(),
		Capabilities:       []string{"parallel_safe"},
		ConfigSchema:       infoConfigSchema(),
		InputSchema:        infoInputSchema(),
		OutputSchema:       infoOutputSchema(),
		Secrets:            declaredSecrets(),
	}, nil
}

// OpenSession validates the session contract fail-closed: config carries
// exactly {base_url, model, timeout, retries, outcome_question} with
// base_url/model required, secrets carries at most api_key, and
// allowed_outcomes may only narrow to the success|failure vocabulary. An
// opened session is wired and validated — a bad base_url or model can never
// reach Execute.
//
// outcome_question (ADR-0013 D3) names the ONE choice question whose
// selected choice maps onto the step outcome. Questions are per-step
// Execute inputs here (never declared on the session's contract), so the
// id-exists and type=choice checks run at Execute against each step's
// questions; OpenSession validates the config value itself fail-closed —
// it must be a bareword question id, because an id outside the question-id
// grammar could never match a validated question and would silently
// disable the mapping.
func (s *decisionService) OpenSession(_ context.Context, request *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	config := request.GetConfig()
	for key := range config {
		switch key {
		case configKeyBaseURL, configKeyModel, configKeyTimeout, configKeyRetries, configKeyOutcomeQuestion:
		default:
			return nil, fmt.Errorf("unknown config key %q; allowed keys: base_url, model, timeout, retries, outcome_question", key)
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
	// An absent or empty value is the default: no outcome mapping (every
	// step stays outcome_pure). A present value must be a bareword question
	// id — the grammar every validated question id obeys, so anything else
	// could never match.
	outcomeQuestion := ""
	if raw := config[configKeyOutcomeQuestion]; raw != "" {
		if !decisionclient.IsBareword(raw) {
			return nil, fmt.Errorf("config outcome_question must be a bareword question id (ASCII letters, digits, or underscores, not starting with a digit); got %q", raw)
		}
		outcomeQuestion = raw
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
		client:          client,
		baseURL:         baseURL,
		model:           model,
		retries:         retries,
		timeout:         timeout,
		secrets:         secrets,
		allowed:         allowed,
		outcomeQuestion: outcomeQuestion,
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
//
// When the session config names an outcome_question (ADR-0013 D3), the step
// mapping applies: the selected choice of that choice question becomes the
// step outcome verbatim, but only if it is a member of the step's
// allowed_outcomes set — otherwise the step routes onto outcome "failure"
// with the typed out-of-set payload. Steps whose questions don't carry the
// configured id stay outcome_pure (always success), so one opened session
// can serve steps with and without the question. With no outcome_question
// configured, every step is outcome_pure and the outcome is always
// "success" regardless of allowed_outcomes.
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
	// api_key is schema-declared as an input-field name, but it may only be
	// bound through secret_input or the adapter secrets{} block: never as a
	// plaintext input value on the taint-visible input channel. Reject any
	// other key — deterministically, in sorted order.
	var unknown []string
	for key := range input {
		if key != "questions" && key != "state" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown input key %q; the step input carries only %q and %q (api_key is a secret — bind it via secret_input or the adapter secrets{} block)", unknown[0], "questions", "state")
	}
	questions, err := decisionclient.ParseQuestions([]byte(questionsJSON))
	if err != nil {
		return err
	}
	// ADR-0013 D3 mapping resolution: questions are per-step Execute inputs
	// here (never declared on the session's contract), so the configured
	// outcome question resolves per step. A step whose questions don't
	// carry the id is an outcome_pure step; a step carrying it with a type
	// that can't produce a choice fails closed before any HTTP traffic.
	mappedIndex, err := mappedOutcomeIndex(sess.outcomeQuestion, questions)
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

	// Outcome mapping (ADR-0013 D3): on a mapped step the selected choice
	// of the configured question maps verbatim onto the step outcome, and
	// only a member of that step's allowed_outcomes set may do so —
	// anything else fails closed with the typed out-of-set payload. An
	// outcome_pure step (no configured outcome_question, or the step's
	// questions don't carry the configured id) always succeeds regardless
	// of allowed_outcomes.
	outcome := outcomeSuccess
	if mappedIndex >= 0 {
		choice := resp.Answers[mappedIndex].Choice
		allowed := stepAllowedOutcomes(request.GetAllowedOutcomes())
		if !containsOutcome(allowed, choice) {
			return executeMappingFailure(sess, request, sink, questions[mappedIndex].ID, choice, allowed, latency.Milliseconds())
		}
		outcome = choice
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

	// The outputs are exactly the upstream answers and usage, byte-verbatim
	// — no sugar projections, no re-encoding. On a mapped step the outcome
	// is the choice itself; the outputs never change with it.
	outputs := fmt.Sprintf(`{"answers":%s,"usage":%s}`, resp.AnswersJSON, resp.UsageJSON)
	return sendResult(sink, &v2.ExecuteResult{
		Outcome:     outcome,
		OutputsJson: []byte(outputs),
	})
}

// mappedOutcomeIndex resolves the session's configured outcome_question
// (ADR-0013 D3) against the step's validated questions. It returns the
// index of the question the mapping reads, or -1 when this step's questions
// don't carry the id — an outcome_pure step, not a config error, so one
// opened session can serve steps with and without the question. A carried
// question must be type=choice: anything else is a config error and the
// step fails closed (before any HTTP traffic).
func mappedOutcomeIndex(configured string, questions []decisionclient.Question) (int, error) {
	if configured == "" {
		return -1, nil
	}
	for i, q := range questions {
		if q.ID != configured {
			continue
		}
		if q.Type != decisionclient.TypeChoice {
			return -1, fmt.Errorf("config outcome_question names question %q with type %q; the strict Choice-to-outcome mapping requires type %q", configured, q.Type, decisionclient.TypeChoice)
		}
		return i, nil
	}
	return -1, nil
}

// stepAllowedOutcomes expands a step's allowed_outcomes narrowing: the
// empty list is the adapter's full success|failure vocabulary. The list was
// already validated against that vocabulary (no unknown or duplicate
// entries), so the expansion preserves the given order verbatim.
func stepAllowedOutcomes(allowed []string) []string {
	if len(allowed) == 0 {
		return []string{outcomeSuccess, outcomeFailure}
	}
	return allowed
}

// containsOutcome reports whether allowed names the outcome want.
func containsOutcome(allowed []string, want string) bool {
	for _, name := range allowed {
		if name == want {
			return true
		}
	}
	return false
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
	// json.Marshal of a map emits deterministic (sorted) keys, and the
	// values are plain strings/ints/bools, so the encodes cannot fail.
	return routeFailure(sess, request, sink, map[string]any{
		"kind":      string(de.Kind),
		"status":    de.Status,
		"message":   redactSecrets(de.Message, redactionValues(sess, request)),
		"retryable": de.Retryable,
	}, latencyMS)
}

// executeMappingFailure routes the strict outcome_question mapping's one
// failure (ADR-0013 D3): the selected choice is not a member of the step's
// allowed_outcomes set. Same surface as a failed decision call — the
// "decision.failed" adapter_event and the outcome "failure" ExecuteResult —
// with the typed payload {error: {kind: outcome_out_of_set, allowed: [...],
// message, status, retryable}} (same posture as the copilot adapter's
// submit_outcome rejection; not retryable — the mapping is a strict
// graph-side contract, and what to do about an out-of-set choice routes
// graph-side).
func executeMappingFailure(sess *decisionSession, request *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender, questionID, choice string, allowed []string, latencyMS int64) error {
	// The payload feeds BOTH the proto adapter_event (structpb only accepts
	// []any of primitives) and the JSON result, so the allowed list must be
	// []any — a []string would fall out of the event as an _encode_error.
	allowedAny := make([]any, len(allowed))
	for i, name := range allowed {
		allowedAny[i] = name
	}
	return routeFailure(sess, request, sink, map[string]any{
		"kind":      outcomeOutOfSetKind,
		"status":    0,
		"message":   redactSecrets(fmt.Sprintf("answer for question %q selected choice %q, which is not one of the step's allowed outcomes %v", questionID, choice, allowed), redactionValues(sess, request)),
		"retryable": false,
		"allowed":   allowedAny,
	}, latencyMS)
}

// routeFailure emits the shared routed-failure surface: the
// "decision.failed" adapter_event carrying the error data plus the latency,
// then the terminal outcome "failure" ExecuteResult with the payload
// {error: <data>}.
func routeFailure(sess *decisionSession, request *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender, errorData map[string]any, latencyMS int64) error {
	secrets := redactionValues(sess, request)
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
