# Laya Classifier Backend

Quick-start guide for the Laya verdict backend. Lands in PR #93
(branch `feat/classifier-corpus-laya`). Design rationale lives in
`docs/adr/0014-local-classifier-corpus.md` and
`docs/adr/0015-laya-serve-verdict-backend.md`; operator rule syntax lives in
`docs/classifier-rules.md`.

## What it does

The classifier normally asks an upstream model for a severity verdict, which
costs quota and adds latency. The Laya backend instead asks a self-hosted
[laya-serve](https://pypi.org/project/laya/) sidecar. The proxy translates the
classifier request into laya's typed-decision protocol (`POST /v1/systemone`),
maps the answer back to a severity, and returns a normal verdict. The client
sees no difference.

Base laya checkpoints score near chance on this task zero-shot. The backend is
useful today as a fast, local, non-blocking verdict. It becomes a real
replacement after fine-tuning on captured corpus (see below).

## Data flow

1. A `reroute` rule matches a classifier request and targets a backend with
   `"format": "laya"`.
2. The adapter extracts the graded action from the request body. The full body
   is about 125 KB; laya's English checkpoint holds about 320 tokens of state,
   so only the action is sent. If the action exceeds `stateChars`, it is
   truncated from the left (the action tail is what is judged).
3. The adapter sends one typed `choice` question (`risk` by default) with the
   configured instructions and criteria.
4. laya-serve answers with a label and confidence.
5. The label maps to a severity through `layaSeverityMap`, clamps to
   `layaMaxSeverity`, and renders as the verdict shape the request kind needs
   (Stage 1 emits `<severity>`, Stage 2 emits `<thinking>` plus `<severity>`).
6. The verdict returns to the client. The row is also recorded to the corpus
   with `source: "laya"`.

## Configuration

Add a backend in `config.json`:

```json
{
  "name": "laya-local",
  "format": "laya",
  "url": "http://127.0.0.1:8000/v1/systemone",
  "model": "english",
  "timeoutMs": 5000
}
```

Then point a rule at it:

```json
{
  "classifier.enabled": true,
  "classifier.rules": [
    { "match": { "model": "claude-*" }, "action": "reroute", "targetBackend": "laya-local" }
  ]
}
```

Set `apiKey` only if laya-serve was started with `LAYA_API_KEY`. Rules run only
while `classifier.enabled` is `true`.

### Defaults (all overridable per backend)

| Setting | Default | Purpose |
|---|---|---|
| `layaQuestionName` | `risk` | Question key in request and response |
| `layaInstructions` | "Classify the risk of this shell action taken by a coding agent." | Prompt for the choice question |
| `layaCriteria` | A/B/C/D opaque risk bands | Choice options; opaque keys so custom criteria can redefine them |
| `layaSeverityMap` | A:0, B:5, C:15, D:35 | Label-to-severity mapping |
| `layaMaxSeverity` | 49 | Hard clamp on the mapped severity |
| `layaStateChars` | 1200 | Left-truncation limit for the action |
| timeout | 5000 ms | Laya default; other formats use 20000 ms |

## Safety model

Severity 50 is the allow/block boundary. Every default maps to a value below
50, and `layaMaxSeverity` (default 49) clamps the result, so with defaults a
Laya verdict **cannot block**. It can block only if an operator sets
`layaMaxSeverity` to 50 or more **and** maps a label to 50 or more in
`layaSeverityMap`. Do not do this with an unfine-tuned checkpoint: it will
block routine actions at random.

## Failure modes

All failures fail the reroute and fall through to the proxy's built-in
handling. No guess is ever returned.

- laya-serve unreachable or timeout.
- Non-200 response.
- Response has no answer for the configured question name.
- Answer label not present in the severity map.
- Request kind is `block-prefilter` or `none`: the adapter answers Stage 1 and
  Stage 2 severity requests only, because the block-prefilter response format
  has never been captured and must not be guessed.

## Verifying a deployment

The wire contract comes from the laya-serve README, and every failure mode is
silent by design. Run the wire check before relying on the backend, and again
after any laya-serve upgrade:

```bash
python3 scripts/check_laya.py --url http://127.0.0.1:8000/v1/systemone
```

To prove the full reroute path through the proxy, send one graded action with
capture enabled and confirm the newest corpus row has `"source": "laya"`.
That row proves the request was rerouted, the label mapped to a severity, and
the verdict recorded. If laya-serve runs on another machine (it needs a
downloaded checkpoint, and a GPU for fast answers), hand `check_laya.py` and
this section to whoever runs the sidecar.

## Fine-tuning from corpus

The proxy records every classifier request and verdict as JSONL rows
(`classifier-YYYY-MM-DD.jsonl`, schema v1). Each row carries a `source` field:
`upstream` rows hold a teacher label; `laya` rows are the local model's own
answers. Training on `laya` rows would teach the model its own errors, so the
exporter keeps `upstream` rows only.

Export training data:

```bash
python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl -o train.jsonl
```

Each exported row is `{state, questions, answers}`. The exporter keeps rows
with `source: "upstream"`, `kind: "stage1-severity"`, a severity of 0 or more,
and a non-empty action. Stage 1 is kept alone by default because Stage 2 also
applies user intent that the exported state does not carry; pass `--kind` to
include others.

Two drift guards protect the pipeline:

- `scripts/test_laya_criteria_sync.py` fails the build if the exporter's
  hard-coded question name, instructions, or criteria drift from the Go
  defaults in `internal/config/config.go`.
- The criteria are risk bands that cite the severity ranges the exporter
  buckets by (A: 0-9, B: 10-24, C: 25-49, D: 50-100), because the teacher emits
  a numeric severity, not an action type.

Nothing in this repository validates the export against laya's training
loader. Confirm the shape against laya's own fine-tuning notebook before a
real run. A backend that overrides `layaQuestionName`, `layaInstructions`, or
`layaCriteria` will not match a checkpoint trained on this export.

## Current limits

- **Stage 1 and Stage 2 severity requests only.** Block-prefilter and
  non-classifier requests fall through.
- **No blocking by default.** The verdict is a locally computed, plausible
  allow until a fine-tuned checkpoint and a measured agreement rate exist.
- **Action-only state.** Context and conversation history are not sent;
  judgments use the action alone.
