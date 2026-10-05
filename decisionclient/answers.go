package decisionclient

// Answer decode (ADR-0013 wire contract, M2). Upstream answers are decoded
// losslessly and mapped verbatim onto the validated questions; every decode
// or mapping defect is an Execute error, never a silent drop.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Answer is the validated, lossless form of one upstream answer for the
// question of the same ID:
//
//   - choice: Choice is the selected option name; Probabilities maps known
//     options to their probabilities; Confidence is in [0, 1].
//   - score:  Score is the zero-based level index the answer resolves to and
//     Legend the matching level name; Probabilities maps known levels (by
//     name or by zero-based level index) to their probabilities; Confidence
//     is in [0, 1]. The wire score is a probability-weighted number that can
//     land between levels (e.g. 1.1394); the decoder resolves it to the
//     nearest level, rounding halves away from zero.
//   - noul:   Noul is the verdict "yes" or "no" (no probabilities or
//     confidence). The backend may report the verdict either as the
//     documented "yes"/"no" string or, as the local Ollama backend does
//     live, as a probability-like number in [0, 1]: values at or above 0.5
//     read as "yes", below as "no".
//
// Exactly the fields matching the question type are populated; the others
// remain zero values.
type Answer struct {
	QuestionID    string
	Choice        string
	Score         int
	Legend        string
	Noul          string
	Probabilities map[string]float64
	Confidence    float64
}

// answersEnvelope is the strict-decode view of the upstream answers-only
// document: the wire contract has exactly one top-level key, so unknown keys
// fail. answers is a JSON map of answer objects keyed by question id, kept
// raw until the per-answer strict decode.
type answersEnvelope struct {
	Answers *json.RawMessage `json:"answers"`
}

// rawAnswer is the strict-decode view of one answer object in the answers
// map. The map key is the question id, so no id field is carried inside the
// object; instead every object states its question type, and the decoder
// rejects answers whose type does not match the question's. Pointer fields
// distinguish "key absent" from "present but invalid", which the cross-type
// and presence checks depend on. Score and Legend stay raw: the live backend
// reports the score as a number lexeme (integral or probability-weighted
// float) and the legend either as a level-name string or as a level
// index → name map, and all shapes are validated in resolveScore/resolveLegend.
type rawAnswer struct {
	Type          *QuestionType       `json:"type"`
	Choice        *string             `json:"choice"`
	Score         *json.RawMessage    `json:"score"`
	Legend        *json.RawMessage    `json:"legend"`
	Noul          *json.RawMessage    `json:"noul"`
	Probabilities *map[string]float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

// DecodeAnswers maps the upstream answers JSON onto answers for the given
// questions, aligned to question order. Decoding is strict and lossless:
//
//   - the envelope carries exactly the "answers" key;
//   - answers is a JSON map of answer objects keyed by question id (map keys
//     are unique, so each question id appears exactly once);
//   - every answer object states the type of its question and carries no
//     unknown keys and no fields belonging to another question type;
//   - each payload field is present, well-formed, and range-checked;
//   - coverage is exact — one answer per question, with answers to unknown
//     questions rejected.
//
// All defects are errors; nothing is dropped or defaulted, so malformed
// upstream answers fail the step instead of passing through silently.
func DecodeAnswers(raw []byte, questions []Question) ([]Answer, error) {
	if len(questions) == 0 {
		return nil, errors.New(errPrefix + "decode answers requires the validated questions")
	}
	var env answersEnvelope
	if err := strictUnmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf(errPrefix+"decode answers: %w", err)
	}
	if env.Answers == nil || jsonNull(*env.Answers) {
		return nil, errors.New(errPrefix + `decode answers: the "answers" map keyed by question id is required`)
	}

	var entries map[string]json.RawMessage
	if err := strictUnmarshal(*env.Answers, &entries); err != nil {
		return nil, fmt.Errorf(errPrefix+"decode answers: answers must be a JSON map of answer objects keyed by question id: %w", err)
	}
	return decodeAnswerEntries(entries, questions)
}

// decodeAnswerEntries maps raw answer objects (keyed by question id) onto
// the validated questions, aligned to question order with exact coverage. It
// is the shared mapping core of [DecodeAnswers] (answers-only envelope) and
// [DecodeDecisionResponse] (full {model, answers, usage} response envelope).
//
// Error order is deterministic: answer keys are validated in sorted order.
func decodeAnswerEntries(entries map[string]json.RawMessage, questions []Question) ([]Answer, error) {
	byID := make(map[string]int, len(questions))
	for i, q := range questions {
		if _, dup := byID[q.ID]; dup {
			return nil, fmt.Errorf(errPrefix+"decode answers: duplicate question id %q", q.ID)
		}
		byID[q.ID] = i
	}

	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	answers := make([]Answer, len(questions))
	answered := make(map[string]bool, len(questions))
	for _, key := range keys {
		var rawA rawAnswer
		if err := strictUnmarshal(entries[key], &rawA); err != nil {
			return nil, fmt.Errorf(errPrefix+"answers[%q]: decode answer: %w", key, err)
		}
		idx, ok := byID[key]
		if !ok {
			return nil, fmt.Errorf(errPrefix+"answers[%q]: answer for unknown question", key)
		}
		a, err := checkRawAnswer(&rawA, questions[idx])
		if err != nil {
			return nil, fmt.Errorf(errPrefix+"answers[%q]: %w", key, err)
		}
		answers[idx] = a
		answered[key] = true
	}
	for _, q := range questions {
		if !answered[q.ID] {
			return nil, fmt.Errorf(errPrefix+"decode answers: no answer for question %q", q.ID)
		}
	}
	return answers, nil
}

// checkRawAnswer validates the decoded answer against its question and
// returns the typed form. The answer must state the question's type; the
// presence matrix is otherwise exact: the payload fields for the question
// type are required and every other type's fields are cross-type errors.
func checkRawAnswer(raw *rawAnswer, q Question) (Answer, error) {
	a := Answer{QuestionID: q.ID}
	if raw.Type == nil {
		return Answer{}, errors.New(`answer "type" is required`)
	}
	if *raw.Type != q.Type {
		return Answer{}, fmt.Errorf("answer type %q does not match the question's type %q", *raw.Type, q.Type)
	}
	switch q.Type {
	case TypeChoice:
		if len(q.Options) == 0 {
			return Answer{}, fmt.Errorf("question %q has no choice options", q.ID)
		}
		if err := requireAbsent(raw, q, "score", "legend", "noul"); err != nil {
			return Answer{}, err
		}
		if raw.Choice == nil || *raw.Choice == "" {
			return Answer{}, errors.New(`choice answer must carry a non-empty "choice" field`)
		}
		if _, ok := q.Options[*raw.Choice]; !ok {
			return Answer{}, fmt.Errorf("choice answer selected %q, which is not one of the question's options", *raw.Choice)
		}
		a.Choice = *raw.Choice

	case TypeScore:
		if len(q.Levels) == 0 {
			return Answer{}, fmt.Errorf("question %q has no score levels", q.ID)
		}
		if err := requireAbsent(raw, q, "choice", "noul"); err != nil {
			return Answer{}, err
		}
		if raw.Score == nil {
			return Answer{}, errors.New(`score answer must carry a "score" field`)
		}
		var legend json.RawMessage
		if raw.Legend != nil {
			legend = *raw.Legend
		}
		idx, name, err := resolveScore(*raw.Score, legend, q)
		if err != nil {
			return Answer{}, err
		}
		a.Score = idx
		a.Legend = name

	case TypeNoul:
		if err := requireAbsent(raw, q, "choice", "score", "legend", "probabilities", "confidence"); err != nil {
			return Answer{}, err
		}
		if raw.Noul == nil {
			return Answer{}, errors.New(`noul answer must carry a "noul" field`)
		}
		noul, err := resolveNoul(*raw.Noul)
		if err != nil {
			return Answer{}, err
		}
		a.Noul = noul
	}

	if q.Type != TypeNoul {
		confidence, err := checkConfidence(raw.Confidence, q)
		if err != nil {
			return Answer{}, err
		}
		probabilities, err := checkProbabilities(raw.Probabilities, q)
		if err != nil {
			return Answer{}, err
		}
		a.Confidence = confidence
		a.Probabilities = probabilities
	}
	return a, nil
}

// resolveScore decodes a score answer's score + legend pair onto the
// question's levels, returning the resolved zero-based level index and its
// name. The score must be a JSON number lexeme; the live System One score is
// a probability-weighted number that can land between levels (e.g. 1.1394):
// the score resolves to the nearest level index, rounding halves away from
// zero. The legend — the level naming the backend reports — is accepted in
// both live shapes and is validated against the question's levels with
// [resolveLegend]. Negative scores and scores that resolve past the last
// level are decode defects.
func resolveScore(score json.RawMessage, legend json.RawMessage, q Question) (int, string, error) {
	if len(score) == 0 || jsonNull(score) {
		return 0, "", errors.New(`score answer must carry a "score" field`)
	}
	trimmed := bytes.TrimSpace(score)
	if trimmed[0] == '"' {
		return 0, "", errors.New("score must be a JSON number")
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return 0, "", errors.New("score must be a JSON number")
	}
	value, err := number.Float64()
	if err != nil {
		return 0, "", fmt.Errorf("score %q is not a number: %w", number.String(), err)
	}
	if value < 0 {
		return 0, "", fmt.Errorf("score %s is outside the question's %d levels (negative)", number.String(), len(q.Levels))
	}
	idx := int(math.Round(value))
	if idx < 0 || idx >= len(q.Levels) {
		return 0, "", fmt.Errorf("score %s is outside the question's %d levels", number.String(), len(q.Levels))
	}
	name, err := resolveLegend(legend, q, idx)
	if err != nil {
		return 0, "", err
	}
	return idx, name, nil
}

// resolveLegend decodes the score answer's legend onto the question's
// levels. Both live System One shapes are accepted:
//
//   - a string naming the score's resolved level ({"legend":"mid"}); or
//   - an object mapping every level index → its name
//     ({"legend":{"0":"low","1":"med","2":"high"}}), with names matching the
//     question's levels exactly.
//
// The backend reports index-keyed legends for probability-weighted scores
// that land between levels; decoding validates the full naming map so a
// legend that drifts from the question's levels cannot reach the graph.
func resolveLegend(raw json.RawMessage, q Question, idx int) (string, error) {
	if len(raw) == 0 || jsonNull(raw) {
		return "", errors.New(`score answer must carry a non-empty "legend" field`)
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] == '"' {
		var name string
		if err := json.Unmarshal(trimmed, &name); err != nil {
			return "", errors.New(scoreLegendShapeError)
		}
		if name == "" {
			return "", errors.New(`score answer must carry a non-empty "legend" field`)
		}
		if name != q.Levels[idx] {
			return "", fmt.Errorf("score answer legend %q does not match level %d (%q)", name, idx, q.Levels[idx])
		}
		return name, nil
	}
	if trimmed[0] != '{' {
		return "", errors.New(scoreLegendShapeError)
	}
	var legend map[string]string
	if err := json.Unmarshal(trimmed, &legend); err != nil {
		return "", fmt.Errorf("score answer legend must map level index → level name: %w", err)
	}
	seen := make(map[int]bool, len(legend))
	for key, name := range legend {
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(q.Levels) {
			return "", fmt.Errorf("score answer legend key %q is not a level index in [0, %d)", key, len(q.Levels))
		}
		if seen[i] {
			return "", fmt.Errorf("score answer legend repeats level index %d", i)
		}
		seen[i] = true
		if name != q.Levels[i] {
			return "", fmt.Errorf("score answer legend names level %d as %q, want %q", i, name, q.Levels[i])
		}
	}
	if len(seen) != len(q.Levels) {
		for i, level := range q.Levels {
			if !seen[i] {
				return "", fmt.Errorf("score answer legend is missing level %d (%q)", i, level)
			}
		}
	}
	return q.Levels[idx], nil
}

// scoreLegendShapeError is the shape message for a legend that is neither a
// level-name string nor a level index → name map.
const scoreLegendShapeError = `score answer legend must be a level name string or a level index → name map`

// resolveNoul decodes a noul answer's verdict onto "yes" or "no". Both live
// shapes are accepted:
//
//   - the documented verdict string, strictly "yes" or "no"; or
//   - the probability-like number the local Ollama backend reports live
//     ({"noul": 0.78}): a value in [0, 1] that reads "yes" at 0.5 or above
//     and "no" below, mirroring the score decoder's nearest-level rule.
//
// Any other JSON type — booleans, objects, arrays — is a decode defect, as
// is every string other than exactly "yes" or "no".
func resolveNoul(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New(`noul answer must carry a "noul" field`)
	}
	if trimmed[0] == '"' {
		var verdict string
		if err := json.Unmarshal(trimmed, &verdict); err != nil {
			return "", errors.New(`noul answer must be the string "yes", the string "no", or a probability-like number in [0, 1]`)
		}
		if verdict != "yes" && verdict != "no" {
			return "", fmt.Errorf("noul answer must be exactly %q or %q; got %q", "yes", "no", verdict)
		}
		return verdict, nil
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return "", errors.New(`noul answer must be the string "yes", the string "no", or a probability-like number in [0, 1]`)
	}
	value, err := number.Float64()
	if err != nil {
		return "", fmt.Errorf("noul answer %s is not a probability-like number in [0, 1]: %w", number.String(), err)
	}
	if value < 0 || value > 1 {
		return "", fmt.Errorf("noul answer %s is outside [0, 1]", number.String())
	}
	if value >= 0.5 {
		return "yes", nil
	}
	return "no", nil
}

// requireAbsent rejects cross-type fields with a message naming the
// question type and the offending field. Raw-message fields (score, legend,
// noul) count a JSON null as absent, matching the package-wide "null counts as
// absent" convention. The "type" field needs no case: every answer carries
// it and it is checked against the question's type before this helper runs.
func requireAbsent(raw *rawAnswer, q Question, fields ...string) error {
	for _, field := range fields {
		var present bool
		switch field {
		case "choice":
			present = raw.Choice != nil
		case "score":
			present = raw.Score != nil && !jsonNull(*raw.Score)
		case "legend":
			present = raw.Legend != nil && !jsonNull(*raw.Legend)
		case "noul":
			present = raw.Noul != nil
		case "probabilities":
			present = raw.Probabilities != nil
		case "confidence":
			present = raw.Confidence != nil
		}
		if present {
			return fmt.Errorf("answer for a %s question must not carry %q", q.Type, field)
		}
	}
	return nil
}

// checkConfidence validates the required confidence value in [0, 1].
func checkConfidence(raw *float64, q Question) (float64, error) {
	if raw == nil {
		return 0, fmt.Errorf("%s answer must carry a \"confidence\" field", q.Type)
	}
	if !unitRange(*raw) {
		return 0, fmt.Errorf("confidence %v is outside [0, 1]", *raw)
	}
	return *raw, nil
}

// checkProbabilities validates the required, non-empty probabilities map:
// every key must be known for the question's type and every value must be in
// [0, 1]. Choice answers key probabilities by option name. Score answers key
// them by level name or by zero-based level index ("0", "1", ...) — the
// index-keyed form is a live System One shape that accompanies index-keyed
// legends for probability-weighted scores.
func checkProbabilities(raw *map[string]float64, q Question) (map[string]float64, error) {
	if raw == nil {
		return nil, fmt.Errorf("%s answer must carry a \"probabilities\" field", q.Type)
	}
	if len(*raw) == 0 {
		return nil, errors.New("probabilities must be non-empty")
	}
	known := make(map[string]bool)
	for _, key := range criteriaKeys(q) {
		known[key] = true
	}
	indexKeys := make(map[string]bool)
	if q.Type == TypeScore {
		for i := range q.Levels {
			indexKeys[strconv.Itoa(i)] = true
		}
	}
	for key, value := range *raw {
		if !known[key] && !indexKeys[key] {
			return nil, fmt.Errorf("probabilities key %q is not one of the question's %s", key, criteriaKind(q))
		}
		if !unitRange(value) {
			return nil, fmt.Errorf("probabilities[%q] = %v is outside [0, 1]", key, value)
		}
	}
	return *raw, nil
}

// criteriaKeys returns the keys probabilities may use: the choice options
// or the score levels.
func criteriaKeys(q Question) []string {
	if q.Type == TypeScore {
		return q.Levels
	}
	keys := make([]string, 0, len(q.Options))
	for key := range q.Options {
		keys = append(keys, key)
	}
	return keys
}

// criteriaKind names what probability keys must be, for error messages.
func criteriaKind(q Question) string {
	if q.Type == TypeScore {
		return "levels"
	}
	return "options"
}

// unitRange reports whether v is a finite number in [0, 1].
func unitRange(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}
