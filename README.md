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

## decisionclient

The isolated HTTP client for the System One wire format. It never applies
defaults for `base_url` or `model` (both are explicitly required), draws no
distinction between backends, and returns the backend's raw JSON response
byte-identical:

```go
client, err := decisionclient.New("https://s1.typesafe.ai", "clef", os.Getenv("SYSTEM_ONE_API_KEY"))
if err != nil {
    return err // base URL/model validation, or bad scheme
}
response, err := client.Decision(ctx, "ready", []string{"Does the run pass?"})
// response is json.RawMessage — the verbatim response body
```

When the optional api key is omitted (or empty), requests carry no
`Authorization` header; when a key is given, requests carry
`Authorization: Bearer <key>`.
