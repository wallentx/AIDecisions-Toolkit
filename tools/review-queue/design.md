# AID.review-queue design intake

## Task and contract

An agent is often asked which pull requests are waiting on the user's review,
and then whether each one is new work or a re-review of something already
approved. Today the agent runs a search, then reconstructs each PR's history by
hand. `AID.review-queue` returns that list in one bounded call:

```text
owner scope + reviewer
  -> open PRs with a pending review request for the reviewer
  -> each tagged new or re-review, with the pr-delta verdict for re-reviews
  -> agent picks which PR to open first and how much to verify
```

The tool is read-only. It never requests, dismisses, approves or comments.

## Inputs

| Input | Default | Notes |
| --- | --- | --- |
| `--owner OWNER` (repeatable) | inferred, see below | Organization or user that owns the repositories. |
| `--reviewer LOGIN` | authenticated `gh` user | Review requests to this login, directly or through a team. |
| `--workspace DIR` | current directory | Directory used for owner inference. The MCP server passes the client's root here. |
| `--no-delta` | off | Skip the per-PR delta and return the list only (one search call). |
| `--max-prs N` | 25 | Bound on PRs listed; more sets `truncated`. |
| `--json`, `--text`, `--dry-run`, `--config` | as in AID.pr | |

### Owner inference

The first rule that yields a value wins, and the output records which rule it
was in `scope.source`:

1. `--owner` flags (`flag`).
2. `tools.review-queue.owners` in the AID config (`config`).
3. The `origin` remote of the git repository containing `--workspace`
   (`workspace_repo`).
4. The most common `origin` owner among git repositories one level below
   `--workspace`, when one owner holds a strict majority (`workspace_children`).
   A workspace such as `~/wsrc` full of one organization's clones resolves to
   that organization.
5. No owner. The search then covers every repository the reviewer can see
   (`all`), and the output says so.

Remote URLs are parsed for `github.com` (or the configured host) in HTTPS and
SSH forms. Repositories on other hosts are ignored. Inference reads only local
git config files; it runs no network call.

## Output (JSON, `schema_version` 1)

```json
{
  "schema_version": 1,
  "reviewer": "wallentx",
  "scope": {"owners": ["FarmGPU"], "source": "workspace_children", "workspace": "/home/u/wsrc"},
  "as_of": "2026-11-16T22:00:00Z",
  "summary": {"total": 3, "new": 1, "re_review": 2, "re_review_unchanged": 1, "unknown": 0},
  "items": [
    {"repo": "FarmGPU/fgpu_ansible", "number": 498, "title": "...", "author": "ereinha3",
     "url": "https://github.com/FarmGPU/fgpu_ansible/pull/498", "draft": false,
     "requested_via": "user", "updated_at": "...",
     "kind": "re_review",
     "delta": {"head_relation": "rebased", "verdict": "unchanged", "changed_files": 0,
               "threads_unresolved": 0, "threads_unresolved_with_reply": 0,
               "ci": {"success": 8, "failure": 0, "pending": 0}, "complete": true}}
  ],
  "sources": {"search": {"status": "ok"}, "delta": {"status": "ok", "skipped": 0}},
  "complete": true,
  "warnings": []
}
```

- `kind` is `new` when the reviewer has no submitted review on the PR, and
  `re_review` otherwise.
- `delta` reuses the pr-delta comparison, baselined on the reviewer's latest
  submitted review. It is `null` for `new` items and when `--no-delta` is set.
- `requested_via` is `user` or `team:<slug>`.
- `summary.re_review_unchanged` counts re-reviews whose verdict is `unchanged` or
  `moved_only` with `complete: true`.

Text output prints one line per PR, for example
`FarmGPU/fgpu_ansible#498 re-review rebased, diff unchanged, 0 open threads, CI 8/8`.

## Shared code

The delta comparison moves from `tools/pr-delta` into `internal/prdelta` so
both tools call it without a cross-tool import. `AID.pr-delta` keeps its
contract and version; only the package location changes.

## Failure modes

- Search failure or denied access: `sources.search` carries the error kind,
  `items` is empty, exit 3. This stays distinguishable from an empty queue,
  which is exit 0 with `total: 0`.
- A per-PR delta failure marks that item's `delta.complete: false` and the whole
  result incomplete (exit 3). Other items are still returned.
- Inference that finds no owner is reported as `scope.source: "all"`, which is
  not an error.

## Cost

One search call, plus about three API calls per re-review for the delta
(review list, compare, threads). Five re-reviews cost about 16 calls. The MCP
server caches results; see `docs/mcp.md`.

## Exclusions

The tool does not rank PRs by importance, read diffs for meaning, or post
anything. It does not cover review requests outside GitHub.

## Acceptance examples

| Input | Expected |
| --- | --- |
| `--workspace` is a folder of clones owned by one organization, three open requests, one already approved and since rebased without change. | `scope.source` is `workspace_children`; `summary` shows 1 new and 2 re-reviews, one of them `re_review_unchanged`. |
| `--owner other-org` with no requests. | Exit 0, `total: 0`, `complete: true`. |
| The search call fails with an authentication error. | Exit 3, `sources.search.status` names the error, `items` is empty. |
