# AIDecisions-Toolkit configuration and usage

Configuration lives at `${XDG_CONFIG_HOME:-~/.config}/AID/config.yml`. AID also
accepts `AID_CONFIG` or `--config PATH`. Initialization creates a default file
if none exists, and existing configuration files are preserved.

```yaml
version: 1
global:
  provider: openai
  api_key_file: "~/.ssh/tokens/openai"
  model: gpt-6-luna
  timeout_seconds: 30
  output: auto
providers:
  openai:
    api_key_file: "~/.ssh/tokens/openai"
  typesafe:
    api_key_file: "~/.ssh/tokens/typesafe"
    model: jev-latest
  gemini:
    api_key_file: "~/.ssh/tokens/gemini"
    model: gemini-decision-1
  openrouter:
    api_key_file: "~/.ssh/tokens/openrouter"
    model: "~typesafe/jev-latest"
  litellm:
    api_key_file: "~/.ssh/tokens/litellm"
    endpoint: "http://127.0.0.1:4000"
    model: jev
updates:
  repository: "wallentx/AIDecisions-Toolkit"
  install_dir: "" # Empty means the current AID executable directory
tools:
  diff:
    checks_file: ""
    context: ""
```

Every `api_key_file` entry stores a **file path** and never a raw secret key.
Provider-specific environment variables take precedence over key files.

## Supported Decisions providers

`AID` integrates natively with OpenAI, TypeSafe Jev, Google Gemini, and
OpenRouter over Go's standard `net/http` client, so compiled binaries need no
Python, Node.js, or external proxy process. An optional `litellm` adapter
targets externally managed LiteLLM gateways for self-hosted or enterprise-routed
decision models.

| Provider ID | Native / Gateway | Default model | Default endpoint | Auth header & environment variable |
| --- | --- | --- | --- | --- |
| `openai` (default) | Native | `gpt-6-luna` | `https://api.openai.com/v1/decisions` | `Authorization: Bearer` via `OPENAI_API_KEY` |
| `typesafe` | Native (System One) | `jev-latest` | `https://api.typesafe.ai/v1/systemone` | `Authorization: Bearer` via `TYPESAFE_API_KEY` |
| `gemini` | Native (`v1beta`) | `gemini-decision-1` | `https://generativelanguage.googleapis.com/v1beta/decisions` | `x-goog-api-key` via `GEMINI_API_KEY` |
| `openrouter` | Native (multi-model) | `~typesafe/jev-latest` | `https://openrouter.ai/api/alpha/decisions` | `Authorization: Bearer` via `OPENROUTER_API_KEY` |
| `litellm` | Optional gateway | Explicit required | Explicit required (`/v1/decisions` default path, `/v1/systemone` supported) | `Authorization: Bearer` via `LITELLM_API_KEY` |

### Precedence and credential isolation

Provider selection is always explicit or resolved from configuration (`openai`
when omitted); the toolkit never guesses a provider from whichever API key
environment variable happens to be set.

For the resolved provider `P`, `model` and `endpoint` resolve in order:

1. CLI flags (`--model`, `--endpoint`)
2. `tools.<tool>` (`model`, `endpoint`), when `tools.<tool>` targets `P`
3. `providers.<P>` (`model`, `endpoint`)
4. `global` (`model`, `endpoint`), when `global` targets `P`
5. Built-in provider defaults (`gpt-6-luna`, `jev-latest`, `gemini-decision-1`, `~typesafe/jev-latest`)

Switching `--provider` on the command line or under `tools.diff.provider`
automatically drops un-overridden `global` model, endpoint, and `api_key_file`
values bound to another provider. `OPENAI_API_KEY` and `global.api_key_file`
never leak to `typesafe`, `gemini`, `openrouter`, or `litellm`. Relative paths in YAML
resolve relative to `config.yml`, and `~/` expands to the user home directory.

## Equivalent AID.diff invocations

```sh
AID.diff --provider openai --model gpt-6-luna --staged
AID.diff --provider typesafe --model jev-latest --staged
AID.diff --provider gemini --model gemini-decision-1 --staged
AID.diff --provider openrouter --model google/gemini-decision-1 --staged
AID.diff --provider litellm --model jev --endpoint http://127.0.0.1:4000 --staged
```

Pass `--dry-run` to any command above to inspect the resolved `provider`,
`endpoint`, `model`, and translated wire `request` JSON without reading
credentials or making network calls:

```sh
AID.diff --provider gemini --example --dry-run
```

## Canonical question types and Go usage

The `internal/decisions` package exposes a canonical `Evaluator` interface
supporting three question types across all five adapters:

1. `predicate` (alias `noul`): Estimates a yes/no probability in `[0, 1]`. TypeSafe and Gemini adapters translate `predicate` to wire `noul` (`criteria: {true, false}` on TypeSafe; `criteria: {true_description, false_description}` on Gemini) and normalize responses back to `type: "predicate"` with `probability`.
2. `choice`: Selects from 2 to 255 typed `ChoiceOption` values (`StringChoice` or `BoolChoice`), returning the selected `choice`, ordered `choice_probabilities`, and `confidence`. Boolean choices preserve their `bool` type across all providers.
3. `score`: Evaluates an ordered rubric of `ScoreLevel` definitions indexed from `0`, returning the probability-weighted `score`, ordered `score_probabilities`, and `confidence`.

The same canonical predicate request runs unchanged against OpenAI, TypeSafe,
and Gemini and normalizes into the same `Answer` shape:

```go
req, err := decisions.BuildRequest(
	diffText,
	"", // Uses each provider's default model (gpt-6-luna, jev-latest, gemini-decision-1)
	decisions.Checks{"changes_behavior": "Does this diff change observable runtime behavior?"},
)
if err != nil {
	return err
}

for _, client := range []decisions.Client{
	{Provider: decisions.ProviderOpenAI, APIKey: os.Getenv("OPENAI_API_KEY")},
	{Provider: decisions.ProviderTypeSafe, APIKey: os.Getenv("TYPESAFE_API_KEY")},
	{Provider: decisions.ProviderGemini, APIKey: os.Getenv("GEMINI_API_KEY")},
} {
	answers, err := client.Decide(ctx, req)
	if err != nil {
		return err
	}
	fmt.Println(client.Provider, *answers["changes_behavior"].Probability)
}
```

## Provider capability matrix and limitations

| Capability | OpenAI (`openai`) | TypeSafe Jev (`typesafe`) | Google Gemini (`gemini`) | OpenRouter (`openrouter`) | LiteLLM (`litellm`) |
| --- | --- | --- | --- | --- | --- |
| Wire protocol | `POST /v1/decisions` | `POST /v1/systemone` | `POST /v1beta/decisions` | `POST /api/alpha/decisions` (or `/api/v1/systemone`) | `POST /v1/decisions` (or `/v1/systemone`) |
| `predicate` / `noul` | Yes (`predicate`) | Yes (`noul`) | Yes (`noul`) | Yes (`noul`) | Yes |
| Predicate `criteria` (`true`/`false` descriptions) | Unsupported (explicit error) | Yes (`true`, `false`) | Yes (`true_description`, `false_description`) | Yes (`true`, `false`) | `/v1/systemone` only; rejected on `/v1/decisions` |
| Predicate `confidence` in response | Omitted (`nil`) | Omitted (`nil`) | Returned (`[0, 1]`) | Preserved when returned by routed model | Omitted on `/v1/decisions` |
| `choice` (2 to 255 options) | Yes (`string` and `bool`, including `"true"` + `true`) | Yes (`string` and `bool`; rejects `"true"` + `true` collision) | Yes (`string` and `bool`; rejects `"true"` + `true` collision) | Yes (`string` and `bool`; rejects `"true"` + `true` collision) | Yes (`string` and `bool`; rejects `"true"` + `true` collision) |
| `score` rubric levels | 2 to 10 levels | 2 to 10 levels | 2 to 255 levels | 2 to 255 levels (model-dependent upstream) | 2 to 10 levels |
| Questions per request | 1 to 400 | Bounded by 64k tokens / 32 MiB | 1 to 1,000 (keys `[A-Za-z0-9_-]{1,64}`) | Model-dependent (e.g. 200 on Luna, 1,000 on Gemini) | 1 to 128 |
| Refusal / safety representation | Per-question `type: "refusal"` (exit `3` in `AID.diff`) | No per-question refusal type | Request-level HTTP `400` safety code (`ErrSafetyBlocked`) | Preserves `type: "refusal"` when returned | Per-question `type: "refusal"` on `/v1/decisions` |
| Response `model` echo | Returned | Returned (resolved version, e.g. `jev-1.14.0`) | Omitted by API | Returned (resolved model ID) | Returned |

The current implementation enforces three scope and verification boundaries:

- Multimodal inputs (images, audio, video, PDFs), conversation-item arrays, and experimental `box` questions are outside the current canonical v1 scope and return explicit `UnsupportedError` errors rather than dropping fields.
- Automated tests validate all five protocols offline against documented wire schemas using `httptest`; live provider calls are gated behind `AID_LIVE_DECISIONS_TEST=1` and were not executed in automated builds.
- No automatic retries or cross-provider fallbacks occur; HTTP redirects are refused so credentials never cross host boundaries.

## Diff checks and exit codes

`--checks FILE` accepts a nonempty JSON object mapping check names to questions.
The default checks cover behavior changes, compatibility, and security-related
code. `tools/diff/examples/checks.json` shows alternate questions.

```sh
AID.diff --diff change.diff --checks tools/diff/examples/checks.json
AID.diff --staged --output text
AID.diff --example --json --timeout 60
```

Diff and context inputs are limited to 1 MiB each, checks files to 256 KiB, and
API responses to 4 MiB.

| Exit | Meaning |
| --- | --- |
| 0 | Preview or all requested answers received; no safety guarantee |
| 1 | File, Git, credential, network, safety-block, or API failure |
| 2 | Invalid arguments, config, checks, empty diff, or unsupported provider capability |
| 3 | At least one refusal; available answers still printed |

The standalone Python example remains at `tools/diff/python/assess_change.py`
for stdlib-only OpenAI `/v1/decisions` reference use and intentionally does not
load suite YAML.

## Discovery evidence

See [AID.discover](discover.md) for history-based tool discovery. It prepares
evidence without API credentials or model calls and keeps reusable snapshots in
the local private cache. The current agent applies criteria and obtains
independent review; discovery does not install tools or change guidance.

## Install agent skills

```sh
AID --init
AID --init --list
AID --init --provider codex --dry-run
AID --init --provider codex --provider claude
```

The interactive picker supports arrows or j/k, Space to toggle, Enter to
review, u to update checked installed clients, and q/Esc to cancel. Installed
clients start checked; detected clients are displayed without preselection.
The review screen defaults to Cancel. Unchecking an installed client plans its
removal; u updates checked clients without removing unchecked ones.

Explicit `--provider` selections allow scripted setup. `--dry-run` prints a
plan without writing skills or config. Local skills are installed for Codex,
Claude Code, Cursor, Gemini CLI, Antigravity CLI, OpenCode, and Copilot CLI.
Only the selected clients are modified. No hooks or external plugins are
required for the current tools.

Installer state is tracked at `${XDG_STATE_HOME:-~/.local/state}/AID/installer.json`.
Owned hashes protect user edits; conflicting unmanaged or edited files block
application. Files and parents must avoid symlinks. Skill commands bind to
absolute sibling executables, so place the suite in its intended directory
before installing skills. After moving it, rerun initialization.

## Update on the current device

The default release repository is `wallentx/AIDecisions-Toolkit`. Existing
configuration files are preserved; set `updates.repository` if yours is empty:

```sh
AID --update --check
AID --update --dry-run
AID --update
```

`--repo owner/repo` overrides configuration. A newer stable GitHub release
triggers a fresh tag checkout in a temporary directory, native builds, version
verification, and replacement of installed binaries. The user's source
checkout remains untouched. New tools are discovered from release manifests;
retired registered binaries are removed, while unrelated binaries are left
alone. Replacement failures trigger rollback.

Updates need Git, Go, network access, and a writable install directory. Windows
uses a temporary copied helper to replace executables after the launcher exits;
it reports the result-log path without claiming immediate completion. After
updating, rerun `AID --init` to refresh installed skills.

The updater has mock-server, build, and replacement tests. No live GitHub
release update has been performed. Native Windows builds and automated tests
run in CI; interactive Windows installation remains outside this device's
validation.
