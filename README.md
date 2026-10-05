# criteria-adapter-decision

Decision adapter for the [criteria](https://github.com/brokenbots/criteria) workflow engine:
serves the System One decision-model wire format (`POST /v1/systemone`) exposed by
[TypeSafe AI](https://docs.typesafe.ai) (Jev) in the cloud and
[Ollama >= v0.35.1](https://github.com/ollama/ollama/releases/tag/v0.35.1)
(Clef / Clef Flash) locally.

## Status

Each milestone below describes what the adapter does today, as it was built
(Kanboard 200-206, ADR-0013):

- **M1 scaffold (Kanboard 200)**: the repo builds the protocol-v2 adapter
  binary with gRPC sessions wired (`Info`, `OpenSession`, `Execute`) and ships
  the isolated System One HTTP client next to it.
- **M2 wire contract (Kanboard 201)**: questions and state are validated
  fail-closed before any HTTP traffic (bareword ids, known types, criteria
  matching the question type, no unknown keys), and upstream answers decode
  losslessly onto typed values — every decode defect is an error, never a
  silent drop.

- **M3 mapping (Kanboard 202-203)**: the session opens fail-closed — config
  keys are exact (`base_url`, `model`, `timeout`, `retries`, `outcome_question`),
  `timeout` must be a positive Go duration, `retries` a non-negative whole number
  of extra attempts (default 0) — and the `outcome_question` mapping is strict:
  the selected choice of the ONE configured choice question maps verbatim onto
  the step outcome and must be a member of that step's declared outcomes, or the
  step fails with the typed `outcome_out_of_set` payload.

- **M5 info surface (Kanboard 204)**: the Info handshake declares the
  full compile-time config/input/output schemas, the `api_key` secret, and the
  `parallel_safe` capability; the SDK default `--emit-manifest` emits a JSON
  manifest that CI checks (schema round trip + determinism via `make
  manifest-check`).

- **M7 docs (Kanboard 206)**: this README and
  [docs/backends.md](docs/backends.md) document the full contract — backend
  guide included (TypeSafe cloud keys, the Ollama model list) — and the adapter
  is mentioned in criteria's
  [adapter reference](https://github.com/brokenbots/criteria/blob/main/docs/adapters.md).

Until the series closes (Kanboard 207), the module pins
[criteria-adapter-proto](https://github.com/brokenbots/criteria-adapter-proto)
at v0.5.x; see [docs/dependency-policy.md](docs/dependency-policy.md) for the
dated exception.


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
client, err := decisionclient.New("https://s1.typesafe.ai", "jev", os.Getenv("SYSTEM_ONE_API_KEY"))
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
## Backends

Every backend serves the identical wire format (`POST <base_url>/v1/systemone`);
the adapter and its client never specialize by backend — `base_url` and
`model` name the backend explicitly, and there are no defaults:

| Backend | `base_url` | Models | Credential |
| --- | --- | --- | --- |
| [TypeSafe AI](https://docs.typesafe.ai) (Jev) — cloud | `https://s1.typesafe.ai` | `jev` | optional `api_key`: when set, every request carries an `Authorization` bearer header; absent/empty means anonymous requests |
| [Ollama >= v0.35.1](https://github.com/ollama/ollama/releases/tag/v0.35.1) (Clef / Clef Flash) — local | `http://localhost:11434` | `clef`, `clef-flash` | none — the endpoint is unauthenticated |

The full per-backend guide — the secret-tainted `api_key` variable (D69), the
complete config matrix, timeout/retries semantics, and what stays identical
across backends — is [docs/backends.md](docs/backends.md).

## Adapter contract (compile-time Info schemas)

The adapter's Info handshake declares the shape the compiler enforces: only
these config keys, only these input keys, only these output references. A
workflow referencing the adapter is validated against the schemas at
`criteria compile` time — no engine change and no runtime round trip needed.

### Adapter block

```hcl
# Secret source: values for the api_key must be secret-tainted (D69); a
# plaintext literal is a compile error.
variable "systemone_api_key" {
  type   = string
  secret = true
}

# Cloud (TypeSafe AI / Jev): authenticated, per-call deadline, retry knob.
adapter "decision" "system_one" {
  config {
    base_url         = "https://s1.typesafe.ai" # adapter appends /v1/systemone
    model            = "jev"                    # required; no default
    timeout          = "45s"                    # optional; absent = no deadline ("0s" is rejected)
    retries          = 1                        # optional extra attempts; default 0
    outcome_question = "does_run_pass"          # optional; ADR-0013 D3 mapping
  }
  secrets {
    api_key = var.systemone_api_key
  }
}

# Local (Ollama >= v0.35.1 / Clef or Clef Flash): unauthenticated endpoint.
adapter "decision" "local_system_one" {
  config {
    base_url = "http://localhost:11434"
    model    = "clef-flash"
  }
}
```

| Config key | Type | Description |
| --- | --- | --- |
| `base_url` | string, **required** | Fully-qualified base URL of the System One endpoint; the adapter appends `/v1/systemone`. No default. |
| `model` | string, **required** | The System One decision model to invoke (e.g. `jev` on the TypeSafe cloud or `clef`/`clef-flash` on Ollama — see [docs/backends.md](docs/backends.md)). No default. |
| `timeout` | string | Optional per-call HTTP deadline as a positive Go duration (`45s`, `2m`). A present but invalid or zero value fails the session open. Absent = no per-call deadline. |
| `retries` | number | Optional extra attempts for retryable failures (HTTP 429 and 5xx; Retry-After honored). Default `0`. |
| `outcome_question` | string | Optional bareword id of the ONE choice question mapped onto the step outcome (below). |

### Input and secret input

```hcl
step "decide" {
  target = adapter.decision.system_one
  input {
    questions = <<-EOT
      [
        {"id":"does_run_pass","type":"choice",
         "instructions":"Pick the run outcome.",
         "criteria":{"pass":"The run passed.","fail":"The run failed."}}
      ]
    EOT
    state     = "{\"branch\":\"main\"}"
  }
  secret_input {
    api_key = var.systemone_api_key
  }
  outcome "success" { next = step.act }
  outcome "failure" { next = step.review }
}
```

The plaintext `input {}` channel carries exactly the keys `questions` and
`state`; anything else — notably `api_key` — is rejected fail-closed before any
HTTP traffic (it must be bound over the secret `secret_input {}` channel or the
adapter `secrets {}` block instead).

Input grammar:

| Key | Shape | Validation (fail-closed, before any HTTP traffic) |
| --- | --- | --- |
| `questions` | JSON array of question objects | Decoded strictly. Question object shape: `id` bareword (ASCII letters, digits, underscores; not starting with a digit; unique per array) · `type` `choice\|score\|noul` · `instructions` non-empty · `criteria` matching the type. |
| `state` | JSON string, object, or array | Decoded strictly; numbers, booleans, and `null` are rejected. |

Question-object `criteria` per type:

- `choice` — non-empty object `{ "<option name>": "<description>", ... }`.
- `score` — non-empty ordered level list, e.g. `["low","medium","high"]`.
- `noul` — an optional yes/no gloss string (absent is allowed).

### Outputs and the failure payload

On step success the outputs are `steps.<step>.answers` (the backend's answers
array, byte-verbatim; each entry carries its question's `id` plus exactly the
payload of that question's type — `choice` = `{choice, confidence,
probabilities}`, `score` = `{score, legend, confidence, probabilities}`,
`noul` = `{noul}`, and nothing else: `confidence` is a finite number in [0, 1]
for graph-side gating, `probabilities` maps the question's known
options/levels onto values in [0, 1], and payload fields of other question
types are rejected) and `steps.<step>.usage` (the backend usage object,
byte-verbatim; may be empty). The adapter's outcome mapping resolves its
question by `id`, but the answer array keeps the backend's order — graph-side
positional indexing (`answers[0]`, below) relies on the backend answering in
question order, so prefer matching entries by `id` when order matters. On
step failure the output is `steps.<step>.error`, the typed failure payload:

| `error.kind` | Meaning |
| --- | --- |
| `http` | The backend responded with a non-2xx status (payload carries `status`). |
| `auth` | Authentication failure per the backend. |
| `timeout` | The per-call deadline elapsed. |
| `decode` | The backend response violated the System One envelope contract. |
| `transport` | The HTTP transport failed (connection refused, reset, etc.). |
| `canceled` | The context was canceled. |
| `outcome_out_of_set` | The mapped question's selected choice isn't in the step's declared outcomes. The payload's `allowed` lists the step's declared outcomes. |

The payload's `retryable` field marks whether another attempt would help;
retryable classes are retried up to the `retries` config (default 0). A
failure payload example:

```json
"error": {
  "kind": "http",
  "status": 429,
  "message": "systemone: 429 Too Many Requests: rate limited",
  "retryable": true
}
```

### outcome_question contract

With `outcome_question = "does_run_pass"` configured, the selected choice of
THE one named `choice` question maps verbatim onto the step outcome; the
choice must be a member of that step's declared outcomes or the step fails
with the typed `outcome_out_of_set` payload (`allowed` lists the step's
declared outcomes). When the config is absent, empty, or the step's questions
don't carry the configured id, the step is outcome_pure: it always succeeds
regardless of `allowed_outcomes`.

```hcl
step "classify" {
  target = adapter.decision.system_one
  input {
    questions = "[{\"id\":\"does_run_pass\",\"type\":\"choice\",\"instructions\":\"Pick the run outcome.\",\"criteria\":{\"pass\":\"...\",\"fail\":\"...\",\"hold\":\"...\"}}]"
    state     = "{\"branch\":\"main\"}"
  }
  # Declared outcomes narrow the mapping: choosing an undeclared one fails
  # the step with outcome_out_of_set (fail-closed, never a silent pass).
  outcome "pass"  { next = step.act }
  outcome "fail"  { next = state.remediate }
}

switch "gate" {
  # Confidence gating on the graph: only confident answers skip review.
  match {
    condition = steps.classify.answers[0].confidence >= 0.85
    next      = step.act
  }
  default { next = step.review }
}
```

### Manifest emission (`--emit-manifest`)

The SDK default `--emit-manifest` flag prints the adapter manifest (JSON) and
exits: identity (`name`, `version`, `description`, `source_url`),
`capabilities`, `platforms`, `sdk_protocol_version`, and the three schemas
`config_schema` / `input_schema` / `output_schema` plus `secrets`.
`criteria adapter publish` runs it to produce and validate the manifest before
pushing. CI emits it twice, requires byte-identical output (determinism), and
gates the structure with `make manifest-check` (mirrored in-tree by
`TestManifestRoundTrip`, which decodes under the host strict-parser
semantics); the host engine's manifest parser (KB-180) parses the same
document — pre-180, the emitted document is canonical JSON.

## Build, test, gates

The Makefile carries the same gates CI runs on every push and PR:

```sh
make build          # go build ./...
make test           # go test -race ./...
make vet            # go vet ./...
make manifest-check # --emit-manifest round trip: structural gates + determinism
make vuln-scan      # osv-scanner (version pinned), local parity with the CI osv-scan job
```

`make deps-outdated` and `make deps-majors` report dependency freshness
(WS50); the policy and any below-latest pins — and their dated exceptions —
live in [docs/dependency-policy.md](docs/dependency-policy.md). CI's
`deps-report` job publishes the freshness report non-blocking on every PR.
