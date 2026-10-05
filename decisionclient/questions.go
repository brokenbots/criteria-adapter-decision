package decisionclient

// Question/state request model (ADR-0013 wire contract, M2). Questions
// arrive as JSON and are decoded and validated FAIL-CLOSED before any HTTP
// traffic: unknown keys, wrong shapes, and missing requirements are errors,
// never defaults or silent drops.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// QuestionType enumerates the System One question kinds carried in the
// decision request.
type QuestionType string

const (
	// TypeChoice asks the backend to pick exactly one of the question's
	// options (a non-empty option map is required criteria).
	TypeChoice QuestionType = "choice"
	// TypeScore asks the backend to grade the state against an ordered level
	// list (a non-empty ordered level list is required criteria).
	TypeScore QuestionType = "score"
	// TypeNoul asks the backend for a yes/no verdict; the yes/no gloss in
	// criteria is optional.
	TypeNoul QuestionType = "noul"
)

// State is the decision state carried on the wire. It may be a JSON string,
// object, or array — the parsed Go forms are string, map[string]any, and
// []any (numbers preserve their lexeme via json.Number). Anything else is
// rejected fail-closed.
type State any

// Question is one System One decision question in its validated form. ID,
// Type, and Instructions are always required; the criteria payload is
// type-specific and validated strictly:
//
//   - choice: Options — non-empty map of option name → description.
//   - score:  Levels — non-empty, ordered list of unique level names; the
//     decoded answer's score is the zero-based index into this list.
//   - noul:   Gloss — optional yes/no gloss; "" means no criteria.
//
// Only the field matching Type may be set; Validate and both JSON paths
// reject any other combination.
type Question struct {
	ID           string
	Type         QuestionType
	Instructions string

	Options map[string]string // choice criteria
	Levels  []string          // score criteria
	Gloss   string            // noul criteria ("" = absent)
}

// questionTypes are the accepted Type values, reported in error messages.
var questionTypes = []QuestionType{TypeChoice, TypeScore, TypeNoul}

// validQuestionType reports whether t is one of the defined types and, for
// error messages, returns the quoted alternatives.
func validQuestionType(t QuestionType) bool {
	for _, want := range questionTypes {
		if t == want {
			return true
		}
	}
	return false
}

// rawQuestion is the strict-decode view of a question object on the wire.
// Pointer fields distinguish "key absent" from "present but empty" so every
// rejection names the actual defect; Criteria stays raw until the question
// type decides how to decode it.
type rawQuestion struct {
	ID           *string         `json:"id"`
	Type         *string         `json:"type"`
	Instructions *string         `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

// strictUnmarshal decodes exactly one JSON document into v, rejecting
// unknown fields and trailing data. This is the v0.6.0 config hardline
// (strict object discipline) applied at every object boundary this package
// validates.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

// isBareword reports whether s is a bareword: non-empty ASCII letters,
// digits, or underscores, not starting with a digit.
func isBareword(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// jsonNull reports whether raw is the JSON null literal.
func jsonNull(raw json.RawMessage) bool {
	return len(raw) != 0 && string(bytes.TrimSpace(raw)) == "null"
}

// ParseQuestions decodes questions JSON (a non-empty array of question
// objects) with strict object discipline: unknown keys anywhere inside a
// question object are errors, criteria must match the question type, and
// every validation rule (bareword ids, known types, non-empty instructions,
// required criteria shapes) is enforced fail-closed. It never defaults or
// drops anything: the first defect is the error.
func ParseQuestions(data []byte) ([]Question, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return nil, fmt.Errorf(errPrefix+"questions must be a JSON array of question objects: %w", err)
	}
	if len(elems) == 0 {
		return nil, errors.New(errPrefix + "questions must contain at least one question")
	}
	qs := make([]Question, 0, len(elems))
	seen := make(map[string]bool, len(elems))
	for i, raw := range elems {
		q, err := parseQuestion(raw)
		if err != nil {
			return nil, fmt.Errorf(errPrefix+"questions[%d]: %w", i, err)
		}
		if seen[q.ID] {
			return nil, fmt.Errorf(errPrefix+"questions[%d]: duplicate question id %q", i, q.ID)
		}
		seen[q.ID] = true
		qs = append(qs, q)
	}
	return qs, nil
}

// parseQuestion decodes and validates ONE question object (see Question for
// the rules). It is the single decode path used by ParseQuestions and by
// Question.UnmarshalJSON, so both are equally strict.
func parseQuestion(data []byte) (Question, error) {
	var raw rawQuestion
	if err := strictUnmarshal(data, &raw); err != nil {
		return Question{}, fmt.Errorf("decode question: %w", err)
	}
	if raw.ID == nil {
		return Question{}, errors.New("id is required")
	}
	if !isBareword(*raw.ID) {
		return Question{}, fmt.Errorf("id must be a bareword (ASCII letters, digits, or underscores, not starting with a digit); got %q", *raw.ID)
	}
	if raw.Type == nil {
		return Question{}, errors.New("type is required")
	}
	qtype := QuestionType(*raw.Type)
	if !validQuestionType(qtype) {
		return Question{}, fmt.Errorf("type must be %s; got %q", quotedQuestionTypes(), *raw.Type)
	}
	if raw.Instructions == nil || *raw.Instructions == "" {
		return Question{}, errors.New("instructions is required and must be non-empty")
	}

	q := Question{ID: *raw.ID, Type: qtype, Instructions: *raw.Instructions}
	var err error
	switch qtype {
	case TypeChoice:
		q.Options, err = parseChoiceCriteria(raw.Criteria)
	case TypeScore:
		q.Levels, err = parseScoreCriteria(raw.Criteria)
	case TypeNoul:
		q.Gloss, err = parseNoulCriteria(raw.Criteria)
	}
	if err != nil {
		return Question{}, fmt.Errorf("type %q: %w", qtype, err)
	}
	return q, nil
}

// parseChoiceCriteria decodes the required choice criteria: a non-empty map
// of option name → description. JSON null counts as absent.
func parseChoiceCriteria(raw json.RawMessage) (map[string]string, error) {
	const want = "choice questions require criteria: a non-empty option map (option name → description)"
	if len(raw) == 0 || jsonNull(raw) {
		return nil, errors.New(want)
	}
	var options map[string]string
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, fmt.Errorf(want+": %w", err)
	}
	if len(options) == 0 {
		return nil, errors.New(want)
	}
	for name := range options {
		if name == "" {
			return nil, errors.New("choice criteria option names must be non-empty")
		}
	}
	return options, nil
}

// parseScoreCriteria decodes the required score criteria: a non-empty,
// ordered list of unique level names. JSON null counts as absent.
func parseScoreCriteria(raw json.RawMessage) ([]string, error) {
	const want = "score questions require criteria: a non-empty ordered level list (a JSON array of level names)"
	if len(raw) == 0 || jsonNull(raw) {
		return nil, errors.New(want)
	}
	var levels []string
	if err := json.Unmarshal(raw, &levels); err != nil {
		// Name the common wrong shape directly: {"low": 1, "high": 2}.
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			return nil, errors.New("score criteria must be an ordered level list (a JSON array of level names), not an object")
		}
		return nil, fmt.Errorf(want+": %w", err)
	}
	if len(levels) == 0 {
		return nil, errors.New(want)
	}
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		if level == "" {
			return nil, errors.New("score criteria level names must be non-empty")
		}
		if seen[level] {
			return nil, fmt.Errorf("score criteria levels must be unique; %q appears more than once", level)
		}
		seen[level] = true
	}
	return levels, nil
}

// parseNoulCriteria decodes the optional noul criteria: a non-empty yes/no
// gloss string. Absent key and JSON null both mean "no criteria".
func parseNoulCriteria(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || jsonNull(raw) {
		return "", nil
	}
	var gloss string
	if err := json.Unmarshal(raw, &gloss); err != nil {
		return "", errors.New("noul criteria, when present, must be a yes/no gloss string")
	}
	if gloss == "" {
		return "", errors.New("noul criteria, when present, must be a non-empty yes/no gloss string")
	}
	return gloss, nil
}

// quotedQuestionTypes renders the accepted types for error messages.
func quotedQuestionTypes() string {
	quoted := make([]byte, 0, 32)
	for i, t := range questionTypes {
		if i > 0 {
			quoted = append(quoted, ", "...)
		}
		quoted = append(quoted, '"')
		quoted = append(quoted, t...)
		quoted = append(quoted, '"')
	}
	return string(quoted)
}

// MarshalJSON renders the question exactly as the wire contract defines it:
// {id, type, instructions, criteria}, with criteria present only when the
// type requires or supplies it. It validates first, so hand-built questions
// that would not pass parseQuestion cannot reach the wire either.
func (q Question) MarshalJSON() ([]byte, error) {
	if err := validateQuestion(q); err != nil {
		return nil, fmt.Errorf("%s%s", errPrefix, err)
	}
	wire := struct {
		ID           string       `json:"id"`
		Type         QuestionType `json:"type"`
		Instructions string       `json:"instructions"`
		Criteria     any          `json:"criteria,omitempty"`
	}{ID: q.ID, Type: q.Type, Instructions: q.Instructions}
	switch q.Type {
	case TypeChoice:
		wire.Criteria = q.Options
	case TypeScore:
		wire.Criteria = q.Levels
	case TypeNoul:
		if q.Gloss != "" {
			wire.Criteria = q.Gloss
		}
	}
	return json.Marshal(wire)
}

// UnmarshalJSON decodes a question object with the same strict rules as
// ParseQuestions, so Question round-trips are validated in both directions.
func (q *Question) UnmarshalJSON(data []byte) error {
	parsed, err := parseQuestion(data)
	if err != nil {
		return fmt.Errorf("%s%s", errPrefix, err)
	}
	*q = parsed
	return nil
}

// validateQuestion checks one question's internal coherence: bareword id,
// known type, non-empty instructions, and a criteria payload matching the
// type (and no stray payload from another type).
func validateQuestion(q Question) error {
	if !isBareword(q.ID) {
		return fmt.Errorf("question id must be a bareword (ASCII letters, digits, or underscores, not starting with a digit); got %q", q.ID)
	}
	if !validQuestionType(q.Type) {
		return fmt.Errorf("type must be %s; got %q", quotedQuestionTypes(), q.Type)
	}
	if q.Instructions == "" {
		return fmt.Errorf("question %q: instructions is required and must be non-empty", q.ID)
	}
	switch q.Type {
	case TypeChoice:
		if q.Levels != nil || q.Gloss != "" {
			return fmt.Errorf("choice question %q must not carry score criteria or a noul gloss", q.ID)
		}
		if len(q.Options) == 0 {
			return fmt.Errorf("choice question %q requires criteria: a non-empty option map", q.ID)
		}
	case TypeScore:
		if q.Options != nil || q.Gloss != "" {
			return fmt.Errorf("score question %q must not carry choice options or a noul gloss", q.ID)
		}
		if len(q.Levels) == 0 {
			return fmt.Errorf("score question %q requires criteria: a non-empty ordered level list", q.ID)
		}
	case TypeNoul:
		if q.Options != nil || q.Levels != nil {
			return fmt.Errorf("noul question %q must not carry choice options or score levels", q.ID)
		}
	}
	return nil
}

// ParseState decodes the decision state from JSON text. The state may be a
// JSON string, object, or array; numbers, booleans, null, and anything
// malformed are rejected fail-closed. Objects and arrays keep their number
// lexemes (json.Number), so forwarding the decoded value back to the wire
// loses nothing.
func ParseState(data []byte) (State, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New(errPrefix + "state is required: JSON text of a string, object, or array")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf(errPrefix+"invalid state JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New(errPrefix + "trailing data after the state JSON document")
	}
	if err := validateState(v); err != nil {
		return nil, err
	}
	return v, nil
}

// validateState checks the parsed state kind: string, object, or array.
func validateState(state State) error {
	switch state.(type) {
	case string, map[string]any, []any:
		return nil
	default:
		return fmt.Errorf(errPrefix+"state must be a JSON string, object, or array; got %T", state)
	}
}

// validateRequest is the pre-flight check that Decision runs before any HTTP
// traffic: the state kind and every question must pass strict validation,
// with no duplicate ids.
func validateRequest(state State, questions []Question) error {
	if err := validateState(state); err != nil {
		return err
	}
	if len(questions) == 0 {
		return errors.New(errPrefix + "questions must contain at least one question")
	}
	seen := make(map[string]bool, len(questions))
	for i, q := range questions {
		if err := validateQuestion(q); err != nil {
			return fmt.Errorf(errPrefix+"questions[%d]: %w", i, err)
		}
		if seen[q.ID] {
			return fmt.Errorf(errPrefix+"questions[%d]: duplicate question id %q", i, q.ID)
		}
		seen[q.ID] = true
	}
	return nil
}
