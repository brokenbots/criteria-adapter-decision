package decisionclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseQuestionsValidShapes covers the accepted question forms and
// pins that parsing preserves ids, types, instructions, criteria, and
// order.
func TestParseQuestionsValidShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want []Question
	}{
		{
			name: "choice with option map",
			data: `[{"id":"does_run_pass","type":"choice","instructions":"Pick the run outcome.","criteria":{"pass":"The run passed.","fail":"The run failed."}}]`,
			want: []Question{{ID: "does_run_pass", Type: TypeChoice, Instructions: "Pick the run outcome.", Options: map[string]string{"pass": "The run passed.", "fail": "The run failed."}}},
		},
		{
			name: "score with ordered level list",
			data: `[{"id":"quality","type":"score","instructions":"Grade the run.","criteria":["low","mid","high"]}]`,
			want: []Question{{ID: "quality", Type: TypeScore, Instructions: "Grade the run.", Levels: []string{"low", "mid", "high"}}},
		},
		{
			name: "noul without criteria",
			data: `[{"id":"safe","type":"noul","instructions":"Is the run safe?"}]`,
			want: []Question{{ID: "safe", Type: TypeNoul, Instructions: "Is the run safe?"}},
		},
		{
			name: "noul with yes/no gloss",
			data: `[{"id":"safe","type":"noul","instructions":"Is the run safe?","criteria":"yes when nothing failed"}]`,
			want: []Question{{ID: "safe", Type: TypeNoul, Instructions: "Is the run safe?", Gloss: "yes when nothing failed"}},
		},
		{
			name: "noul with explicit null criteria counts as absent",
			data: `[{"id":"safe","type":"noul","instructions":"Is the run safe?","criteria":null}]`,
			want: []Question{{ID: "safe", Type: TypeNoul, Instructions: "Is the run safe?"}},
		},
		{
			name: "several questions keep order",
			data: `[{"id":"a","type":"noul","instructions":"One."},{"id":"b","type":"noul","instructions":"Two."}]`,
			want: []Question{{ID: "a", Type: TypeNoul, Instructions: "One."}, {ID: "b", Type: TypeNoul, Instructions: "Two."}},
		},
		{
			name: "whitespace-only instructions are valid JSON strings",
			data: `[{"id":"ws","type":"noul","instructions":"   "}]`,
			want: []Question{{ID: "ws", Type: TypeNoul, Instructions: "   "}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseQuestions([]byte(tc.data))
			if err != nil {
				t.Fatalf("ParseQuestions(%s) err = %v; want nil", tc.data, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseQuestions(%s) len = %d; want %d", tc.data, len(got), len(tc.want))
			}
			for i := range tc.want {
				g, w := got[i], tc.want[i]
				if g.ID != w.ID || g.Type != w.Type || g.Instructions != w.Instructions || g.Gloss != w.Gloss {
					t.Fatalf("ParseQuestions(%s) questions[%d] = %+v; want %+v", tc.data, i, g, w)
				}
				if len(g.Options) != len(w.Options) {
					t.Fatalf("ParseQuestions(%s) questions[%d].Options = %v; want %v", tc.data, i, g.Options, w.Options)
				}
				for key, want := range w.Options {
					if g.Options[key] != want {
						t.Fatalf("ParseQuestions(%s) questions[%d].Options[%q] = %q; want %q", tc.data, i, key, g.Options[key], want)
					}
				}
				if strings.Join(g.Levels, "|") != strings.Join(w.Levels, "|") {
					t.Fatalf("ParseQuestions(%s) questions[%d].Levels = %v; want %v", tc.data, i, g.Levels, w.Levels)
				}
			}
		})
	}
}

// TestParseQuestionsRejectMatrix is the fail-closed reject matrix: every
// defective shape must be rejected with an error naming the defect, and
// nothing may be defaulted or dropped.
func TestParseQuestionsRejectMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{name: "not an array", data: `{"id":"q1","type":"noul","instructions":"g"}`, wantErr: "questions must be a JSON array of question objects"},
		{name: "bare string", data: `"q1"`, wantErr: "questions must be a JSON array of question objects"},
		{name: "empty array", data: `[]`, wantErr: "at least one question"},
		{name: "malformed JSON", data: `[{"id":`, wantErr: "questions must be a JSON array of question objects"},

		// Strict object discipline: unknown keys inside a question object.
		{name: "unknown key in question object", data: `[{"id":"q1","type":"noul","instructions":"g","priority":1}]`, wantErr: `decode question: json: unknown field "priority"`},
		{name: "nested object as option description", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":{"a":{"desc":"d","weight":2}}}]`, wantErr: "cannot unmarshal object into Go struct field .a of type string"},

		// id.
		{name: "missing id", data: `[{"type":"noul","instructions":"g"}]`, wantErr: "id is required"},
		{name: "empty id", data: `[{"id":"","type":"noul","instructions":"g"}]`, wantErr: "id must be a bareword"},
		{name: "id with space", data: `[{"id":"does run pass","type":"noul","instructions":"g"}]`, wantErr: "id must be a bareword"},
		{name: "id with dash", data: `[{"id":"does-run-pass","type":"noul","instructions":"g"}]`, wantErr: "id must be a bareword"},
		{name: "id starting with digit", data: `[{"id":"1option","type":"noul","instructions":"g"}]`, wantErr: "id must be a bareword"},
		{name: "non-string id", data: `[{"id":1,"type":"noul","instructions":"g"}]`, wantErr: "cannot unmarshal number into Go struct field rawQuestion.id"},

		// type.
		{name: "missing type", data: `[{"id":"q1","instructions":"g"}]`, wantErr: "type is required"},
		{name: "bad type", data: `[{"id":"q1","type":"emoji","instructions":"g"}]`, wantErr: `type must be "choice", "score", "noul"; got "emoji"`},
		{name: "non-string type", data: `[{"id":"q1","type":true,"instructions":"g"}]`, wantErr: "cannot unmarshal bool into Go struct field rawQuestion.type"},

		// instructions.
		{name: "missing instructions", data: `[{"id":"q1","type":"noul"}]`, wantErr: "instructions is required and must be non-empty"},
		{name: "empty instructions", data: `[{"id":"q1","type":"noul","instructions":""}]`, wantErr: "instructions is required and must be non-empty"},

		// choice criteria.
		{name: "choice without criteria", data: `[{"id":"q1","type":"choice","instructions":"g"}]`, wantErr: "choice questions require criteria: a non-empty option map (option name → description)"},
		{name: "choice criteria null", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":null}]`, wantErr: "choice questions require criteria"},
		{name: "choice criteria empty object", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":{}}]`, wantErr: "choice questions require criteria"},
		{name: "choice criteria as array", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":["pass"]}]`, wantErr: "choice questions require criteria: a non-empty option map"},
		{name: "choice criteria with non-string option", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":{"pass":1}}]`, wantErr: "a non-empty option map"},
		{name: "choice criteria with empty option name", data: `[{"id":"q1","type":"choice","instructions":"g","criteria":{"":"the empty one"}}]`, wantErr: "choice criteria option names must be non-empty"},

		// score criteria.
		{name: "score criteria as map (wrong shape)", data: `[{"id":"q1","type":"score","instructions":"g","criteria":{"low":1,"high":2}}]`, wantErr: "score criteria must be an ordered level list (a JSON array of level names), not an object"},
		{name: "score without criteria", data: `[{"id":"q1","type":"score","instructions":"g"}]`, wantErr: "score questions require criteria: a non-empty ordered level list"},
		{name: "score criteria null", data: `[{"id":"q1","type":"score","instructions":"g","criteria":null}]`, wantErr: "score questions require criteria"},
		{name: "score criteria empty array", data: `[{"id":"q1","type":"score","instructions":"g","criteria":[]}]`, wantErr: "score questions require criteria"},
		{name: "score criteria as scalar", data: `[{"id":"q1","type":"score","instructions":"g","criteria":"low"}]`, wantErr: "a non-empty ordered level list"},
		{name: "score criteria with empty level name", data: `[{"id":"q1","type":"score","instructions":"g","criteria":["low",""]}]`, wantErr: "score criteria level names must be non-empty"},
		{name: "score criteria with duplicate levels", data: `[{"id":"q1","type":"score","instructions":"g","criteria":["low","low"]}]`, wantErr: `score criteria levels must be unique; "low" appears more than once`},
		{name: "score criteria with non-string level", data: `[{"id":"q1","type":"score","instructions":"g","criteria":["low",1]}]`, wantErr: "a non-empty ordered level list"},

		// noul criteria.
		{name: "noul criteria as map", data: `[{"id":"q1","type":"noul","instructions":"g","criteria":{"yes":"y"}}]`, wantErr: "noul criteria, when present, must be a yes/no gloss string"},
		{name: "noul criteria as array", data: `[{"id":"q1","type":"noul","instructions":"g","criteria":["yes"]}]`, wantErr: "noul criteria, when present, must be a yes/no gloss string"},
		{name: "noul criteria as boolean", data: `[{"id":"q1","type":"noul","instructions":"g","criteria":true}]`, wantErr: "noul criteria, when present, must be a yes/no gloss string"},
		{name: "noul criteria empty string", data: `[{"id":"q1","type":"noul","instructions":"g","criteria":""}]`, wantErr: "noul criteria, when present, must be a non-empty yes/no gloss string"},

		// duplicates across the array.
		{name: "duplicate question ids", data: `[{"id":"q1","type":"noul","instructions":"one"},{"id":"q1","type":"choice","instructions":"two","criteria":{"a":"b"}}]`, wantErr: `questions[1]: duplicate question id "q1"`},

		// trailing data after the array.
		{name: "trailing data", data: `[] {}`, wantErr: "questions must be a JSON array of question objects"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseQuestions([]byte(tc.data))
			if err == nil {
				t.Fatalf("ParseQuestions(%s) err = nil; want containing %q", tc.data, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseQuestions(%s) err = %q; want containing %q", tc.data, err.Error(), tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), errPrefix) {
				t.Fatalf("ParseQuestions(%s) err = %q; want %q prefix", tc.data, err.Error(), errPrefix)
			}
		})
	}
}

// TestParseQuestionsContext pins the positional error context.
func TestParseQuestionsContext(t *testing.T) {
	t.Parallel()

	_, err := ParseQuestions([]byte(`[{"id":"ok","type":"noul","instructions":"g"},{"id":"bad key","type":"noul","instructions":"g"}]`))
	if err == nil {
		t.Fatal("ParseQuestions() err = nil; want reject")
	}
	if !strings.Contains(err.Error(), `questions[1]: id must be a bareword`) {
		t.Fatalf("ParseQuestions() err = %q; want questions[1] context", err.Error())
	}
}

// TestQuestionUnmarshalJSON pins that the Question decode path is exactly
// as strict as ParseQuestions, including unknown-key rejection.
func TestQuestionUnmarshalJSON(t *testing.T) {
	t.Parallel()

	t.Run("round-trips a valid question", func(t *testing.T) {
		t.Parallel()
		var q Question
		data := `{"id":"q1","type":"score","instructions":"g","criteria":["low","high"]}`
		if err := json.Unmarshal([]byte(data), &q); err != nil {
			t.Fatalf("Unmarshal(%s) err = %v; want nil", data, err)
		}
		if q.ID != "q1" || q.Type != TypeScore || len(q.Levels) != 2 {
			t.Fatalf("Unmarshal(%s) = %+v; want score question with levels", data, q)
		}
	})

	t.Run("rejects unknown keys", func(t *testing.T) {
		t.Parallel()
		var q Question
		err := json.Unmarshal([]byte(`{"id":"q1","type":"noul","instructions":"g","why":"because"}`), &q)
		if err == nil {
			t.Fatal("Unmarshal() err = nil; want unknown-field reject")
		}
		if !strings.Contains(err.Error(), `unknown field "why"`) {
			t.Fatalf("Unmarshal() err = %q; want unknown field \"why\"", err.Error())
		}
	})

	t.Run("rejects wrong-shaped criteria", func(t *testing.T) {
		t.Parallel()
		var q Question
		err := json.Unmarshal([]byte(`{"id":"q1","type":"score","instructions":"g","criteria":{"low":1}}`), &q)
		if err == nil || !strings.Contains(err.Error(), "not an object") {
			t.Fatalf("Unmarshal() err = %v; want map-shape reject", err)
		}
	})
}

// TestQuestionMarshalJSON pins the wire rendering: field order
// {id, type, instructions, criteria} and criteria omitted when the type
// supplies none. It also proves hand-built incoherent questions cannot
// reach the wire.
func TestQuestionMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		q       Question
		want    string
		wantErr string
	}{
		{name: "choice", q: Question{ID: "q1", Type: TypeChoice, Instructions: "g", Options: map[string]string{"a": "b"}}, want: `{"id":"q1","type":"choice","instructions":"g","criteria":{"a":"b"}}`},
		{name: "score", q: Question{ID: "q1", Type: TypeScore, Instructions: "g", Levels: []string{"low", "high"}}, want: `{"id":"q1","type":"score","instructions":"g","criteria":["low","high"]}`},
		{name: "noul without criteria omits the key", q: Question{ID: "q1", Type: TypeNoul, Instructions: "g"}, want: `{"id":"q1","type":"noul","instructions":"g"}`},
		{name: "noul with gloss", q: Question{ID: "q1", Type: TypeNoul, Instructions: "g", Gloss: "yes/no"}, want: `{"id":"q1","type":"noul","instructions":"g","criteria":"yes/no"}`},
		{name: "choice without options rejected", q: Question{ID: "q1", Type: TypeChoice, Instructions: "g"}, wantErr: "requires criteria"},
		{name: "choice carrying score levels rejected", q: Question{ID: "q1", Type: TypeChoice, Instructions: "g", Options: map[string]string{"a": "b"}, Levels: []string{"low"}}, wantErr: "must not carry score criteria"},
		{name: "score carrying choice options rejected", q: Question{ID: "q1", Type: TypeScore, Instructions: "g", Levels: []string{"low"}, Options: map[string]string{"a": "b"}}, wantErr: "must not carry choice options"},
		{name: "noul carrying choice options rejected", q: Question{ID: "q1", Type: TypeNoul, Instructions: "g", Options: map[string]string{"a": "b"}}, wantErr: "must not carry choice options"},
		{name: "unknown type rejected", q: Question{ID: "q1", Type: QuestionType("emoji"), Instructions: "g"}, wantErr: "type must be"},
		{name: "empty instructions rejected", q: Question{ID: "q1", Type: TypeNoul}, wantErr: "instructions is required"},
		{name: "non-bareword id rejected", q: Question{ID: "q 1", Type: TypeNoul, Instructions: "g"}, wantErr: "bareword"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.q)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Marshal(%+v) err = nil; want containing %q", tc.q, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Marshal(%+v) err = %q; want containing %q", tc.q, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Marshal(%+v) err = %v; want nil", tc.q, err)
			}
			if string(raw) != tc.want {
				t.Fatalf("Marshal(%+v) = %s; want %s", tc.q, raw, tc.want)
			}
		})
	}
}

// TestQuestionMarshalUnmarshalRoundTrip pins lossless round-trips for the
// three question types: re-marshaling produces the same canonical wire
// array (object keys render sorted, so the source is written canonical).
func TestQuestionMarshalUnmarshalRoundTrip(t *testing.T) {
	t.Parallel()

	data := `[{"id":"a","type":"choice","instructions":"g","criteria":{"no":"n","yes":"y"}},` +
		`{"id":"b","type":"score","instructions":"g","criteria":["low","high"]},` +
		`{"id":"c","type":"noul","instructions":"g"}]`
	qs, err := ParseQuestions([]byte(data))
	if err != nil {
		t.Fatalf("ParseQuestions() err = %v; want nil", err)
	}
	again, err := json.Marshal(qs)
	if err != nil {
		t.Fatalf("Marshal() err = %v; want nil", err)
	}
	if string(again) != data {
		t.Fatalf("Marshal() = %s; want identical source array %s", again, data)
	}
}

// TestIsBareword pins the bareword boundary.
func TestIsBareword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{in: "", want: false},
		{in: "a", want: true},
		{in: "_a", want: true},
		{in: "A_b2", want: true},
		{in: "2a", want: false},
		{in: "a-2", want: false},
		{in: "a 2", want: false},
		{in: "aé", want: false},
		{in: "a.2", want: false},
	}
	for _, tc := range tests {
		if got := IsBareword(tc.in); got != tc.want {
			t.Errorf("IsBareword(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseStateValidShapes covers the accepted state kinds, including
// lossless number-lexeme preservation.
func TestParseStateValidShapes(t *testing.T) {
	t.Parallel()

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		state, err := ParseState([]byte(`"ready"`))
		if err != nil || state != "ready" {
			t.Fatalf("ParseState() = %#v, %v; want \"ready\", nil", state, err)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		state, err := ParseState([]byte(`""`))
		if err != nil || state != "" {
			t.Fatalf("ParseState() = %#v, %v; want \"\", nil", state, err)
		}
	})

	t.Run("object with number lexeme preserved", func(t *testing.T) {
		t.Parallel()
		state, err := ParseState([]byte(`{"reps":1.50,"flag":true}`))
		if err != nil {
			t.Fatalf("ParseState() err = %v; want nil", err)
		}
		obj, ok := state.(map[string]any)
		if !ok {
			t.Fatalf("ParseState() = %#v; want object", state)
		}
		reps, ok := obj["reps"].(json.Number)
		if !ok || reps.String() != "1.50" {
			t.Fatalf("ParseState() reps = %#v; want json.Number \"1.50\"", obj["reps"])
		}
	})

	t.Run("array", func(t *testing.T) {
		t.Parallel()
		state, err := ParseState([]byte(`["a", 2]`))
		if err != nil {
			t.Fatalf("ParseState() err = %v; want nil", err)
		}
		arr, ok := state.([]any)
		if !ok || len(arr) != 2 {
			t.Fatalf("ParseState() = %#v; want 2-element array", state)
		}
		if n, ok := arr[1].(json.Number); !ok || n.String() != "2" {
			t.Fatalf("ParseState() arr[1] = %#v; want json.Number \"2\"", arr[1])
		}
	})

	t.Run("empty object and array", func(t *testing.T) {
		t.Parallel()
		for _, data := range []string{`{}`, `[]`} {
			if _, err := ParseState([]byte(data)); err != nil {
				t.Errorf("ParseState(%s) err = %v; want nil", data, err)
			}
		}
	})
}

// TestParseStateRejectMatrix is the fail-closed state matrix.
func TestParseStateRejectMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{name: "empty input", data: ``, wantErr: "state is required"},
		{name: "whitespace only", data: `   `, wantErr: "state is required"},
		{name: "null", data: `null`, wantErr: "state must be a JSON string, object, or array; got <nil>"},
		{name: "number", data: `3`, wantErr: "state must be a JSON string, object, or array; got json.Number"},
		{name: "boolean", data: `true`, wantErr: "state must be a JSON string, object, or array; got bool"},
		{name: "malformed", data: `{"a":`, wantErr: "invalid state JSON"},
		{name: "trailing data", data: `{} {}`, wantErr: "trailing data after the state JSON document"},
		{name: "fragment", data: `str`, wantErr: "invalid state JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseState([]byte(tc.data))
			if err == nil {
				t.Fatalf("ParseState(%s) err = nil; want containing %q", tc.data, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseState(%s) err = %q; want containing %q", tc.data, err.Error(), tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), errPrefix) {
				t.Fatalf("ParseState(%s) err = %q; want %q prefix", tc.data, err.Error(), errPrefix)
			}
		})
	}
}

// TestValidateRequestRejectMatrix pins the typed-value pre-flight used by
// Decision: same defects as the JSON path, plus coverage and duplicates.
func TestValidateRequestRejectMatrix(t *testing.T) {
	t.Parallel()

	good := testQuestions()
	tests := []struct {
		name    string
		state   State
		input   []Question
		wantErr string
	}{
		{name: "nil state", state: nil, input: good, wantErr: "state must be a JSON string, object, or array"},
		{name: "int state", state: 3, input: good, wantErr: "state must be a JSON string, object, or array"},
		{name: "float state", state: 3.5, input: good, wantErr: "state must be a JSON string, object, or array"},
		{name: "bool state", state: false, input: good, wantErr: "state must be a JSON string, object, or array"},
		{name: "nil questions", state: "ready", input: nil, wantErr: "at least one question"},
		{name: "empty questions", state: "ready", input: []Question{}, wantErr: "at least one question"},
		{name: "no id", state: "ready", input: []Question{{Type: TypeNoul, Instructions: "g"}}, wantErr: "bareword"},
		{name: "bad type", state: "ready", input: []Question{{ID: "q1", Type: QuestionType("grade"), Instructions: "g"}}, wantErr: "type must be"},
		{name: "empty instructions", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul}}, wantErr: "instructions is required"},
		{name: "choice without options", state: "ready", input: []Question{{ID: "q1", Type: TypeChoice, Instructions: "g"}}, wantErr: "choice question"},
		{name: "score without levels", state: "ready", input: []Question{{ID: "q1", Type: TypeScore, Instructions: "g"}}, wantErr: "score question"},
		{name: "noul with options", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: "g", Options: map[string]string{"a": "b"}}}, wantErr: "noul question"},
		{name: "choice with gloss", state: "ready", input: []Question{{ID: "q1", Type: TypeChoice, Instructions: "g", Options: map[string]string{"a": "b"}, Gloss: "x"}}, wantErr: "choice question"},
		{name: "duplicate ids", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: "a"}, {ID: "q1", Type: TypeNoul, Instructions: "b"}}, wantErr: `questions[1]: duplicate question id "q1"`},
		{name: "duplicate ids at third position", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: "a"}, {ID: "q2", Type: TypeNoul, Instructions: "b"}, {ID: "q1", Type: TypeNoul, Instructions: "c"}}, wantErr: `questions[2]: duplicate question id "q1"`},
		{name: "positional context", state: "ready", input: []Question{{ID: "q1", Type: TypeNoul, Instructions: "a"}, {ID: "q9", Type: QuestionType("grade"), Instructions: "b"}}, wantErr: "questions[1]:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRequest(tc.state, tc.input)
			if err == nil {
				t.Fatalf("validateRequest(%v, %+v) err = nil; want containing %q", tc.state, tc.input, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateRequest(%v, %+v) err = %q; want containing %q", tc.state, tc.input, err.Error(), tc.wantErr)
			}
		})
	}

	t.Run("valid request passes", func(t *testing.T) {
		t.Parallel()
		if err := validateRequest(map[string]any{"branch": "main"}, testQuestions()); err != nil {
			t.Fatalf("validateRequest() err = %v; want nil", err)
		}
	})
}
