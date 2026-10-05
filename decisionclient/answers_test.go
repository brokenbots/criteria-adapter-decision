package decisionclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeFixture questions used by the answer-decode tests.
func decodeFixture() []Question {
	return []Question{
		{ID: "pick", Type: TypeChoice, Instructions: "Pick one.", Options: map[string]string{"pass": "Passed.", "fail": "Failed."}},
		{ID: "grade", Type: TypeScore, Instructions: "Grade it.", Levels: []string{"low", "mid", "high"}},
		{ID: "verdict", Type: TypeNoul, Instructions: "Yes or no?", Gloss: "yes when clean"},
	}
}

// TestDecodeAnswersValidShapes covers every answer shape and pins verbatim,
// lossless mapping aligned to question order (answers may arrive in any
// order on the wire).
func TestDecodeAnswersValidShapes(t *testing.T) {
	t.Parallel()

	pick := Question{ID: "pick", Type: TypeChoice, Instructions: "Pick one.", Options: map[string]string{"pass": "Passed.", "fail": "Failed."}}
	grade := Question{ID: "grade", Type: TypeScore, Instructions: "Grade it.", Levels: []string{"low", "mid", "high"}}
	verdict := Question{ID: "verdict", Type: TypeNoul, Instructions: "Yes or no?", Gloss: "yes when clean"}

	tests := []struct {
		name string
		qs   []Question
		raw  string
		want string // JSON rendering of the decoded answers for comparison
	}{
		{
			name: "choice answer with full probabilities",
			qs:   []Question{pick},
			raw:  `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":0.9,"fail":0.1},"confidence":0.8}]}`,
			want: `[{"QuestionID":"pick","Choice":"pass","Probabilities":{"fail":0.1,"pass":0.9},"Confidence":0.8}]`,
		},
		{
			name: "choice answer with partial probabilities (known keys only)",
			qs:   []Question{pick},
			raw:  `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1},"confidence":0.5}]}`,
			want: `[{"QuestionID":"pick","Choice":"pass","Probabilities":{"pass":1},"Confidence":0.5}]`,
		},
		{
			name: "score answer with legend and probabilities",
			qs:   []Question{grade},
			raw:  `{"answers":[{"id":"grade","score":2,"legend":"high","probabilities":{"low":0.1,"mid":0.2,"high":0.7},"confidence":0.9}]}`,
			want: `[{"QuestionID":"grade","Score":2,"Legend":"high","Probabilities":{"high":0.7,"low":0.1,"mid":0.2},"Confidence":0.9}]`,
		},
		{
			name: "score answer at the zero index",
			qs:   []Question{grade},
			raw:  `{"answers":[{"id":"grade","score":0,"legend":"low","probabilities":{"low":1},"confidence":0}]}`,
			want: `[{"QuestionID":"grade","Score":0,"Legend":"low","Probabilities":{"low":1}}]`,
		},
		{
			name: "noul verdict only",
			qs:   []Question{verdict},
			raw:  `{"answers":[{"id":"verdict","noul":"yes"}]}`,
			want: `[{"QuestionID":"verdict","Noul":"yes"}]`,
		},
		{
			name: "noul verdict no",
			qs:   []Question{verdict},
			raw:  `{"answers":[{"id":"verdict","noul":"no"}]}`,
			want: `[{"QuestionID":"verdict","Noul":"no"}]`,
		},
		{
			name: "mixed answers out of order are mapped back to question order",
			qs:   []Question{pick, grade, verdict},
			raw:  `{"answers":[{"id":"verdict","noul":"no"},{"id":"grade","score":1,"legend":"mid","probabilities":{"low":0.2,"mid":0.5,"high":0.3},"confidence":0.75},{"id":"pick","choice":"fail","probabilities":{"pass":0.25,"fail":0.75},"confidence":0.25}]}`,
			want: `[{"QuestionID":"pick","Choice":"fail","Probabilities":{"fail":0.75,"pass":0.25},"Confidence":0.25},{"QuestionID":"grade","Score":1,"Legend":"mid","Probabilities":{"high":0.3,"low":0.2,"mid":0.5},"Confidence":0.75},{"QuestionID":"verdict","Noul":"no"}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeAnswers([]byte(tc.raw), tc.qs)
			if err != nil {
				t.Fatalf("DecodeAnswers(%s) err = %v; want nil", tc.raw, err)
			}
			rendered, err := renderAnswers(got)
			if err != nil {
				t.Fatalf("marshal decoded answers: %v", err)
			}
			if rendered != tc.want {
				t.Fatalf("DecodeAnswers(%s) = %s; want %s", tc.raw, rendered, tc.want)
			}
		})
	}
}

// renderAnswers marshals answers with the zero-value fields of unset payload
// types hidden, so each test compares only the fields that carry data.
func renderAnswers(answers []Answer) (string, error) {
	type viewAnswer struct {
		QuestionID    string             `json:"QuestionID"`
		Choice        string             `json:"Choice,omitempty"`
		Score         *int               `json:"Score,omitempty"`
		Legend        string             `json:"Legend,omitempty"`
		Noul          string             `json:"Noul,omitempty"`
		Probabilities map[string]float64 `json:"Probabilities,omitempty"`
		Confidence    float64            `json:"Confidence,omitempty"`
	}
	views := make([]viewAnswer, 0, len(answers))
	for _, a := range answers {
		v := viewAnswer{
			QuestionID:    a.QuestionID,
			Choice:        a.Choice,
			Legend:        a.Legend,
			Noul:          a.Noul,
			Probabilities: a.Probabilities,
			Confidence:    a.Confidence,
		}
		// A score answer always carries a legend; expose the index even
		// when it is the zero value (level 0).
		if a.Legend != "" {
			score := a.Score
			v.Score = &score
		}
		views = append(views, v)
	}
	raw, err := json.Marshal(views)
	return string(raw), err
}

// TestDecodeAnswersRejectMatrix is the fail-closed answer matrix: every
// defective shape, cross-type field, out-of-range value, and coverage
// defect fails decoding instead of passing through silently.
func TestDecodeAnswersRejectMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		// Envelope strictness.
		{name: "envelope unknown key", raw: `{"answers":[],"extra":1}`, wantErr: `decode answers: json: unknown field "extra"`},
		{name: "envelope not an object", raw: `[{"id":"pick"}]`, wantErr: "decode answers: json: cannot unmarshal array into Go value of type"},
		{name: "envelope missing answers key", raw: `{"other":[]}`, wantErr: `decode answers: json: unknown field "other"`},
		{name: "answers not an array", raw: `{"answers":{}}`, wantErr: "decode answers: json: cannot unmarshal object"},
		{name: "envelope trailing data", raw: `{"answers":[]} {}`, wantErr: "trailing data"},

		// Answer object strictness.
		{name: "answer unknown key", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1},"confidence":0.5,"verdict":"ok"}]}`, wantErr: `answers[0]: decode answer: json: unknown field "verdict"`},
		{name: "answer missing id", raw: `{"answers":[{"choice":"pass","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: "answers[0]: answer id is required"},
		{name: "answer id not a bareword", raw: `{"answers":[{"id":"does run pass","choice":"pass","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for unknown question "does run pass"`},
		{name: "answer for unknown question", raw: `{"answers":[{"id":"other","choice":"pass","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for unknown question "other"`},
		{name: "duplicate answer", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1},"confidence":0.5},{"id":"pick","choice":"fail","probabilities":{"fail":1},"confidence":0.5}]}`, wantErr: `answers[1]: duplicate answer for question "pick"`},
		{name: "answer id type mismatch", raw: `{"answers":[{"id":3}]}`, wantErr: "answers[0]: decode answer: json: cannot unmarshal number"},
		{name: "answer not an object", raw: `{"answers":["pick"]}`, wantErr: "answers[0]: decode answer: json: cannot unmarshal string"},

		// Choice answers.
		{name: "choice missing choice field", raw: `{"answers":[{"id":"pick","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: choice answer must carry a non-empty "choice" field`},
		{name: "choice empty choice value", raw: `{"answers":[{"id":"pick","choice":"","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: choice answer must carry a non-empty "choice" field`},
		{name: "choice unknown option", raw: `{"answers":[{"id":"pick","choice":"maybe","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: choice answer selected "maybe", which is not one of the question's options`},
		{name: "choice carrying score field", raw: `{"answers":[{"id":"pick","choice":"pass","score":1,"probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for a choice question must not carry "score"`},
		{name: "choice carrying legend field", raw: `{"answers":[{"id":"pick","choice":"pass","legend":"mid","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for a choice question must not carry "legend"`},
		{name: "choice carrying noul field", raw: `{"answers":[{"id":"pick","choice":"pass","noul":"yes","probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for a choice question must not carry "noul"`},
		{name: "choice missing probabilities", raw: `{"answers":[{"id":"pick","choice":"pass","confidence":0.5}]}`, wantErr: `answers[0]: choice answer must carry a "probabilities" field`},
		{name: "choice empty probabilities", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{},"confidence":0.5}]}`, wantErr: "answers[0]: probabilities must be non-empty"},
		{name: "choice probabilities unknown key", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"maybe":1},"confidence":0.5}]}`, wantErr: `answers[0]: probabilities key "maybe" is not one of the question's options`},
		{name: "choice probability out of range", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1.5},"confidence":0.5}]}`, wantErr: `answers[0]: probabilities["pass"] = 1.5 is outside [0, 1]`},
		{name: "choice negative probability", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":-0.1},"confidence":0.5}]}`, wantErr: "answers[0]: probabilities[\"pass\"] = -0.1 is outside [0, 1]"},
		{name: "choice missing confidence", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1}}]}`, wantErr: `answers[0]: choice answer must carry a "confidence" field`},
		{name: "choice confidence out of range", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1},"confidence":2}]}`, wantErr: "answers[0]: confidence 2 is outside [0, 1]"},

		// Score answers.
		{name: "score missing score field", raw: `{"answers":[{"id":"grade","legend":"mid","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `answers[0]: score answer must carry a "score" field`},
		{name: "score out of range (too high)", raw: `{"answers":[{"id":"grade","score":3,"legend":"high","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: "answers[0]: score 3 is outside the question's 3 levels"},
		{name: "score negative", raw: `{"answers":[{"id":"grade","score":-1,"legend":"low","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: "answers[0]: score -1 is outside the question's 3 levels"},
		{name: "score missing legend", raw: `{"answers":[{"id":"grade","score":1,"probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `answers[0]: score answer must carry a non-empty "legend" field`},
		{name: "score legend mismatch", raw: `{"answers":[{"id":"grade","score":1,"legend":"high","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `answers[0]: score answer legend "high" does not match level 1 ("mid")`},
		{name: "score carrying choice field", raw: `{"answers":[{"id":"grade","score":1,"legend":"mid","choice":"pass","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for a score question must not carry "choice"`},
		{name: "score carrying noul field", raw: `{"answers":[{"id":"grade","score":1,"legend":"mid","noul":"yes","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `answers[0]: answer for a score question must not carry "noul"`},
		{name: "score probabilities unknown level", raw: `{"answers":[{"id":"grade","score":1,"legend":"mid","probabilities":{"peak":1},"confidence":0.5}]}`, wantErr: `answers[0]: probabilities key "peak" is not one of the question's levels`},
		{name: "score missing confidence", raw: `{"answers":[{"id":"grade","score":1,"legend":"mid","probabilities":{"low":1}}]}`, wantErr: `answers[0]: score answer must carry a "confidence" field`},

		// Noul answers.
		{name: "noul missing noul field", raw: `{"answers":[{"id":"verdict"}]}`, wantErr: `answers[0]: noul answer must carry a "noul" field`},
		{name: "noul verdict not yes/no", raw: `{"answers":[{"id":"verdict","noul":"Yes"}]}`, wantErr: `answers[0]: noul answer must be exactly "yes" or "no"; got "Yes"`},
		{name: "noul verdict empty", raw: `{"answers":[{"id":"verdict","noul":""}]}`, wantErr: `answers[0]: noul answer must be exactly "yes" or "no"; got ""`},
		{name: "noul carrying confidence", raw: `{"answers":[{"id":"verdict","noul":"yes","confidence":0.5}]}`, wantErr: `answers[0]: answer for a noul question must not carry "confidence"`},
		{name: "noul carrying probabilities", raw: `{"answers":[{"id":"verdict","noul":"yes","probabilities":{"yes":1}}]}`, wantErr: `answers[0]: answer for a noul question must not carry "probabilities"`},
		{name: "noul carrying score", raw: `{"answers":[{"id":"verdict","noul":"yes","score":1}]}`, wantErr: `answers[0]: answer for a noul question must not carry "score"`},

		// Coverage.
		{name: "empty answers array", raw: `{"answers":[]}`, wantErr: `decode answers: no answer for question "pick"`},
		{name: "missing one answer", raw: `{"answers":[{"id":"pick","choice":"pass","probabilities":{"pass":1},"confidence":0.5},{"id":"grade","score":1,"legend":"mid","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `decode answers: no answer for question "verdict"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeAnswers([]byte(tc.raw), decodeFixture())
			if err == nil {
				t.Fatalf("DecodeAnswers(%s) err = nil; want containing %q", tc.raw, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeAnswers(%s) err = %q; want containing %q", tc.raw, err.Error(), tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), errPrefix) {
				t.Fatalf("DecodeAnswers(%s) err = %q; want %q prefix", tc.raw, err.Error(), errPrefix)
			}
		})
	}
}

// TestDecodeAnswersQuestionsRequired pins the decode-side guard for calls
// without validated questions.
func TestDecodeAnswersQuestionsRequired(t *testing.T) {
	t.Parallel()

	for _, qs := range [][]Question{nil, {}} {
		if _, err := DecodeAnswers([]byte(`{"answers":[]}`), qs); err == nil || !strings.Contains(err.Error(), "requires the validated questions") {
			t.Fatalf("DecodeAnswers(%v) err = %v; want requires-questions error", qs, err)
		}
	}
}

// TestDecodeAnswersDuplicateQuestionIDs pins that a duplicated id in the
// question list is rejected at the decode boundary (coverage is a
// bijection).
func TestDecodeAnswersDuplicateQuestionIDs(t *testing.T) {
	t.Parallel()

	qs := []Question{{ID: "q1", Type: TypeNoul, Instructions: "a"}, {ID: "q1", Type: TypeNoul, Instructions: "b"}}
	raw := `{"answers":[{"id":"q1","noul":"yes"},{"id":"q1","noul":"no"}]}`
	_, err := DecodeAnswers([]byte(raw), qs)
	if err == nil || !strings.Contains(err.Error(), `duplicate question id "q1"`) {
		t.Fatalf("DecodeAnswers() err = %v; want duplicate question id", err)
	}
}

// TestDecodeAnswersZeroValuesNeverPass pins that "absent" payloads cannot
// be smuggled in as zero values: a score answer with score explicitly null
// is "key absent" for presence purposes only if the key is missing; a
// null payload must decode-fail rather than silently zero.
func TestDecodeAnswersZeroValuesNeverPass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "null choice", raw: `{"answers":[{"id":"pick","choice":null,"probabilities":{"pass":1},"confidence":0.5}]}`, wantErr: `choice answer must carry a non-empty "choice"`},
		{name: "null score", raw: `{"answers":[{"id":"grade","score":null,"legend":"low","probabilities":{"low":1},"confidence":0.5}]}`, wantErr: `score answer must carry a "score"`},
		{name: "null noul", raw: `{"answers":[{"id":"verdict","noul":null}]}`, wantErr: `noul answer must carry a "noul"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeAnswers([]byte(tc.raw), decodeFixture())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeAnswers(%s) err = %v; want containing %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}
