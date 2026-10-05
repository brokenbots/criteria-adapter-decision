// decisionclient/response_test.go — full {model, answers, usage} response
// decode: strict envelope, lossless answers, byte-verbatim sub-bytes (M3).

package decisionclient

import (
	"errors"
	"strings"
	"testing"
)

// testFullQuestions covers every question type for the response decoder.
func testFullQuestions() []Question {
	return []Question{
		{ID: "q1", Type: TypeNoul, Instructions: "Decide.", Gloss: "yes: go ahead, no: stop"},
		{ID: "q2", Type: TypeChoice, Instructions: "Pick.", Options: map[string]string{"go": "advance", "halt": "stop"}},
		{ID: "q3", Type: TypeScore, Instructions: "Grade.", Levels: []string{"low", "high"}},
	}
}

// TestDecodeDecisionResponseLosslessAndVerbatim pins the decode contract:
// typed answers aligned to question order, and the answers/usage sub-bytes
// preserved byte-for-byte (asymmetric whitespace and number lexemes intact)
// so adapters can pass them through without re-encoding.
func TestDecodeDecisionResponseLosslessAndVerbatim(t *testing.T) {
	t.Parallel()

	// Deliberately asymmetric whitespace, number lexemes, non-sorted answers
	// keys, and unknown-in-our-model usage keys: the decode must not
	// re-encode a single byte.
	body := `{  "usage" : { "input_tokens":812, "output_tokens": 0, "custom":1.2300e2 }, "model": "clef-1.4.2",   "answers":{` +
		`"q1":{"type":"noul","noul":"yes"},` +
		`"q3":{"type":"score","score":1,"legend":"high","probabilities":{"low":0.1,"high":0.9},"confidence":1},` +
		`"q2":{"type":"choice","choice":"go","probabilities":{"go":0.75,"halt":0.25},"confidence":0.9}` +
		`}}`

	resp, err := DecodeDecisionResponse([]byte(body), testFullQuestions())
	if err != nil {
		t.Fatalf("DecodeDecisionResponse() err = %v; want nil", err)
	}
	if resp.Model != "clef-1.4.2" {
		t.Errorf("Model = %q; want %q", resp.Model, "clef-1.4.2")
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("len(Answers) = %d; want 3", len(resp.Answers))
	}
	if resp.Answers[0].QuestionID != "q1" || resp.Answers[0].Noul != "yes" {
		t.Errorf("Answers[0] = %+v; want noul yes", resp.Answers[0])
	}
	if resp.Answers[1].Choice != "go" || resp.Answers[1].Confidence != 0.9 {
		t.Errorf("Answers[1] = %+v; want choice go, confidence 0.9", resp.Answers[1])
	}
	if resp.Answers[2].Score != 1 || resp.Answers[2].Legend != "high" {
		t.Errorf("Answers[2] = %+v; want score 1 legend high", resp.Answers[2])
	}

	wantAnswersStart := `{"q1":{"type":"noul","noul":"yes"},"q3":{"type":"score","score":1,"legend":"high","probabilities":{"low":0.1,"high":0.9},"confidence":1},"q2":{"type":"choice","choice":"go","probabilities":{"go":0.75,"halt":0.25},"confidence":0.9}}`
	if string(resp.AnswersJSON) != wantAnswersStart {
		t.Errorf("AnswersJSON = %s; want byte-verbatim %s", resp.AnswersJSON, wantAnswersStart)
	}
	wantUsageJSON := `{ "input_tokens":812, "output_tokens": 0, "custom":1.2300e2 }`
	if string(resp.UsageJSON) != wantUsageJSON {
		t.Errorf("UsageJSON = %s; want byte-verbatim %s", resp.UsageJSON, wantUsageJSON)
	}
}

// TestDecodeDecisionResponseStrictEnvelope covers the fail-closed envelope
// rules: exact {model, answers, usage} keys, non-empty model id, map-shaped
// answers keyed by question id, and an object usage.
func TestDecodeDecisionResponseStrictEnvelope(t *testing.T) {
	t.Parallel()

	questions := []Question{{ID: "q1", Type: TypeNoul, Instructions: "Decide."}}

	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{name: "unknown top-level key", body: `{"model":"m","answers":{},"usage":{},"extra":1}`, wantSub: `unknown field "extra"`},
		{name: "missing model", body: `{"answers":{} ,"usage":{}}`, wantSub: `response "model" is required`},
		{name: "empty model", body: `{"model":"","answers":{},"usage":{}}`, wantSub: `response "model" is required`},
		{name: "missing answers", body: `{"model":"m","usage":{}}`, wantSub: `response "answers" is required`},
		{name: "null answers", body: `{"model":"m","answers":null,"usage":{}}`, wantSub: `response "answers" is required`},
		{name: "answers not a map", body: `{"model":"m","answers":[{"id":"q1"}],"usage":{}}`, wantSub: "answers must be a JSON map of answer objects keyed by question id"},
		{name: "missing usage", body: `{"model":"m","answers":{}}`, wantSub: `response "usage" is required`},
		{name: "null usage", body: `{"model":"m","answers":{},"usage":null}`, wantSub: `response "usage" is required`},
		{name: "usage not an object", body: `{"model":"m","answers":{},"usage":[{"t":1}]}`, wantSub: "usage must be a JSON object"},
		{name: "no answer for question", body: `{"model":"m","answers":{},"usage":{}}`, wantSub: "no answer for question"},
		{name: "trailing data", body: `{"model":"m","answers":{},"usage":{}} {}`, wantSub: "decode response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeDecisionResponse([]byte(tc.body), questions)
			if err == nil {
				t.Fatalf("DecodeDecisionResponse(%s) err = nil; want %q", tc.body, tc.wantSub)
			}
			var de *DecisionError
			if !errors.As(err, &de) || de.Kind != DecisionErrorKindDecode || de.Retryable {
				t.Errorf("err = %v (%T); want decode-kind DecisionError, never retryable", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q; want containing %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestDecodeDecisionResponseEmptyUsageAllowed pins that an empty usage
// object is a valid (if odd) response — the object shape is the contract,
// not its contents.
func TestDecodeDecisionResponseEmptyUsageAllowed(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","answers":{"q1":{"type":"noul","noul":"yes"}},"usage":{}}`
	resp, err := DecodeDecisionResponse([]byte(body), []Question{{ID: "q1", Type: TypeNoul, Instructions: "Decide."}})
	if err != nil {
		t.Fatalf("DecodeDecisionResponse() err = %v; want nil", err)
	}
	if string(resp.UsageJSON) != "{}" {
		t.Errorf("UsageJSON = %s; want {}", resp.UsageJSON)
	}
}

// TestDecodeDecisionResponseRequiresQuestions anchors the same guard
// DecodeAnswers carries: no questions, no decode.
func TestDecodeDecisionResponseRequiresQuestions(t *testing.T) {
	t.Parallel()

	_, err := DecodeDecisionResponse([]byte(`{"model":"m","answers":{},"usage":{}}`), nil)
	if err == nil {
		t.Fatal("DecodeDecisionResponse(nil questions) err = nil; want error")
	}
	if !strings.Contains(err.Error(), "requires the validated questions") {
		t.Errorf("err = %q; want questions-required message", err.Error())
	}
}
