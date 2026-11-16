# List pull requests waiting on review

`AID.review-queue` lists open GitHub pull requests that have a pending review request for one reviewer. It tags each one as new work or a re-review, and for a re-review it runs the same comparison as `AID.pr-delta`, baselined on the reviewer's latest submitted review. It uses the authentication already configured for `gh`, makes no model calls, and never requests, dismisses, approves, or comments.

```sh
AID.review-queue --text
AID.review-queue --owner FarmGPU --json
AID.review-queue --workspace ~/src --no-delta --text
AID.review-queue --owner acme --reviewer octo --max-prs 10 --json
```

`--reviewer` defaults to the authenticated `gh` user, which costs one extra API call. `--no-delta` skips the comparisons, so the whole run is one search call. `--dry-run` infers the scope, prints the search query, and contacts nothing.

## Owner scope

The tool searches `is:pr is:open archived:false review-requested:LOGIN` plus one `user:OWNER` qualifier per owner. The first rule that yields an owner wins, and `scope.source` records it:

| Source | Rule |
| --- | --- |
| `flag` | One or more `--owner` values (repeatable or comma-separated). |
| `config` | `tools.review-queue.owners` in the AID config. |
| `workspace_repo` | The `origin` remote of the git repository containing `--workspace` (default: the current directory). |
| `workspace_children` | The `origin` owner holding a strict majority among git repositories one level below the workspace. |
| `all` | No owner. The search covers every repository the reviewer can see, and the result carries warning `scope_all_repositories`. |

Majority inference lists at most 4096 workspace entries. A larger workspace, or a listing cut short by a read error, infers no owner from children; the scope falls through to `all` and the result carries warning `workspace_children_truncated`.

Inference reads `.git/config` files directly. It follows `gitdir:` files and `commondir`, so linked worktrees and submodules resolve to their repository's config. It accepts HTTPS, `ssh://`, and `git@host:owner/repo` remotes whose host equals `--hostname`, and it ignores remotes on any other host. A `.git` directory without `HEAD` is not treated as a repository. Inference runs no `git` command and no network call.

## Result

Schema version 1 returns `reviewer`, `scope`, `as_of`, `summary`, `items`, `truncated`, `sources`, `complete`, and `warnings`. Each item has `repo`, `number`, `title`, `author`, `url`, `draft`, `requested_via`, `updated_at`, `kind`, and `delta`.

- `kind` is `new` when the reviewer has no submitted review on the pull request, `re_review` when they have one, and `unknown` when GitHub omitted the review history.
- `requested_via` is `user` for a direct request. It is `team:<slug>` when exactly one team was requested and the reviewer was not requested directly. Any other case is `unknown`, because team membership is not read.
- `delta` is `null` for new items and with `--no-delta`. For a re-review it holds `baseline_state`, `baseline_commit`, `head_relation`, `verdict`, `changed_files`, `threads_unresolved`, `threads_unresolved_with_author_reply`, `ci` (`success`, `failure`, `pending`, `skipped`, `neutral`, `cancelled`, `other`, copied from pr-delta's counts; `other` includes `timed_out`), `complete`, and any pr-delta `warnings` or `incomplete_sources`.
- `summary.re_review_unchanged` counts re-reviews whose verdict is `unchanged` or `moved_only` with a complete delta.

Text output prints one line per pull request:

```text
AID.review-queue reviewer octo; scope FarmGPU (workspace_children)
3 waiting: 1 new, 2 re-review (1 unchanged)
FarmGPU/fgpu_ansible#498 re-review rebased, diff unchanged, 0 open threads, CI 8/8
FarmGPU/fgpu_ansible#523 re-review rebased, diff changed, 1 files changed, 0 open threads, CI 8/8
FarmGPU/docs#12 new by dev: Document the intake flow
complete: true
```

`CI p/n` counts passing checks over checks that can block a merge; skipped and neutral checks are left out of `n`. Failing, cancelled, and other checks (such as `timed_out`) are named after it, as in `CI 8/9, 1 other`.

Titles are untrusted GitHub data. Run `AID.pr-delta` on one pull request when the changed hunks or thread replies are needed.

## Completeness and exit codes

| Exit | Meaning |
| --- | --- |
| 0 | Complete queue, including an empty one (`summary.total: 0`) |
| 3 | Valid JSON, but the search failed, a delta was incomplete, `--max-prs` truncated the list, or output was capped |
| 2 | Invalid arguments or configuration |
| 1 | Fatal output failure |

A failed search sets `sources.search.status` to the safe error kind (for example `permission_denied`) and leaves `items` empty, so it stays distinguishable from an empty queue. A failed delta marks only that item's `delta.complete: false`; the other items are still returned. `sources.delta` reports how many deltas were computed, incomplete, and skipped.

## Limits and configuration

Configuration follows the suite precedence: a command-line option overrides `tools.review-queue`, which overrides `global`.

| Option | Default | Purpose |
| --- | ---: | --- |
| `--max-prs` | 25 | Pull requests listed (1 to 1,000); more sets `truncated` |
| `--delta-concurrency` | 4 | Re-review comparisons run in parallel |
| `--title-limit` | 200 | Characters kept from each title |
| `--max-output-bytes` | 64 KiB | Encoded JSON budget; items are trimmed from the tail when exceeded |
| `--max-response-bytes` | 8 MiB | One `gh api` response |
| `--timeout` | global or tool config | Total collection time in seconds |

Each re-review costs about ten API calls: pull request metadata at start and end, the review list, two or three compares, review threads, check runs, and statuses. The AID MCP server runs this tool as the `github_review_queue` collector and caches the result; see [mcp.md](mcp.md).
