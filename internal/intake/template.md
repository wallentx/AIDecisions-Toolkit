# AID tool intake

Use this intake to turn a recurring agent task into a small, dependable
`AID.<tool>` executable. Complete missing decisions with the user, then implement
the agreed scope. AID prints this document; it does not start an LLM or execute
generated code itself.

## 1. Establish the task

Capture the following in the proposed tool's `tools/<id>/design.md`:

- The repeated user ask or agent operation, plus two realistic inputs.
- The information or decision returned and the next action an agent takes.
- Inputs, output schema, failure modes, and which operations have side effects.
- Deterministic logic versus model evaluation, required evidence, and expected cost.
- Acceptance examples, ambiguity/refusal behavior, and explicit exclusions.

Prefer existing AID tools or ordinary deterministic code when they already solve
the task. Model calls need a reason: they should replace repeated semantic
judgment, not a reliable parser, filesystem check, or arithmetic operation.

## 2. Define a bounded contract

Choose a lowercase ID (`[a-z][a-z0-9-]*`) and the command `AID.<id>`.
Read `tools/diff` as the working example. Define stable JSON for agent consumers,
readable terminal output, help/version output, and meaningful exit codes.
Keep stdout for results and stderr for progress/errors. A refusal or missing
evidence must remain distinguishable from a negative answer.

Use shared configuration at `~/.config/AID/config.yml` (or `AID_CONFIG` /
`--config`). CLI options override `tools.<id>`, which overrides `global`.
Decode private fields from the tool's own section. Read credentials from
environment variables or configured key-file paths; never embed or print them.
Offer `--dry-run` for request previews or mutation plans when applicable.

## 3. Add the implementation

```text
tools/<id>/
  tool.json       # ID, command, tool version, description, Go package
  tool.go         # Tool-specific behavior and CLI
  tool_test.go    # Observable behavior and failure cases
  SKILL.md        # When an agent should use it and how to interpret results
cmd/AID.<id>/
  main.go         # Thin executable entrypoint
```

Example manifest:

```json
{
  "id": "example",
  "command": "AID.example",
  "version": "0.1.0",
  "description": "Describe the concrete question this tool answers.",
  "package": "./cmd/AID.example"
}
```

Expose embedded `ManifestJSON` and `Skill` byte slices if the tool needs them.
The launcher automatically embeds and discovers `tools/*/tool.json` and
`tools/*/SKILL.md`; no manual registry edit is needed. The manifest owns metadata; do not duplicate its
version in CLI constants. Keep reusable API transport/configuration/build code
under `internal/`. Do not add cross-tool dependencies to a tool package.

Every executable's `--version --json` must return `command`, `tool_version`, and
`release_version` using `internal/buildinfo`. Keep tool versions separate from
the root `VERSION`, which identifies the tested suite release. Bump each tool
whose observable contract or behavior changes, including changes caused by
shared code. A release can contain unchanged tool versions.

The skill must have `name: aid-<id>` and a focused `description` in YAML
frontmatter. Use `{{AID_COMMAND}}` in command examples; the installer binds it
to the installed executable. Explain when to use the tool, required inputs,
output meaning, failures, and one useful example. Do not claim a result proves
more than it does, or authorize actions the user has not requested.

## 4. Prove the contract

Add focused tests for normal inputs, incomplete evidence, failures, and any
mutation boundaries. Mock network calls and use temporary homes for installer
tests. Do not require real credentials or make billable calls in the test suite.
Keep timeouts finite. Source must build natively wherever Go and its required
platform toolchain are available; avoid fixed Termux paths, `/tmp`, POSIX-only
shell commands, and architecture assumptions in application logic.

```sh
go test ./...
go run ./cmd/aid-build
./dist/AID --list
./dist/AID --version --json
./dist/AID.<id> --help
```

Run relevant supported-platform checks. Compilation on another target is not
runtime validation. The local builder discovers every `tool.json`, builds all
binaries on the current device, and verifies their version contracts.

## 5. Assess whether it belongs in AID

Good tools answer bounded, recurring questions using concrete evidence:
`AID.diff` estimates named properties of a supplied diff; a log classifier could
route known failure patterns while preserving an unknown category.

Poor tools hide a broad agent loop behind a command, claim a probability proves
code is safe, silently approve merges, or use a model for an exact deterministic
operation. Improve the scope before adding such a tool.

Finish with runnable examples, validation evidence, and any unresolved limits.
Publishing a release and installing into real agent clients are separate actions
from implementing and testing a tool.
