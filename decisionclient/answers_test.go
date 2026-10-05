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
// lossless mapping aligned to question order. On the wire answers are a JSON
// map keyed by question id — id lives in the map key and every object states
// its question type — so source key order can be anything; the typed answers
// always align to the questions' order.
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
			raw:  `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":0.9,"fail":0.1},"confidence":0.8}}}`,
			want: `[{"QuestionID":"pick","Choice":"pass","Probabilities":{"fail":0.1,"pass":0.9},"Confidence":0.8}]`,
		},
		{
			name: "choice answer with partial probabilities (known keys only)",
			qs:   []Question{pick},
			raw:  `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":0.5}}}`,
			want: `[{"QuestionID":"pick","Choice":"pass","Probabilities":{"pass":1},"Confidence":0.5}]`,
		},
		{
			name: "score answer with legend and name-keyed probabilities",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":2,"legend":"high","probabilities":{"low":0.1,"mid":0.2,"high":0.7},"confidence":0.9}}}`,
			want: `[{"QuestionID":"grade","Score":2,"Legend":"high","Probabilities":{"high":0.7,"low":0.1,"mid":0.2},"Confidence":0.9}]`,
		},
		{
			name: "score answer at the zero index",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":0,"legend":"low","probabilities":{"low":1},"confidence":0}}}`,
			want: `[{"QuestionID":"grade","Score":0,"Legend":"low","Probabilities":{"low":1}}]`,
		},
		{
			name: "probability-weighted float score with index-keyed legend and probabilities (live shape)",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":1.1394,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"0":0.1,"1":0.2,"2":0.7},"confidence":0.9}}}`,
			want: `[{"QuestionID":"grade","Score":1,"Legend":"mid","Probabilities":{"0":0.1,"1":0.2,"2":0.7},"Confidence":0.9}]`,
		},
		{
			name: "probability-weighted float score with string legend",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":1.1394,"legend":"mid","probabilities":{"low":0.2,"mid":0.5,"high":0.3},"confidence":0.9}}}`,
			want: `[{"QuestionID":"grade","Score":1,"Legend":"mid","Probabilities":{"high":0.3,"low":0.2,"mid":0.5},"Confidence":0.9}]`,
		},
		{
			name: "fractional score rounds to the nearest level (halves away from zero)",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":0.5,"legend":"mid","probabilities":{"mid":1},"confidence":0.5}}}`,
			want: `[{"QuestionID":"grade","Score":1,"Legend":"mid","Probabilities":{"mid":1},"Confidence":0.5}]`,
		},
		{
			name: "integral score with index-keyed legend object",
			qs:   []Question{grade},
			raw:  `{"answers":{"grade":{"type":"score","score":2,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"high":1},"confidence":0.9}}}`,
			want: `[{"QuestionID":"grade","Score":2,"Legend":"high","Probabilities":{"high":1},"Confidence":0.9}]`,
		},
		{
			name: "noul verdict only",
			qs:   []Question{verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":"yes"}}}`,
			want: `[{"QuestionID":"verdict","Noul":"yes"}]`,
		},
		{
			name: "noul verdict no",
			qs:   []Question{verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":"no"}}}`,
			want: `[{"QuestionID":"verdict","Noul":"no"}]`,
		},
		{
			name: "noul probability at or above half reads yes (live Ollama shape)",
			qs:   []Question{verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":0.7886}}}`,
			want: `[{"QuestionID":"verdict","Noul":"yes"}]`,
		},
		{
			name: "noul probability below half reads no",
			qs:   []Question{verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":0.25}}}`,
			want: `[{"QuestionID":"verdict","Noul":"no"}]`,
		},
		{
			name: "noul probability exactly half reads yes",
			qs:   []Question{verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":0.5}}}`,
			want: `[{"QuestionID":"verdict","Noul":"yes"}]`,
		},
		{
			name: "answers keyed by id map back to question order regardless of key order",
			qs:   []Question{pick, grade, verdict},
			raw:  `{"answers":{"verdict":{"type":"noul","noul":"no"},"grade":{"type":"score","score":1,"legend":"mid","probabilities":{"low":0.2,"mid":0.5,"high":0.3},"confidence":0.75},"pick":{"type":"choice","choice":"fail","probabilities":{"pass":0.25,"fail":0.75},"confidence":0.25}}}`,
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
		{name: "envelope unknown key", raw: `{"answers":{},"extra":1}`, wantErr: `decode answers: json: unknown field "extra"`},
		{name: "envelope not an object", raw: `[{"pick":{}}]`, wantErr: "decode answers: json: cannot unmarshal array into Go value of type"},
		{name: "envelope missing answers key", raw: `{"other":{}}`, wantErr: `decode answers: json: unknown field "other"`},
		{name: "answers null", raw: `{"answers":null}`, wantErr: `decode answers: the "answers" map keyed by question id is required`},
		{name: "answers missing entirely", raw: `{}`, wantErr: `decode answers: the "answers" map keyed by question id is required`},
		{name: "answers not a map", raw: `{"answers":[{"id":"pick"}]}`, wantErr: "decode answers: answers must be a JSON map of answer objects keyed by question id"},
		{name: "envelope trailing data", raw: `{"answers":{}} {}`, wantErr: "trailing data"},

		// Answer object strictness.
		{name: "answer unknown key", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":0.5,"verdict":"ok"}}}`, wantErr: `answers["pick"]: decode answer: json: unknown field "verdict"`},
		{name: "answer missing type", raw: `{"answers":{"pick":{"choice":"pass","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: answer "type" is required`},
		{name: "answer type not a string", raw: `{"answers":{"pick":{"type":3,"choice":"pass","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: decode answer: json: cannot unmarshal number`},
		{name: "answer type mismatch", raw: `{"answers":{"pick":{"type":"noul","noul":"yes"}}}`, wantErr: `answers["pick"]: answer type "noul" does not match the question's type "choice"`},
		{name: "answer for unknown question", raw: `{"answers":{"other":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["other"]: answer for unknown question`},
		{name: "unknown answer key not a bareword", raw: `{"answers":{"does run pass":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["does run pass"]: answer for unknown question`},
		{name: "answer not an object", raw: `{"answers":{"pick":"pass"}}`, wantErr: `answers["pick"]: decode answer: json: cannot unmarshal string`},
		{name: "answer keys validated in sorted order", raw: `{"answers":{"zzz":{"type":"noul"},"aaa":{"type":"noul"}}}`, wantErr: `answers["aaa"]: answer for unknown question`},

		// Type-stated choice answers.
		{name: "choice missing choice field", raw: `{"answers":{"pick":{"type":"choice","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: choice answer must carry a non-empty "choice" field`},
		{name: "choice empty choice value", raw: `{"answers":{"pick":{"type":"choice","choice":"","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: choice answer must carry a non-empty "choice" field`},
		{name: "choice unknown option", raw: `{"answers":{"pick":{"type":"choice","choice":"maybe","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: choice answer selected "maybe", which is not one of the question's options`},
		{name: "choice carrying score field", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","score":1,"probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: answer for a choice question must not carry "score"`},
		{name: "choice carrying legend field", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","legend":"mid","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: answer for a choice question must not carry "legend"`},
		{name: "choice carrying noul field", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","noul":"yes","probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: answer for a choice question must not carry "noul"`},
		{name: "choice probabilities reject level-index keys", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"0":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: probabilities key "0" is not one of the question's options`},
		{name: "choice missing probabilities", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","confidence":0.5}}}`, wantErr: `answers["pick"]: choice answer must carry a "probabilities" field`},
		{name: "choice empty probabilities", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{},"confidence":0.5}}}`, wantErr: `answers["pick"]: probabilities must be non-empty`},
		{name: "choice probabilities unknown key", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"maybe":1},"confidence":0.5}}}`, wantErr: `answers["pick"]: probabilities key "maybe" is not one of the question's options`},
		{name: "choice probability out of range", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1.5},"confidence":0.5}}}`, wantErr: `answers["pick"]: probabilities["pass"] = 1.5 is outside [0, 1]`},
		{name: "choice negative probability", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":-0.1},"confidence":0.5}}}`, wantErr: `answers["pick"]: probabilities["pass"] = -0.1 is outside [0, 1]`},
		{name: "choice missing confidence", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1}}}}`, wantErr: `answers["pick"]: choice answer must carry a "confidence" field`},
		{name: "choice confidence out of range", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":2}}}`, wantErr: `answers["pick"]: confidence 2 is outside [0, 1]`},

		// Type-stated score answers.
		{name: "score missing score field", raw: `{"answers":{"grade":{"type":"score","legend":"mid","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer must carry a "score" field`},
		{name: "score null", raw: `{"answers":{"grade":{"type":"score","score":null,"legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer must carry a "score" field`},
		{name: "score not a number", raw: `{"answers":{"grade":{"type":"score","score":"1","legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score must be a JSON number`},
		{name: "score not a number (bool)", raw: `{"answers":{"grade":{"type":"score","score":true,"legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score must be a JSON number`},
		{name: "score out of range (too high)", raw: `{"answers":{"grade":{"type":"score","score":3,"legend":"high","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score 3 is outside the question's 3 levels`},
		{name: "score negative", raw: `{"answers":{"grade":{"type":"score","score":-1,"legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score -1 is outside the question's 3 levels`},
		{name: "score negative fractional", raw: `{"answers":{"grade":{"type":"score","score":-0.2,"legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score -0.2 is outside the question's 3 levels (negative)`},
		{name: "fractional score rounding past the last level", raw: `{"answers":{"grade":{"type":"score","score":2.6,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score 2.6 is outside the question's 3 levels`},
		{name: "score not a finite number", raw: `{"answers":{"grade":{"type":"score","score":1e999,"legend":"low","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score "1e999" is not a number`},
		{name: "score missing legend", raw: `{"answers":{"grade":{"type":"score","score":1,"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer must carry a non-empty "legend" field`},
		{name: "score null legend", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":null,"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer must carry a non-empty "legend" field`},
		{name: "score legend mismatch", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"high","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend "high" does not match level 1 ("mid")`},
		{name: "score legend neither string nor object", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":3,"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend must be a level name string or a level index → name map`},
		{name: "score legend object missing a level", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"0":"low","2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend is missing level 1 ("mid")`},
		{name: "score legend object wrong level name", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"0":"low","1":"nope","2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend names level 1 as "nope", want "mid"`},
		{name: "score legend object out-of-range key", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"0":"low","1":"mid","5":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend key "5" is not a level index in [0, 3)`},
		{name: "score legend object non-index key", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"first":"low","1":"mid","2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend key "first" is not a level index in [0, 3)`},
		{name: "score legend object duplicate level index", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"00":"low","0":"low","1":"mid","2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend repeats level index 0`},
		{name: "score legend object wrong value shape", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":{"0":"low","1":2,"2":"high"},"probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: score answer legend must map level index → level name`},
		{name: "score carrying choice field", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"mid","choice":"pass","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: answer for a score question must not carry "choice"`},
		{name: "score carrying noul field", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"mid","noul":"yes","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: answer for a score question must not carry "noul"`},
		{name: "score probabilities unknown level", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"mid","probabilities":{"peak":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: probabilities key "peak" is not one of the question's levels`},
		{name: "score probabilities unknown level index", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"mid","probabilities":{"3":1},"confidence":0.5}}}`, wantErr: `answers["grade"]: probabilities key "3" is not one of the question's levels`},
		{name: "score missing confidence", raw: `{"answers":{"grade":{"type":"score","score":1,"legend":"mid","probabilities":{"low":1}}}}`, wantErr: `answers["grade"]: score answer must carry a "confidence" field`},

		// Type-stated noul answers.
		{name: "noul missing noul field", raw: `{"answers":{"verdict":{"type":"noul"}}}`, wantErr: `answers["verdict"]: noul answer must carry a "noul" field`},
		{name: "noul verdict not yes/no", raw: `{"answers":{"verdict":{"type":"noul","noul":"Yes"}}}`, wantErr: `answers["verdict"]: noul answer must be exactly "yes" or "no"; got "Yes"`},
		{name: "noul verdict empty", raw: `{"answers":{"verdict":{"type":"noul","noul":""}}}`, wantErr: `answers["verdict"]: noul answer must be exactly "yes" or "no"; got ""`},
		{name: "noul probability out of range", raw: `{"answers":{"verdict":{"type":"noul","noul":1.4}}}`, wantErr: `answers["verdict"]: noul answer 1.4 is outside [0, 1]`},
		{name: "noul negative probability", raw: `{"answers":{"verdict":{"type":"noul","noul":-0.1}}}`, wantErr: `answers["verdict"]: noul answer -0.1 is outside [0, 1]`},
		{name: "noul boolean verdict", raw: `{"answers":{"verdict":{"type":"noul","noul":true}}}`, wantErr: `answers["verdict"]: noul answer must be the string "yes", the string "no", or a probability-like number in [0, 1]`},
		{name: "noul object verdict", raw: `{"answers":{"verdict":{"type":"noul","noul":{"v":1}}}}`, wantErr: `answers["verdict"]: noul answer must be the string "yes", the string "no", or a probability-like number in [0, 1]`},
		{name: "noul carrying confidence", raw: `{"answers":{"verdict":{"type":"noul","noul":"yes","confidence":0.5}}}`, wantErr: `answers["verdict"]: answer for a noul question must not carry "confidence"`},
		{name: "noul carrying probabilities", raw: `{"answers":{"verdict":{"type":"noul","noul":"yes","probabilities":{"yes":1}}}}`, wantErr: `answers["verdict"]: answer for a noul question must not carry "probabilities"`},
		{name: "noul carrying score", raw: `{"answers":{"verdict":{"type":"noul","noul":"yes","score":1}}}`, wantErr: `answers["verdict"]: answer for a noul question must not carry "score"`},

		// Coverage.
		{name: "empty answers map", raw: `{"answers":{}}`, wantErr: `decode answers: no answer for question "pick"`},
		{name: "missing one answer", raw: `{"answers":{"pick":{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":0.5},"grade":{"type":"score","score":1,"legend":"mid","probabilities":{"low":1},"confidence":0.5}}}`, wantErr: `decode answers: no answer for question "verdict"`},
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
		if _, err := DecodeAnswers([]byte(`{"answers":{}}`), qs); err == nil || !strings.Contains(err.Error(), "requires the validated questions") {
			t.Fatalf("DecodeAnswers(%v) err = %v; want requires-questions error", qs, err)
		}
	}
}

// TestDecodeAnswersDuplicateQuestionIDs pins that a duplicated id in the
// question list is rejected at the decode boundary (coverage against the
// answers map is a bijection).
func TestDecodeAnswersDuplicateQuestionIDs(t *testing.T) {
	t.Parallel()

	qs := []Question{{ID: "q1", Type: TypeNoul, Instructions: "a"}, {ID: "q1", Type: TypeNoul, Instructions: "b"}}
	raw := `{"answers":{"q1":{"type":"noul","noul":"yes"}}}`
	_, err := DecodeAnswers([]byte(raw), qs)
	if err == nil || !strings.Contains(err.Error(), `duplicate question id "q1"`) {
		t.Fatalf("DecodeAnswers() err = %v; want duplicate question id", err)
	}
}

// TestDecodeAnswersZeroValuesNeverPass pins that "absent" payloads cannot
// be smuggled in as zero values: a null payload must decode-fail rather than
// silently zero.
func TestDecodeAnswersZeroValuesNeverPass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "null choice", raw: `{"answers":{"pick":{"type":"choice","choice":null,"probabilities":{"pass":1},"confidence":0.5}}}`, wantErr: `choice answer must carry a non-empty "choice"`},
		{name: "null noul", raw: `{"answers":{"verdict":{"type":"noul","noul":null}}}`, wantErr: `noul answer must carry a "noul"`},
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
