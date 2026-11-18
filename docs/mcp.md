# AID MCP server design

## Purpose

An LLM client should be able to ask AID for current state, such as "do I have
pull requests waiting for review", without knowing which executable answers it.
AID works like node_exporter: each tool can declare a cheap collector, and one
server reports every collector's latest result on request.

```text
client --(MCP stdio)--> AID --mcp --(exec, --json)--> AID.<tool> collectors
                          |
                          `-- per-collector cache (TTL, as_of)
```

## Launch

`AID --mcp` runs a stdio MCP server. Running `AID --init` automatically
registers the `aid` server (bound to the absolute installed `AID` executable
path) alongside the per-tool `SKILL.md` files for every selected client (Codex,
Claude Code, Cursor, Gemini CLI, Antigravity, OpenCode, and GitHub Copilot),
while preserving unrelated settings and foreign MCP servers. Manual client
configuration uses the same arguments:

```json
{"mcpServers": {"aid": {"command": "/path/to/AID", "args": ["--mcp"]}}}
```

The server speaks MCP's JSON-RPC over stdio with a small hand-written
implementation: `initialize`, `tools/list`, `tools/call`, `roots/list` (as a
client request) and `ping`. Hand-writing it keeps the module's dependency set
unchanged. If the protocol surface grows past these methods, the official Go
SDK is the replacement.

## Collectors

A tool opts in through its manifest. The server reads embedded manifests, as
the launcher already does, and runs the installed executable for each one.

```json
{
  "id": "review-queue",
  "command": "AID.review-queue",
  "collector": {
    "name": "github_review_queue",
    "description": "Open pull requests waiting on the user's review, new versus re-review.",
    "args": ["--json"],
    "workspace_flag": "--workspace",
    "timeout_flag": "--timeout",
    "ttl_seconds": 60,
    "timeout_seconds": 60
  }
}
```

- The server appends `workspace_flag <root>` when it knows the client's
  workspace root.
- The server stops a run after `timeout_seconds`. When the manifest names a
  `timeout_flag`, the server also passes that flag a shorter budget: the
  deadline minus 15%, with the margin held between 0.5 and 10 seconds (51
  seconds for a 60-second deadline). The child then stops its own requests
  and prints a partial result with exit 3 before the server's deadline. An
  explicit flag takes precedence over `timeout_seconds` in the AID config, so
  MCP runs use the manifest's budget. `pr_delta` runs get `--timeout 110`
  under a 120-second deadline in the same way.
- Each run's stdout must be the tool's JSON result; the server never parses
  text output.
- Exit codes keep their meaning: 0 complete, 3 incomplete. Exit 1 or 2, a
  timeout, or invalid JSON becomes a collector error in the result.

Tools without a `collector` block are not run by the server.

## MCP tools exposed

| MCP tool | Arguments | Returns |
| --- | --- | --- |
| `stats` | `refresh` (bool) | Every collector's latest result envelope: `{name, as_of, age_seconds, cached, exit_code, complete, error, summary}`. `summary` is the tool's own `summary` object when present. |
| `collector` | `name`, `refresh` | One collector's full JSON result in the same envelope. |
| `pr_delta` | `repo`, `pr`, optional `reviewer`, `since_commit` | Runs `AID.pr-delta --json` once (not cached). |

`stats` is the cheap first call. A client then asks for one collector or one
PR's delta when it needs detail.

## Workspace root

The root passed to collectors comes from, in order: the MCP `roots/list`
response when the client supports roots (first `file://` root that names an
existing directory), then the server process's working directory. On Windows
the server accepts `file:///C:/dir`, `file://localhost/C:/dir`, and UNC roots
such as `file://server/share/dir`. A `notifications/roots/list_changed` that
arrives while a `roots/list` request is unanswered makes the server ask again
before it uses the reply. Clients usually start stdio servers in the
project directory, so the second rule covers clients without roots support.
The envelope records which source was used.

## Caching

Results are cached in memory per collector and per workspace root for
`ttl_seconds`. `refresh: true` bypasses the cache. Two concurrent requests for
the same collector share one run.

## Cancellation

`notifications/cancelled` cancels the named request, and the server sends no
response for it. A `pr_delta` run, or a collector run that no other request
shares, is stopped: the child receives an interrupt (a kill on Windows) and is
killed if it is still running 2 seconds later. A shared collector run keeps
going while another request still waits on it. Canceling the server, for
example with Ctrl-C, cancels every request and returns even while stdin stays
open. Nothing is written to disk, and nothing runs
between requests: the server is pull-based, like a scrape.

## Safety

- The server only runs executables named by embedded manifests, with fixed
  arguments from the manifest plus validated values (`repo` matches
  `owner/name`, `pr` is a positive integer, `reviewer` and `since_commit` match
  GitHub's character sets).
- No shell is involved; arguments are passed as an argv array.
- Collector output is data. The server forwards it without interpreting text
  fields, and clients should treat PR titles and thread excerpts as untrusted.

## Passive signal

An MCP server answers only when a client calls it. For a signal that appears
without asking, a client-side hook or status line can run
`AID.review-queue --text --no-delta` and show the count. That configuration
lives in the client, outside this repository.

## Tests

- JSON-RPC framing and the method set, against canned client messages.
- Manifest collector discovery, including tools without a collector block.
- Cache TTL, `refresh`, and request coalescing, with a fake clock and a fake
  executable.
- Error envelopes for exit codes 1, 2 and 3, a timeout, and invalid JSON.
- Cancellation of a request, of one waiter on a shared run, and of the server
  while stdin stays open.
- `roots/list_changed` during a `roots/list` exchange, and file URI parsing
  for Linux, macOS, and Windows forms.
- Argument validation that rejects shell metacharacters and malformed values.

## Usage

Build the suite and keep `AID` next to its tool executables. The server looks
for each collector's executable in the directory of the running `AID` binary
first, then on `PATH`.

```sh
go run ./cmd/aid-build
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stats","arguments":{}}}' \
  | ./dist/AID --mcp
```

The server accepts protocol versions 2025-11-25, 2025-06-18, 2025-03-26 and
2024-11-05, and answers any other requested version with 2025-11-25. Clients on
2025-06-18 or later also receive the envelope as `structuredContent`. Each
`tools/call` result carries the envelope as JSON text. A collector failure or
an invalid argument sets `isError: true` with an `error` object whose `code` is
one of `failed` (exit 1), `usage` (exit 2), `timeout`, `invalid_json`,
`output_limit`, `exec_failed`, `cancelled` (the server shut down during the
run), or `invalid_arguments`. An unknown MCP tool name
is a JSON-RPC error.

Every envelope also carries `command` and, for collectors that take a
workspace, `workspace` with the `root` and its `source` (`roots` or `cwd`).
`AID --config PATH --mcp` passes the configuration path to collectors through
`AID_CONFIG`. Diagnostics go to stderr, and stdout carries only protocol
messages.
