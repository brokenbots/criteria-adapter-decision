package decisionclient

// Answer decode (ADR-0013 wire contract, M2). Upstream answers are decoded
// losslessly and mapped verbatim onto the validated questions; every decode
// or mapping defect is an Execute error, never a silent drop.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Answer is the validated, lossless form of one upstream answer for the
// question of the same ID:
//
//   - choice: Choice is the selected option name; Probabilities maps known
//     options to their probabilities; Confidence is in [0, 1].
//   - score:  Score is the zero-based level index and Legend the matching
//     level name; Probabilities maps known levels; Confidence in [0, 1].
//   - noul:   Noul is the verdict "yes" or "no" (no probabilities or
//     confidence).
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

// answersEnvelope is the strict-decode view of the upstream response: the
// wire contract has exactly one top-level key, so unknown keys fail.
type answersEnvelope struct {
	Answers []json.RawMessage `json:"answers"`
}

// rawAnswer is the strict-decode view of one answer object. Pointer fields
// distinguish "key absent" from "present but invalid", which the cross-type
// and presence checks depend on.
type rawAnswer struct {
	ID            *string             `json:"id"`
	Choice        *string             `json:"choice"`
	Score         *int                `json:"score"`
	Legend        *string             `json:"legend"`
	Noul          *string             `json:"noul"`
	Probabilities *map[string]float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

// DecodeAnswers maps the upstream answers JSON onto answers for the given
// questions, aligned to question order. Decoding is strict and lossless:
//
//   - the envelope carries exactly the "answers" key;
//   - answer objects carry no unknown keys and no fields belonging to
//     another question type;
//   - each payload field is present, well-formed, and range-checked;
//   - coverage is exact — one answer per question, with duplicates and
//     answers to unknown questions rejected.
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

	byID := make(map[string]int, len(questions))
	for i, q := range questions {
		if _, dup := byID[q.ID]; dup {
			return nil, fmt.Errorf(errPrefix+"decode answers: duplicate question id %q", q.ID)
		}
		byID[q.ID] = i
	}

	answers := make([]Answer, len(questions))
	answered := make(map[string]bool, len(questions))
	for i, r := range env.Answers {
		var rawA rawAnswer
		if err := strictUnmarshal(r, &rawA); err != nil {
			return nil, fmt.Errorf(errPrefix+"answers[%d]: decode answer: %w", i, err)
		}
		if rawA.ID == nil {
			return nil, fmt.Errorf(errPrefix+"answers[%d]: answer id is required", i)
		}
		idx, ok := byID[*rawA.ID]
		if !ok {
			return nil, fmt.Errorf(errPrefix+"answers[%d]: answer for unknown question %q", i, *rawA.ID)
		}
		q := questions[idx]
		if answered[q.ID] {
			return nil, fmt.Errorf(errPrefix+"answers[%d]: duplicate answer for question %q", i, q.ID)
		}
		a, err := checkRawAnswer(&rawA, q)
		if err != nil {
			return nil, fmt.Errorf(errPrefix+"answers[%d]: %w", i, err)
		}
		answers[idx] = a
		answered[q.ID] = true
	}
	for _, q := range questions {
		if !answered[q.ID] {
			return nil, fmt.Errorf(errPrefix+"decode answers: no answer for question %q", q.ID)
		}
	}
	return answers, nil
}

// checkRawAnswer validates the decoded answer against its question and
// returns the typed form. The presence matrix is exact: the payload fields
// for the question type are required and every other type's fields are
// cross-type errors.
func checkRawAnswer(raw *rawAnswer, q Question) (Answer, error) {
	a := Answer{QuestionID: q.ID}
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
		if *raw.Score < 0 || *raw.Score >= len(q.Levels) {
			return Answer{}, fmt.Errorf("score %d is outside the question's %d levels", *raw.Score, len(q.Levels))
		}
		if raw.Legend == nil || *raw.Legend == "" {
			return Answer{}, errors.New(`score answer must carry a non-empty "legend" field`)
		}
		if *raw.Legend != q.Levels[*raw.Score] {
			return Answer{}, fmt.Errorf("score answer legend %q does not match level %d (%q)", *raw.Legend, *raw.Score, q.Levels[*raw.Score])
		}
		a.Score = *raw.Score
		a.Legend = *raw.Legend

	case TypeNoul:
		if err := requireAbsent(raw, q, "choice", "score", "legend", "probabilities", "confidence"); err != nil {
			return Answer{}, err
		}
		if raw.Noul == nil {
			return Answer{}, errors.New(`noul answer must carry a "noul" field`)
		}
		if *raw.Noul != "yes" && *raw.Noul != "no" {
			return Answer{}, fmt.Errorf("noul answer must be exactly %q or %q; got %q", "yes", "no", *raw.Noul)
		}
		a.Noul = *raw.Noul
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

// requireAbsent rejects cross-type fields with a message naming the
// question type and the offending field.
func requireAbsent(raw *rawAnswer, q Question, fields ...string) error {
	for _, field := range fields {
		var present bool
		switch field {
		case "choice":
			present = raw.Choice != nil
		case "score":
			present = raw.Score != nil
		case "legend":
			present = raw.Legend != nil
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
// every key must be a known option (choice) or level (score) and every
// value must be in [0, 1].
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
	for key, value := range *raw {
		if !known[key] {
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
