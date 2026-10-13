# Develop AIDecisions-Toolkit tools

Run `AID --build` and give its output to the coding agent. The maintained
[intake template](../internal/intake/template.md) covers the repeated task,
contract, implementation, tests, and examples of appropriate tools.

The completed [AID.discover intake](../tools/discover/design.md) shows a
deterministic evidence-preparation tool with a separate agent review step.
Its [usage guide](discover.md) covers source adapters, bounded JSON, and privacy.

```text
AIDecisions-Toolkit/
|-- cmd/
|   |-- AID/main.go
|   |-- AID.ci/main.go
|   |-- AID.diff/main.go
|   |-- AID.discover/main.go
|   |-- AID.pr/main.go
|   |-- AID.runtime/main.go
|   `-- aid-build/main.go
|-- tools/diff/
|   |-- tool.json
|   |-- SKILL.md
|   |-- cli.go
|   |-- checks.json
|   `-- python/
|-- tools/pr/
|   |-- tool.json
|   |-- SKILL.md
|   |-- design.md
|   `-- ...
|-- internal/
|   |-- config/
|   |-- decisions/
|   |-- catalog/
|   |-- builder/
|   |-- installer/
|   |-- github/
|   |-- evidence/
|   |-- update/
|   `-- intake/template.md
`-- VERSION
```

Each tool owns its implementation, manifest, skill, tests, and examples. Shared
transport and infrastructure live under `internal/`. The launcher automatically
embeds `tools/*/tool.json` and `tools/*/SKILL.md`; no manual tool registry edit is
needed. A new manifest's package must be `./cmd/AID.<id>`.

`AID.pr` and `AID.ci` share GitHub reference validation, fixed-argument `gh`
transport, source status, and bounded I/O. CI log reads add an in-memory raw GET
operation without changing PR JSON request behavior. Attempt selection and log
processing remain CI-specific; neither tool imports the other.

`tool.json` is the source of truth for each tool's version. Root `VERSION`
identifies the whole tested release. Changing shared code bumps the versions
of tools whose observable behavior changes; unrelated tools keep their version.
The updater compares suite releases, which record a compatible set of tools.

```sh
go test ./...
go vet ./...
python -m unittest discover -s tools/diff/python/tests -v
go run ./cmd/aid-build
./dist/AID --version --json
```

Tests must use fake credentials, mocked services, and temporary client homes.
Do not make billable requests or modify installed client guidance as part of
the automated test suite. Test behavior and failure boundaries, not wording.

A release must contain this source layout and a `VERSION` matching its stable
GitHub tag (`vMAJOR.MINOR.PATCH`). Run the native builder before publishing;
it discovers tools, builds each executable plus AID, and verifies their version
contracts. Users build the release locally; cross-platform binary assets are
not required. Publishing requires a selected repository and a separate release
action; implementing a tool does not publish it.

The installer is a fresh Go implementation of the interaction pattern inspected
in `wallentx/codex-reflect` (`reflect init`). Current integrations use provider
skill directories instead of copying Reflect's history or hook machinery.
