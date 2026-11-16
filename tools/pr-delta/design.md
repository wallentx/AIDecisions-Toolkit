# AID.pr-delta design intake

## Task and contract

On a re-review, an agent needs to know what changed in a pull request since a
given reviewer last reviewed it. With that evidence it can re-approve an
unchanged rebase quickly, or focus verification on changed hunks and on the
reviewer's open threads. `AID.pr-delta` collects that evidence:

```text
PR reference + reviewer (default: authenticated gh user) [+ --since-commit]
  -> baseline commit (reviewer's latest submitted review, or the flag)
  -> head relation, PR-diff hunk comparison, rebased-commit equivalence
  -> reviewer's threads, CI on head, stack state, head stability
  -> versioned JSON or a short text summary; the agent decides what to verify
```

The tool is evidence-only. It never posts, resolves, approves, requests
changes, or judges code. It makes no model calls and has no API-key setting.
All logic is deterministic: compare-status mapping, unified-diff hunk parsing,
and multiset comparison of hunk bodies.

## Realistic inputs

| Input | Returned evidence and next action |
| --- | --- |
| `AID.pr-delta https://github.com/acme/api/pull/42 --json` | The authenticated user's last review commit, `head_relation: rebased`, `pr_diff.verdict: moved_only`, and no unresolved threads. The agent can re-approve after confirming CI. |
| `AID.pr-delta --repo acme/api --pr 42 --reviewer octo --since-commit 7fe9e675 --text` | `fast_forward` with one new commit and two changed files with hunk headers. The agent verifies those hunks and the threads where the author replied. |

## Inputs and side effects

Exactly one PR reference is accepted: an HTTPS PR URL whose host equals
`--hostname`, or `--repo OWNER/REPO --pr N`. `--reviewer` is a GitHub login
(an app login may carry `[bot]`; matching ignores that suffix and case because
GraphQL omits it). `--since-commit` takes 7 to 40 hex characters; a short SHA
is resolved through `repos/{o}/{r}/commits/{sha}`. Configuration comes from
`tools.pr-delta` in the shared AID config; CLI flags override it.

Only read-only `gh api` calls with fixed arguments are made, through
`internal/github` (`Client.REST` and `Client.GraphQL`). No cache is written.
`--dry-run` validates arguments and prints the plan without running `gh`.

## Algorithm

1. Read PR metadata (start). Resolve the reviewer (`GET user` when not given).
2. Baseline: `--since-commit`, otherwise the reviewer's latest review by
   `submitted_at` whose state is not `PENDING` (all review pages). No review
   yields `baseline: null`, warning `baseline_missing`, no delta, exit 3.
3. Stack: `base_is_default_branch`; otherwise one GraphQL query gives
   `base_branch_exists` from `repository.ref(qualifiedName:
   "refs/heads/{base_ref}")` (null sets `base_branch_missing`) and `base_pr`
   from `repository.pullRequests(headRefName: {base_ref}, states: [OPEN,
   CLOSED, MERGED])` with a head in the same repository (open first, else the
   highest number). Branch names travel as GraphQL variables, so names that are
   not valid REST path segments, such as ones containing `+` or `#`, work
   without path escaping.
4. Head relation from `compare/{baseline}...{head}`: `identical`, `ahead`
   (`fast_forward`, with all `new_commits`), `diverged` (`rebased`), `behind`.
5. PR diff: three-dot compares `compare/{base_sha}...{baseline}` and
   `compare/{base_sha}...{head}`. The base is selected as follows, and a branch
   base is resolved once to a commit through GraphQL `ref(qualifiedName:)`
   (`pr_diff.base.sha`). Both compares and the base-history walk use that SHA,
   so a base update during collection cannot mix snapshots.
   - Open PR: the current `base_ref`; if that branch is gone, the repository
     default branch, with warning `base_branch_missing`.
   - Closed or merged PR: the base SHA GitHub recorded at close
     (`recorded_base_sha`). A merged head is contained in its base branch, so a
     branch-based compare would be empty.
   - If either side is `identical`/`behind` (already contained in the base), the
     verdict is `unknown` with warning `head_contained_in_base` or
     `baseline_contained_in_base`.
6. Base rewrite check. When the two sides have different merge bases, the base
   history is walked (`commits?sha={base_sha}`) back to the baseline merge base.
   Baseline-side commits whose author email, author date, and subject reappear
   there are old base commits, for example a rebased stack. If they form a
   prefix that ends at the parent of the first remaining commit, the baseline
   side is recompared from the last such commit (`baseline_base_realigned`).
   Interleaved matches give `unknown` (`baseline_base_ambiguous`). History
   beyond the page bound keeps the plain three-dot result and adds
   `baseline_base_unverified`.
7. File comparison by path. Each patch is split into hunks, and a hunk's
   identity is its body with the `@@` header ignored. A file is `unchanged` when
   its patch text is identical, `moved_only` when the multisets of hunk bodies
   match but offsets differ, and `changed` otherwise. `added` and `removed` mean
   the file entered or left the PR diff. A missing patch (binary or too large)
   or an unparseable patch makes the file and verdict `unknown`, including for a
   file that entered or left the diff. A pure rename (no patch, no changes) is
   an empty patch. A compare returning 300 files may be truncated and gives
   `unknown`.
8. Rebased commits: the current PR commits whose patch identity has no
   equivalent among baseline-side commits. Identical SHAs match without a
   request. Other commits need `commits/{sha}` patches, capped by
   `--max-commit-patches`. A file with an empty patch, such as a pure rename,
   adds its status, previous filename, and blob SHA to the identity, so renames
   from different sources do not match. Merge commits, missing patches, or the cap give
   `new_commits: null` plus `new_commits_note`.
9. Threads: GraphQL `reviewThreads` (all pages, nested comment pages for
   matching threads) whose first comment author is the reviewer.
   `author_replied_after` is true only when the PR author (`pr.author`, from
   the pull request's `user`) commented after the reviewer's last comment.
   `last_reply_by` and `last_reply_excerpt` describe the latest comment after
   it from anyone, so replies from other reviewers and bots stay visible
   without counting as the author's reply.
10. CI on head: `check-runs?filter=latest` and the combined `status`, counted
    into success, failure, pending, skipped, neutral, cancelled, and other.
11. Resolve a branch base again; a different SHA adds warning `base_moved` and
    fails `pr_diff` with `snapshot_changed`. Then read PR metadata again; a
    moved head sets `head_stable.stable=false` and warning `head_moved`.

## Output schema (schema_version 1)

```text
schema_version, reference, reviewer, reviewer_source, started_at, finished_at,
complete,
pr{number,state,base_ref,base_sha,head_ref,head_sha,author,default_branch,
   review_decision,merge_state},
baseline{source,review_id,review_state,commit,submitted_at} | null,
head_relation, new_commits[{sha,message_first_line}] | null, new_commits_note,
pr_diff{verdict,reason,base{ref,sha,source},baseline_side,head_side,counts,
        files[{path,status,changed_hunks[{old_header,new_header}],
               changed_hunks_omitted,reason}]} | null,
threads{total,unresolved,unresolved_with_author_reply,
        items[{thread_id,first_comment_id,path,line,is_resolved,is_outdated,
               author_replied_after,last_reply_by,last_reply_excerpt,
               last_reply_clipped}]},
ci{head_sha,counts{...},non_success[{name,kind,conclusion}],non_success_omitted},
stack{base_is_default_branch,base_branch_exists,base_pr{number,state,merged}|null},
head_stable{start,end,stable},
sources{name:{complete,captured_at,requests,items,error,limitations}},
warnings[], output{max_bytes,truncated,omitted_items,retry_hint}
```

`changed_hunks` removes hunks whose bodies appear on both sides, then pairs
the remaining baseline-side (`old_header`) and head-side (`new_header`) hunks
in order. Either header may be empty. Files are ordered changed, added,
removed, unknown, moved_only, unchanged, so output trimming drops unchanged
files first.

## Bounds, failures, and exit codes

Defaults are 10 pages per source, 150 requests (the last one or two are
reserved for the end-of-run head and base stability reads), 40 commit patches, 20 hunks per file, 50 non-success CI
names, 280-character reply excerpts, 8 MiB per response, and 128 KiB of output.
Excerpts are whitespace-collapsed and marked `last_reply_clipped`. Clipping an
excerpt does not make the result incomplete; use `AID.pr` for full comment
bodies. An output cap trims CI names, then threads, then commits, then files.
It records `output.omitted_items` and marks the result and the sources that
produced the omitted items incomplete: `ci_checks` or `ci_statuses` by record
kind, and `commit_equivalence` for a rebase's `new_commits`, otherwise
`head_relation`.

`complete` is true only when every source used is complete, a baseline exists,
the head relation is known, the PR-diff verdict is not `unknown`, and neither
the head nor the base moved. Exit 0 means complete. Exit 3 means valid evidence
that is incomplete, has no baseline, has an unknown verdict, was capped, or saw
the head or base move. Exit 2 means invalid usage or configuration. Exit 1 means rendering or
writing failed. Source errors carry only safe codes from `internal/github`.

## Exclusions

The tool does not judge whether changes are correct, does not read file
contents beyond compare patches, does not compute a range-diff of commit
contents, and does not query fork head repositories for third-party check
runs. It does not detect base rewrites that leave no matching author
metadata, such as squashed base commits; those give a conservative `changed`.

## Acceptance examples

- A clean rebase onto a moved base produces `rebased`, `moved_only`,
  `new_commits: []`, and exit 0.
- A follow-up commit gives `fast_forward`, `new_commits` with that commit,
  `changed` files with hunk headers, and exit 0.
- A reviewer with no submitted review gives `baseline: null`, no compare
  requests, warning `baseline_missing`, and exit 3.
- A binary file change gives the file `unknown`, verdict `unknown`, and exit 3.
- A base branch that moves during collection gives warning `base_moved` and
  exit 3.
- A stacked PR whose base PR was rebased and merged gives
  `baseline_base_realigned` plus `base_branch_missing`, and a verdict that
  reflects only the PR's own commits.

Native tests use a fake transport and need no credentials or network.
