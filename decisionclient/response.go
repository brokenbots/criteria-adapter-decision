// Package decisionclient — full System One response decode (ADR-0013 M3).

package decisionclient

import (
	"bytes"
	"encoding/json"
)

// DecisionResponse is the decoded, lossless form of a System One response:
//
//   - Model is the versioned model id the backend reports;
//   - Answers are the per-question answers aligned to question order;
//   - AnswersJSON is the upstream answers array, byte-verbatim;
//   - UsageJSON is the upstream usage object, byte-verbatim.
//
// The sub-byte fields preserve exactly what the backend sent so adapters can
// pass answers/usage through to engine outputs without a decode/re-encode
// round-trip changing a single byte.
type DecisionResponse struct {
	Model       string
	Answers     []Answer
	AnswersJSON json.RawMessage
	UsageJSON   json.RawMessage
}

// decisionResponseEnvelope is the strict-decode view of the full System One
// response. The wire contract has exactly the three keys {model, answers,
// usage} — the same hardline that rejects unknown keys at every other
// object boundary — and all three are required.
type decisionResponseEnvelope struct {
	Model   *string          `json:"model"`
	Answers *json.RawMessage `json:"answers"`
	Usage   *json.RawMessage `json:"usage"`
}

// DecodeDecisionResponse decodes a full System One response (the raw body
// returned by [SystemOneClient.Decision]) against the validated questions.
// Decoding is strict and lossless, exactly like [DecodeAnswers] for the
// answers array it contains:
//
//   - the envelope carries exactly the {model, answers, usage} keys, all
//     required;
//   - the model id is non-empty ("versioned model id");
//   - answers is a JSON array decoded losslessly onto the questions;
//   - usage is a JSON object (possibly empty), kept byte-verbatim.
//
// Every defect is a decode-kind [DecisionError]; nothing is dropped or
// defaulted, so a broken response fails the step instead of passing through
// silently.
func DecodeDecisionResponse(raw []byte, questions []Question) (*DecisionResponse, error) {
	if len(questions) == 0 {
		return nil, decodeError(errPrefix + "decode response requires the validated questions")
	}
	var env decisionResponseEnvelope
	if err := strictUnmarshal(raw, &env); err != nil {
		return nil, decodeError(errPrefix+"decode response: "+err.Error(), err)
	}
	if env.Model == nil || *env.Model == "" {
		return nil, decodeError(errPrefix + `decode response: response "model" is required`)
	}
	if env.Answers == nil || isNullJSON(*env.Answers) {
		return nil, decodeError(errPrefix + `decode response: response "answers" is required`)
	}

	var entries []json.RawMessage
	if err := strictUnmarshal(*env.Answers, &entries); err != nil {
		return nil, decodeError(errPrefix+"decode response: answers must be a JSON array of answer objects: "+err.Error(), err)
	}
	if env.Usage == nil || isNullJSON(*env.Usage) {
		return nil, decodeError(errPrefix + `decode response: response "usage" is required`)
	}
	var usage map[string]json.RawMessage
	if err := strictUnmarshal(*env.Usage, &usage); err != nil {
		return nil, decodeError(errPrefix+"decode response: usage must be a JSON object: "+err.Error(), err)
	}

	answers, err := decodeAnswerEntries(entries, questions)
	if err != nil {
		return nil, decodeError(err.Error(), err)
	}
	return &DecisionResponse{
		Model:       *env.Model,
		Answers:     answers,
		AnswersJSON: *env.Answers,
		UsageJSON:   *env.Usage,
	}, nil
}

// isNullJSON reports whether raw is the JSON null literal.
func isNullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
