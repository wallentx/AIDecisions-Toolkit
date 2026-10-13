---
name: aid-diff
description: Use AID.diff to assess a Git diff with named probability checks for behavior changes, compatibility, security, or custom criteria. Use when code-change triage helps select review or validation work.
---

# Assess a code change

Run `{{AID_COMMAND}} --help` for available flags. The installed command accepts
a diff on stdin, a file with `--diff FILE`, or staged changes with `--staged`.

```sh
{{AID_COMMAND}} --staged --json
git diff --no-ext-diff --no-textconv | {{AID_COMMAND}} --json
{{AID_COMMAND}} --example --dry-run
```

Live requests send the diff and optional `--context` to OpenAI. Use `--dry-run`
to inspect the request without a key or network call. Credentials come from
`OPENAI_API_KEY`, then the configured `api_key_file`; do not print the key or
insert it into a prompt. Shared settings live in `~/.config/AID/config.yml`.

For custom checks, pass `--checks FILE` with a JSON object mapping stable names
to explicit yes/no questions. Include relevant behavior or compatibility
constraints with `--context TEXT`.

JSON results map each check name to an answer with `type`, `name`, and, for a
predicate, `probability` between 0 and 1. A refusal is unanswered, never false.
Use probabilities to prioritize the next concrete inspection or test. They are
not proof that a change is correct and do not grant permission to merge, deploy,
or modify anything. Calibrate thresholds against known examples before using
them for unattended decisions.

Exit codes: `0` answered, `1` runtime/API failure, `2` invalid input, `3` at
least one refusal. Preserve useful answers when exit code 3 is returned.
