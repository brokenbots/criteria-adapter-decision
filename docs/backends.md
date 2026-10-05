# System One backends

The decision adapter is backend-agnostic: it POSTs one wire format —
`{model, state, questions}` to `<base_url>/v1/systemone` — and never applies
defaults, so `base_url` and `model` name the backend explicitly in every
workflow. Two backends serve the format today:

| Backend | `base_url` | Models | Credential |
| --- | --- | --- | --- |
| [TypeSafe AI](https://docs.typesafe.ai) (Jev) — cloud | `https://s1.typesafe.ai` | `jev` | optional `api_key` (bearer) |
| [Ollama >= v0.35.1](https://github.com/ollama/ollama/releases/tag/v0.35.1) (Clef / Clef Flash) — local | `http://localhost:11434` | `clef`, `clef-flash` | none — endpoint is unauthenticated |

## TypeSafe AI (Jev) — cloud

Authenticated cloud endpoint. The adapter appends `/v1/systemone`, so the
`base_url` is the bare origin:

```hcl
variable "systemone_api_key" {
  type        = string
  default     = ""
  secret      = true # D69: values for secret_input/secrets{} must be secret-tainted
  description = "Optional API key for the System One decision backend."
}

adapter "decision" "system_one" {
  config {
    base_url         = "https://s1.typesafe.ai"
    model            = "jev"
    timeout          = "45s"
    retries          = 1
    outcome_question = "department"
  }
  secrets {
    api_key = var.systemone_api_key
  }
}
```

Credential rules (exactly what the adapter does):

- The secret arrives through the adapter `secrets{}` block — or per-step via
  `secret_input {}`, which overlays the adapter secret for that call — never
  through the plaintext `input{}` channel (execute fails closed on
  `api_key` there).
- When the resolved key is non-empty, every request carries
  `Authorization: Bearer <key>`. When it is absent or empty
  (e.g. default `""`), NO authorization header is sent — the adapter treats
  an empty key as anonymous, and this is never a session-open error.
- The adapter never reads the key from the process environment and never
  logs it; adapter events redact secret values to `[REDACTED]`.

`timeout` and `retries` are optional; `45s` / `1` are the example values, not
adapter defaults (absent `timeout` = no per-call deadline, `retries`
default `0` = no extra attempts).

## Ollama >= v0.35.1 — local

Unauthenticated local endpoint (same `System One` wire format):

```hcl
adapter "decision" "system_one" {
  config {
    # The adapter appends /v1/systemone.
    base_url = "http://localhost:11434"
    model    = "clef-flash"
    timeout  = "45s"
  }
}
```

- Models: `clef`, `clef-flash`.
- The endpoint is unauthenticated: no `secrets{}` block at all — the same
  grammar above runs with or without a credential.
- The endpoint URL and model list are the only backend differences; request
  shape, validation, and error semantics are identical.

## Config matrix (strict posture)

The session open is fail-closed: unknown config keys are rejected, and
required keys have no fallbacks.

| Config key | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `base_url` | string | yes | — (no default) | http/https URL, non-empty host; must not embed `user:pass` credentials; `/v1/systemone` is appended. |
| `model` | string | yes | — (no default) | Decision model id, e.g. `jev` (cloud) or `clef`/`clef-flash` (Ollama). |
| `timeout` | string | no | absent = no per-call deadline | Positive Go duration (`45s`); an invalid or `0s` value fails the session open. |
| `retries` | number | no | `0` | Extra attempts for retryable HTTP failures (429/5xx, per `Retry-After`); must be a non-negative whole number. |
| `outcome_question` | string | no | absent = no mapping | Bareword id of the ONE `choice` question mapped onto the step outcome; a carried question of any other type is a config error before any HTTP traffic. |

Every example in this guide spells out the required keys explicitly
(`base_url`, `model`) — nothing is implicit.

## What stays identical across backends

- **Request**: `POST <base_url>/v1/systemone` with `Content-Type:
  application/json` and `Accept: application/json` (+ the `Authorization`
  bearer header only when a non-empty `api_key` is configured); body
  `{model, state, questions}` in fixed wire order, questions in the order
  the step declares.
- **Validation before traffic**: the strict question/state contract —
  bareword ids, known types, type-matching criteria, exact input keys, no
  unknown fields — is validated fail-closed before a connection is made.
- **Response envelope**: exactly `{model, answers, usage}`, all required;
  `answers` is a JSON array decoded strictly against the questions
  (id-based, exact coverage) and both `answers` and `usage` are kept
  byte-verbatim for the workflow outputs; response bodies are capped at
  16 MiB.
- **Answers**: each entry carries its question's `id` plus exactly the
  payload of that question's type — `choice` = `{choice, confidence,
  probabilities}`, `score` = `{score, legend, confidence, probabilities}`,
  `noul` = `{noul}`; nothing else. `confidence` is a finite number in [0, 1]
  and `probabilities` map the question's known options/levels onto values
  in [0, 1] (both required for choice/score, rejected for noul); payload
  fields of other question types are cross-type errors. The output array
  keeps the backend's order.
- **Errors**: `{kind, status, message, retryable[, allowed]}` where kind is
  `http`, `auth`, `timeout`, `decode`, `transport`, `canceled`, or — at the
  adapter level, for a mapped choice outside the step's declared outcomes —
  `outcome_out_of_set`; only `http` (429/5xx, honoring `Retry-After`),
  `timeout`, and `transport` are retryable, and the `retries` config drives
  the attempts.