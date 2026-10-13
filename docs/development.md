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
contracts. CI also packages native builds for Linux, macOS, and Windows.
Termux users build locally with Android's native toolchain.

The first published release resets the suite and all tools to `0.1.0`. Earlier
local snapshot versions are development history. Future suite and tool versions
remain independent under the version contract above.

CI runs on pushes to `main` and pull requests: formatting, version contracts,
Go tests/vet, Python tests, native builds, and archive packaging. It also tests
the minimum Go version. CI uses Go 1.27.2 (minimum lane: 1.26.9) and Python
3.14.8, verified supported on 2026-10-13. Go supports its two newest major
releases; Python 3.14 receives security support through October 2030. See
[Go's support policy](https://go.dev/doc/devel/release) and
[Python lifecycle data](https://endoflife.date/python).

To publish a release, update `VERSION` and the default in
`internal/buildinfo/version.go`, bump affected tool manifests, and write
`docs/releases/vMAJOR.MINOR.PATCH.md`. Commit and push that source before tagging:

```sh
python .github/scripts/verify-version.py --tag vMAJOR.MINOR.PATCH
git tag -a vMAJOR.MINOR.PATCH -m 'AIDecisions-Toolkit vMAJOR.MINOR.PATCH'
git push origin vMAJOR.MINOR.PATCH
```

The tag workflow requires a matching stable version, reruns the complete CI
workflow, and publishes only after every job passes. Release assets contain
native ZIPs, a source archive, and `SHA256SUMS`. Native ZIPs contain binary
checksums and `build-info.json` recording the source commit, tool versions,
Go version, platform, and workflow URL. Hosted Linux builds disable CGO;
Termux's local build keeps its native default.

The installer is a fresh Go implementation of the interaction pattern inspected
in `wallentx/codex-reflect` (`reflect init`). Current integrations use provider
skill directories instead of copying Reflect's history or hook machinery.
