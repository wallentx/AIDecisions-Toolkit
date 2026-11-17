---
name: aid-diff
description: Use AID.diff to assess a Git diff with named probability checks for behavior changes, compatibility, security, or custom criteria. Use when code-change triage helps select review or validation work.
---

# Assess a code change

Run `{{AID_COMMAND}} --help` for available flags. The installed command accepts
a diff on stdin, a file with `--diff FILE`, or staged changes with `--staged`.

```sh
{{AID_COMMAND}} --staged --json
{{AID_COMMAND}} --provider gemini --model gemini-decision-1 --staged --json
{{AID_COMMAND}} --provider typesafe --model jev-latest --staged --json
git diff --no-ext-diff --no-textconv | {{AID_COMMAND}} --json
{{AID_COMMAND}} --example --dry-run
```

Live requests send the diff and optional `--context` to the configured
Decisions provider (`openai` by default, or `typesafe`, `gemini`, or `litellm`).
Use `--dry-run` to inspect the resolved provider, endpoint, model, and
translated wire payload without reading an API key or making a network call.

Each provider reads its own environment variable (`OPENAI_API_KEY`,
`TYPESAFE_API_KEY`, `GEMINI_API_KEY`, or `LITELLM_API_KEY`), then its isolated
`api_key_file` in `~/.config/AID/config.yml` or `--api-key-file`. Keep secret
keys out of prompts, command lines, and terminal output.

For custom checks, pass `--checks FILE` with a JSON object mapping stable names
to explicit yes/no questions. Include relevant behavior or compatibility
constraints with `--context TEXT`.

JSON results map each check name to an answer with `type`, `name`, and, for a
predicate, `probability` between 0 and 1 (plus `confidence` when returned by
the provider). A refusal remains unanswered. Use probabilities to prioritize
the next concrete inspection or test. Model probabilities do not prove that a
change is correct and do not authorize merges, deployments, or edits. Calibrate
thresholds against known examples before using them for unattended automation.

The command returns exit code `0` when answered, `1` on runtime or API failure,
`2` on invalid input or an unsupported provider capability, and `3` when at
least one refusal occurs. Preserve useful answers when exit code `3` is
returned.
