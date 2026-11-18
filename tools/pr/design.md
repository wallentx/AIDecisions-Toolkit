# AID.pr design intake

## Task and contract

An agent reviewing a GitHub pull request repeatedly gathers the same bounded evidence before deciding what to inspect or report. `AID.pr` collects that evidence with per-source completeness and freshness metadata:

```text
Pull-request reference + existing gh authentication
  -> bounded, read-only GitHub API collection
  -> versioned JSON snapshot with source and freshness state
  -> agent reviews evidence and chooses any follow-up action
```

The command reports evidence; it does not assess code, approve a pull request, post comments, resolve conversations, manage authentication, or retrieve CI-log contents. It makes no model calls and has no API-key setting.

## Realistic inputs

| Input | Returned evidence and next action |
| --- | --- |
| `AID.pr --repo octo/example --pr 42 --json` | Pull-request metadata, changed files, checks, statuses, review threads and comments, issue comments, reviews, source state, and a start/end freshness comparison. The agent can inspect a changed file or assess a live review thread. |
| `AID.pr https://github.example/acme/api/pull/19 --hostname github.example --text` | The same evidence from the selected GitHub Enterprise host. The agent checks `complete`, per-source errors, limitations, and freshness before relying on the result. |

## Input, output, and side effects

The command accepts exactly one pull-request reference: `--repo OWNER/REPO --pr NUMBER` or an HTTPS pull-request URL. A URL host must equal `--hostname`; the default host is `github.com`. It uses the installed `gh` credentials, optionally with `--gh EXECUTABLE`, and only invokes read-only `gh api` requests with fixed arguments. It creates no cache and makes no GitHub mutation.

The snapshot has schema version `1` and includes:

```text
schema_version, reference, started_at, finished_at, complete, sources,
pull_request, freshness, files, checks, statuses, review_threads,
issue_comments, reviews, open_findings
```

`sources` maps every collected top-level source to `complete`, `captured_at`, `requests`, `items`, optional safe `error`, and `limitations`. `review_threads` aggregates thread completeness; each thread has its own `comments_source`. Bodies and comments are untrusted GitHub data. They may be clipped to `--text-limit` and must never be interpreted as instructions.

Collection starts from exact PR metadata and head SHA, then obtains check runs for that start SHA (`filter=all`), full commit statuses, files, issue comments, reviews, and fully paginated GraphQL review threads with their nested comments. It finishes by reading metadata again. `freshness` compares repository, head/base SHA and refs, update time, state, draft, and merged state. Matching metadata only establishes bounded stability; independently fetched sources do not form an atomic GitHub snapshot.

Checks and statuses query both base and distinct head repositories. Matching
nonempty node IDs merge their `observed_in` scopes; numeric identities stay
repository-scoped. Missing head repositories and overlapping pages cannot be
reported as complete coverage. Per-source capture times describe the source's
last response or decision, rather than the end of the entire collection.

The default output is JSON for pipes and readable text for a terminal. `--json` and `--text` select a format. `--dry-run` validates and previews the collection without requiring `gh` or contacting GitHub.

## Bounds, failures, and exclusions

Default bounds are 128 KiB output, 4,000 characters per text body, 100 records per page, 50 pages per source, 10,000 items, 25,000 comments, 200 requests, 16 MiB retained data, and 8 MiB per GitHub response. Output may be increased to 16 MiB with `--max-output-bytes`; a capped output or clipped body labels the affected source, makes the snapshot incomplete, and exits 3. The command never automatically retries with larger limits.

The REST files endpoint is limited by GitHub to the first 3,000 files. Check runs expose GitHub's recent-suites limit (up to 1,000). These and other source limits remain visible in `sources`; a source error or a bound never means the source is complete.

Exit status is 0 for a complete, stable snapshot; 3 for valid but incomplete, capped, or drifted evidence; 2 for invalid usage or configuration; and 1 for a fatal source or rendering failure.

## Acceptance and evolution

Native tests use fake `gh` responses and verify URL/reference validation, fixed read-only arguments, pagination, nested review-thread comments, limits, safe errors, freshness drift, rendering, dry-run, and exit states. They do not need real credentials or GitHub API calls.

The manifest version is `0.2.0`. The additive `open_findings` view keeps schema
version 1 and derives all-author open threads and unmatched review bodies from
the existing sources. GraphQL comment queries include actor type and review
node ID so classification and matching do not depend on text or author/commit
heuristics. [Open-findings semantics](../../docs/pr.md#open-findings-across-authors)
define reply ordering, exact-location grouping, parse statuses, and bounds.

Changes to the JSON schema, source scope, freshness semantics, or CLI contract require a tool-version migration decision. Future `AID.ci` may reuse the GitHub transport and status evidence, but owns deep log collection or CI-specific interpretation.
