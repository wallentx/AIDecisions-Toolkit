# AIDecisions-Toolkit

The `AID` command suite lets LLM agents offload recurring work and return
structured answers. **AID.diff** evaluates code changes with configurable
probability checks.
**AID.discover** prepares cached history evidence for the current agent to assess
recurring operations that could become tools, without extra model calls.
**AID.pr** collects bounded, read-only GitHub pull-request evidence with source
completeness and freshness checks.
**AID.ci** collects attempt-specific GitHub Actions failure evidence, including
bounded log excerpts and explicit source provenance.
**AID.runtime** inspects AArch64 ELF files as data and returns bounded static
facts, provenance, and explicitly requested host/path observations.

## Install a release

Download the native ZIP matching your OS and architecture from
[GitHub Releases](https://github.com/wallentx/AIDecisions-Toolkit/releases).
Extract it, keep all six executables together, and run `AID --init` to install
agent skills. Archives include checksums and build provenance.

Termux users build from source using the commands below. `AID.runtime` inspects
files only on Linux and Android hosts; its other platform builds return an
explicit unavailable result.

## Build on this device

Run `./install.sh` to build and install the complete suite to `~/.local/bin`.
The script requires Go and prints a PATH setup command if needed. To choose
another directory, run `INSTALL_DIR=/absolute/path/to/bin ./install.sh`.

Run these commands from the `AIDecisions-Toolkit` directory:

```sh
go run ./cmd/aid-build
./dist/AID --list
./dist/AID.diff --example --dry-run
```

Use a currently supported Go toolchain (language minimum: Go 1.26). Each device
builds native binaries locally. On Windows, use `dist\AID.exe` and
`dist\AID.diff.exe`. On Termux, use the native Go/C toolchain and its default CGO
setting. Running the built executables needs no Go. Discovery requires Reflect
for native history, optionally Python for a `.py` helper; importing normalized
scan JSON needs neither dependency.

Keep `AID` and its tool executables together. Add their directory to PATH, or build into
your preferred binary directory with `go run ./cmd/aid-build --out DIRECTORY`.

## Suite commands

| Command | Result |
| --- | --- |
| `AID -b`, `AID --build` | Print the new-tool intake template for an agent or user |
| `AID -v`, `AID --version` | Show suite and individual executable versions |
| `AID -l`, `AID --list` | List tools and descriptions |
| `AID -i`, `AID --init` | Open the agent-client installer |
| `AID -u`, `AID --update` | Fetch release source, build locally, replace installed binaries |
| `AID -h`, `AID --help` | Show usage |

`--build` starts the design intake; `go run ./cmd/aid-build` compiles the code.
`--list` and `--version` accept `--json`.

## Use AID.diff

```sh
AID.diff --example --dry-run
AID.diff --staged --json
git diff --no-ext-diff --no-textconv | AID.diff
AID.diff --diff change.diff --context 'Public REST API'
```

Set `OPENAI_API_KEY`, or put its file path in
`~/.config/AID/config.yml` under `global.api_key_file`. Live requests send the
selected diff and context to OpenAI. Dry runs need no credentials and make no
API call. Results default to JSON for agents/pipes and a table in a terminal.

Use `AID --init` to select clients and review the per-tool skills to install.
The picker follows Reflect's provider-selection and review/apply pattern.
Nothing is installed until the plan is applied.

## Prepare discovery evidence

```sh
AID.discover --all-projects --days 365 --dry-run
AID.discover --input scan.json --all-projects --days 365
```

Discovery needs no API key. It returns bounded JSON and snapshot references;
the current agent judges candidates and obtains independent review. See
[discovery usage and privacy](docs/discover.md).

## Collect pull-request evidence

```sh
AID.pr --repo OWNER/REPO --pr 42 --json
AID.pr https://github.com/OWNER/REPO/pull/42 --text
AID.pr --repo OWNER/REPO --pr 42 --dry-run
```

The tool reuses your installed `gh` authentication and only makes read-only API
requests. It returns files, checks, statuses, reviews, comments, review threads,
and source/freshness metadata; it does not post or approve anything. Inspect
incomplete sources and freshness drift before relying on a result. See
[pull-request evidence](docs/pr.md).

## Collect CI failure evidence

```sh
AID.ci --repo OWNER/REPO --run 123 --attempt 2 --json
AID.ci https://github.com/OWNER/REPO/actions/runs/123 --json
AID.ci --repo OWNER/REPO --run 123 --attempt 2 --dry-run
```

The tool reuses `gh` and the same shared GitHub transport as `AID.pr`. It pins
one run attempt, collects jobs, annotations and bounded log excerpts, and keeps
run-wide artifact metadata separate. Missing logs and truncated evidence remain
visible. See [CI evidence and limitations](docs/ci.md).

## Inspect static runtime facts

```sh
AID.runtime --json /absolute/path/to/binary
AID.runtime --host-manifest host.json --json /absolute/path/to/binary
```

The tool reads AArch64 ELF files in-process and launches no inspection
subprocess. It returns declarations, hashes, explicitly allowlisted host/path
observations, and unknowns in a packet capped at 4 KiB. See
[static runtime facts and manifest format](docs/runtime.md).

[Configuration and usage](docs/usage.md) |
[Adding tools and releases](docs/development.md) |
[Release notes](docs/releases/v0.1.0.md) |
[Pull-request evidence](docs/pr.md) |
[CI evidence](docs/ci.md) |
[Static runtime facts](docs/runtime.md) |
[Intake template](internal/intake/template.md)
