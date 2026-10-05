# criteria-adapter-decision

Decision adapter for the [criteria](https://github.com/brokenbots/criteria) workflow engine:
serves the System One decision-model wire format (`POST /v1/systemone`) exposed by
[TypeSafe AI](https://docs.typesafe.ai) (Jev) in the cloud and
[Ollama >= v0.35.1](https://github.com/ollama/ollama/releases/tag/v0.35.1)
(Clef / Clef Flash) locally.

## Status

M1 scaffold (Kanboard 200, ADR-0013): the repo builds the protocol-v2 adapter
binary (sessions wired, execution fail-closed — `Execute` returns an error until
Kanboard 202-206 land) and ships the isolated System One HTTP client.

M2 wire contract (Kanboard 201, ADR-0013): questions and state are validated
fail-closed before any HTTP traffic (bareword ids, known types, criteria
matching the question type, no unknown keys), and upstream answers decode
losslessly onto typed values — every decode defect is an error, never a
silent drop.

## decisionclient

The isolated HTTP client for the System One wire format. It never applies
defaults for `base_url` or `model` (both are explicitly required), draws no
distinction between backends, validates the strict question/state contract
before contacting a backend, and returns the backend's raw JSON response
byte-identical:

```go
questions := []decisionclient.Question{
    {ID: "does_run_pass", Type: decisionclient.TypeChoice, Instructions: "Pick the run outcome.",
     Options: map[string]string{"pass": "The run passed.", "fail": "The run failed."}},
    {ID: "safe", Type: decisionclient.TypeNoul, Instructions: "Is the run safe?"},
}
state, err := decisionclient.ParseState([]byte(`{"branch":"main"}`)) // string, object, or array
if err != nil {
    return err // strictly validated, fail-closed
}
client, err := decisionclient.New("https://s1.typesafe.ai", "clef", os.Getenv("SYSTEM_ONE_API_KEY"))
if err != nil {
    return err // base URL/model validation, or bad scheme
}
response, err := client.Decision(ctx, state, questions)
// response is json.RawMessage — the verbatim response body
if err != nil {
    return err // validation failures happen before any HTTP traffic
}
answers, err := decisionclient.DecodeAnswers(response, questions)
// answers: one typed decisionclient.Answer per question, in question order
```

When the optional api key is omitted (or empty), requests carry no
`Authorization` header; when a key is given, requests carry
`Authorization: Bearer <key>`.
