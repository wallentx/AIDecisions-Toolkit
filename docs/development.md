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
|   |-- AID.pr-delta/main.go
|   |-- AID.review-queue/main.go
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
|   |-- prdelta/
|   |-- mcp/
|   |-- evidence/
|   |-- update/
|   `-- intake/template.md
`-- install.sh
```

Each tool owns its implementation, manifest, skill, tests, and examples. Shared
transport and infrastructure live under `internal/`. The launcher automatically
embeds `tools/*/tool.json` and `tools/*/SKILL.md`; no manual tool registry edit is
needed. A new manifest's package must be `./cmd/AID.<id>`.

`AID.pr`, `AID.pr-delta`, `AID.review-queue`, and `AID.ci` share GitHub
reference validation, fixed-argument `gh` transport, source status, and bounded
I/O. CI log reads add an in-memory raw GET operation without changing PR JSON
request behavior. Attempt selection and log processing remain CI-specific; no
tool imports another. The pull-request delta comparison lives in
`internal/prdelta`, which `AID.pr-delta` and `AID.review-queue` both call.

## Adding a Decisions provider adapter

`internal/decisions` owns canonical `Request`, `Question`, and `Answer` types,
the `Evaluator` interface, bounded `net/http` transport, and native provider
adapters (`openai.go`, `typesafe.go`, `gemini.go`, `openrouter.go`, and
`litellm.go`). To add another Decisions provider:

1. Verify the provider's official wire endpoint, authentication header, request schema, response schema, refusal or safety-error behavior, and question/level limits.
2. Add a `Provider` constant in `internal/decisions/types.go` and register its `ProviderInfo` (`DefaultModel`, `DefaultEndpoint`, `APIKeyEnv`) in `providerRegistry` and `adapterFor` inside `internal/decisions/client.go`.
3. Implement `providerAdapter` (`preview` and `decide`) in `internal/decisions/<provider>.go`, translating canonical `predicate`, `choice`, and `score` questions into the native wire format, returning `*UnsupportedError` for unsupported capabilities, and using `postJSON` so timeouts, redirect refusal, 4 MiB response bounds, and credential redaction stay uniform.
4. Add `providers.<id>` defaults in `internal/config/default.yml` only when needed; `internal/config` automatically validates registered providers and isolates `<APIKeyEnv>` and `providers.<id>.api_key_file` per provider.
5. Add offline `httptest` coverage and `testdata/` fixtures in `internal/decisions/providers_test.go`, `internal/config/config_test.go`, and `tools/diff/cli_test.go`, then update the compatibility matrix in `docs/usage.md`.

A manifest may declare an optional `collector` block (name, description, fixed
`--json` arguments, `workspace_flag`, `ttl_seconds`, `timeout_seconds`). The
stdio MCP server in `internal/mcp`, started with `AID --mcp`, runs only those
collectors and the fixed `AID.pr-delta` call. See [mcp.md](mcp.md).

`tool.json` is the source of truth for each tool's version. Git release tags
(`vMAJOR.MINOR.PATCH`) identify each published suite release, and
`cmd/aid-build` resolves `RELEASE_TAG` (or the latest reachable `v*` Git tag)
into `internal/buildinfo.Version` via `-ldflags`. Changing shared code bumps
the versions of tools whose observable behavior changes; unrelated tools keep
their version. The updater compares published GitHub release tags and builds
the release commit locally with that tag stamped in.

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

CI and Release workflows integrate `wallentx/gh-actions/composite/actions-toolbox`
for runner diagnostics, GitHub metadata, and semantic-version increment
calculation (`GH_NEXT_PATCH`, `GH_NEXT_MINOR`, and `GH_NEXT_MAJOR`). CI runs on
pushes to `main` and pull requests across Linux, macOS, and Windows using Go
1.27.2 (minimum lane: 1.26.9) and Python 3.14.8. See
[Go's support policy](https://go.dev/doc/devel/release) and
[Python lifecycle data](https://endoflife.date/python).

To publish a release, bump affected `tools/*/tool.json` manifests on `main` and
dispatch the `Release` workflow (`workflow_dispatch`) with `bump` set to
`patch`, `minor`, or `major` (or pass an explicit `version` tag such as
`v0.2.0`):

```sh
gh workflow run release.yml -f bump=patch
```

The `Release` workflow resolves the next SemVer tag through `actions-toolbox`,
runs the Go, minimum-Go, and Python gates, builds and attests native archives
across Linux, macOS, and Windows, generates the source archive and
`SHA256SUMS`, creates the GitHub release (using `docs/releases/vX.Y.Z.md` when
present or generated changelog notes from `.github/release.yml`), and uploads
all release assets. Publishing a release directly in GitHub also triggers the
same asset build and upload pipeline. Hosted Linux builds disable CGO; Termux's
local build keeps its native default.

The installer is a fresh Go implementation of the interaction pattern inspected
in `wallentx/codex-reflect` (`reflect init`). Current integrations use provider
skill directories instead of copying Reflect's history or hook machinery.
