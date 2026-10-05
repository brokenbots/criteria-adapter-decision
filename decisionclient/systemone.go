// Package decisionclient provides the isolated HTTP client for the System One
// decision-model wire format (POST /v1/systemone), as defined by ADR-0013 and
// exposed by [TypeSafe AI](https://docs.typesafe.ai) (Jev) in the cloud and
// [Ollama >= v0.35.1](https://github.com/ollama/ollama/releases/tag/v0.35.1)
// (Clef / Clef Flash) locally.
//
// The client is deliberately minimal and uniform across backends: it never
// applies defaults for base_url or model (an unset value is an error, never a
// package-level default), it draws no distinction between backend types, and
// it sends the same {model, state, questions} request wire shape to every
// System One endpoint. Requests are validated fail-closed before any HTTP
// traffic (strict question/state contract, no unknown keys), and answers
// decode losslessly onto typed [Question]/[Answer] values — every decode
// defect is an error, never a silent drop. Deadlines and cancellation come
// from the caller's context; the client imposes no timeout of its own.
package decisionclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// errPrefix tags every error produced by this package so failures are
// attributable at a glance.
const errPrefix = "decisionclient: "

// systemOnePath is the System One decision-model endpoint appended to the
// configured base URL.
const systemOnePath = "/v1/systemone"

// maxResponseBytes bounds the response body read. System One decision
// responses are small JSON documents; the cap keeps a hostile or broken
// backend from exhausting memory.
const maxResponseBytes = 16 << 20 // 16 MiB

// maxErrorBodyBytes bounds the response-body excerpt carried in a
// non-2xx status error.
const maxErrorBodyBytes = 512

// systemOneRequest is the System One request wire shape. Field order is the
// wire order (encoding/json marshals struct fields in declaration order).
type systemOneRequest struct {
	Model     string     `json:"model"`
	State     State      `json:"state"`
	Questions []Question `json:"questions"`
}

// SystemOneClient calls a System One decision-model backend. All fields are
// fixed by New; a client is safe for concurrent use.
type SystemOneClient struct {
	// endpoint is the fully-resolved POST target (base URL + systemOnePath).
	endpoint string
	// model names the decision model the backend answers with.
	model string
	// apiKey is the optional api credential; empty means no auth header.
	apiKey string
	// hc issues the requests. Unexported; a fresh client with no timeout is
	// created by New so concurrent users never share a mutable default.
	hc *http.Client
}

// New creates a SystemOneClient that POSTs the System One wire format to
// baseURL + /v1/systemone using the given decision model. baseURL and model
// must be provided explicitly; there are no implicit defaults.
//
// apiKey is optional: when a non-empty key is given, requests carry
// an Authorization header in bearer-token form; when it is absent (or empty), no
// Authorization header is sent. The backend choice is made entirely by the
// caller through baseURL/model — the client distinguishes nothing between
// System One backends.
func New(baseURL, model string, apiKey ...string) (*SystemOneClient, error) {
	if baseURL == "" {
		return nil, errors.New(errPrefix + "base_url is required; no defaults are applied")
	}
	if model == "" {
		return nil, errors.New(errPrefix + "model is required; no defaults are applied")
	}
	switch len(apiKey) {
	case 0:
		// no key
	case 1:
		// validated below
	default:
		return nil, errors.New(errPrefix + "New takes a single optional api key")
	}
	key := ""
	if len(apiKey) == 1 {
		key = apiKey[0]
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"invalid base_url %q: %w", baseURL, err)
	}
	if base.User != nil {
		return nil, errors.New(errPrefix + "base_url must not embed credentials; pass them via the api key (Bearer)")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf(errPrefix+"invalid base_url %q: want an http or https URL", baseURL)
	}
	if base.Host == "" {
		return nil, fmt.Errorf(errPrefix+"invalid base_url %q: missing host", baseURL)
	}
	endpoint, err := url.JoinPath(base.String(), systemOnePath)
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"invalid base_url %q: %w", baseURL, err)
	}

	client := &SystemOneClient{
		endpoint: endpoint,
		model:    model,
		apiKey:   key,
		hc:       &http.Client{},
	}
	return client, nil
}

// Decision posts the decision request for state and questions to the backend
// and returns its raw JSON response body verbatim — no decoding, no
// re-encoding, byte-identical to what the backend sent. Decode the response
// with [DecodeAnswers] against the same questions.
//
// The request is validated FAIL-CLOSED before any HTTP traffic: the state
// must be a JSON string, object, or array, and every question must pass the
// strict questions contract (bareword id, known type, non-empty
// instructions, criteria matching the type, no unknown keys — see
// [ParseQuestions]). Every validation defect is an error and the backend is
// never contacted for an invalid request.
//
// The request carries exactly {model, state, questions}: the configured
// model, the state (string, object, or array — number lexemes inside
// objects/arrays are preserved), and the questions in the order given.
func (c *SystemOneClient) Decision(ctx context.Context, state State, questions []Question) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New(errPrefix + "nil context")
	}
	if err := validateRequest(state, questions); err != nil {
		return nil, err
	}

	rawRequest, err := json.Marshal(systemOneRequest{Model: c.model, State: state, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(rawRequest))
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The Authorization header is added exactly when a key was configured;
	// otherwise no auth header is sent at all.
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"POST %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf(errPrefix+"read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf(errPrefix+"POST %s: unexpected status %s: %s", c.endpoint, resp.Status, bodyExcerpt(raw))
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf(errPrefix+"response exceeds %d bytes; refusing the truncated body", maxResponseBytes)
	}
	if !json.Valid(raw) {
		return nil, errors.New(errPrefix + "response is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// bodyExcerpt renders the first maxErrorBodyBytes bytes of a response body
// for an error message, collapsing empty bodies to a placeholder. Used for
// actionable-cause surfacing only; it never carries request auth data.
func bodyExcerpt(raw []byte) string {
	if len(raw) == 0 {
		return "(empty body)"
	}
	excerpt := raw
	if len(excerpt) > maxErrorBodyBytes {
		excerpt = excerpt[:maxErrorBodyBytes]
		return string(excerpt) + "…"
	}
	return string(excerpt)
}
