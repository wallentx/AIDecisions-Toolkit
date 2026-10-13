# AID.ci design intake

## Task and contract

Agents repeatedly gather the same GitHub Actions run, job, step, annotation, log, and artifact evidence before diagnosing CI. `AID.ci` turns a workflow run reference into a bounded schema-version-1 snapshot. It reports evidence and completeness; it does not decide root cause, retry work, download archives, call a model, manage authentication, or mutate GitHub.

Realistic inputs are `AID.ci --repo octo/example --run 123 --attempt 2 --json` for an exact rerun and `AID.ci https://github.com/octo/example/actions/runs/123/job/456 --text` for one focused job. The first returns all jobs in attempt 2 and failure-focused detail. The second verifies job 456 against a pinned attempt and returns no other job logs.

## Identity and source scope

Attempt 0 performs one read of `repos/O/R/actions/runs/ID` to resolve the latest attempt. Every identity and job request after that uses `repos/O/R/actions/runs/ID/attempts/N` or its `/jobs` child. An explicit attempt never uses the mutable latest-run endpoint. A job URL must occur in the fully paginated exact-attempt job list.

The snapshot rereads the exact attempt at the end and compares run ID, attempt, workflow ID, head SHA, event, status, conclusion, and timestamps. Drift and an ongoing run make evidence incomplete. Independent API calls are never described as atomic.

For diagnostic conclusions (`failure`, `cancelled`, `timed_out`, `action_required`, `startup_failure`, or `stale`), logs are fetched only by job IDs proven to belong to the selected attempt. A strict `check_run_url` parser validates the expected API host and repository, reconstructs the annotation endpoint, and checks the current check-run ID, head SHA, status, and conclusion before pagination. Current annotations can differ from historical state and carry `historical_attribution: unverified`.

Artifacts come from the run-wide artifacts endpoint. Only metadata is returned with `scope: workflow_run_all_attempts` and `attempt_attribution: false`; the API cannot attribute those records to an attempt. The tool never follows signed downloads or opens ZIP data.

## Bounds, redaction, and output

Hard maxima are 100 items per page, 20 pages, 500 jobs, 20 job logs, 200 requests, 16 MiB retained normalized evidence, 500 annotations, 500 artifacts, a 16 MiB prefix per job log (2 MiB by default), 100,000 processed lines per log, 32 MiB per metadata response (8 MiB by default), 5 excerpts per job, 3 context lines, 60 lines per excerpt, 2,000 characters per returned line, and 16 MiB encoded output (128 KiB by default). Exact pagination rejects duplicate IDs where the API provides them and changing or mismatched `total_count` values. Annotation objects have no stable upstream ID, so their current check-run `annotations_count` plus bounded pages establish completeness while page/index/ordinal identify records only inside one snapshot. Limits stop further requests rather than first fetching unbounded data.

Each log reports `available`, `empty`, `unavailable`, `permission_denied`, `truncated`, `skipped`, or the reserved `expired` state. GitHub log 404 is `unavailable` with reason `expired_or_unavailable`; it cannot prove expiry. HTTP 410 is `unavailable` with reason `gone`. Artifact expiry is separate API metadata.

Before a selected log line is returned, the complete line is redacted for authorization headers, bearer values, common GitHub/cloud tokens, URL credentials, secret query parameters, and common secret assignments. Then terminal escapes/control characters are removed and the character cap is applied. This defense is intentionally documented as non-exhaustive and preserves platform masking.

Output defaults to JSON for pipes and text for a terminal. `--json` and `--text` override shared configuration. If final JSON exceeds its budget, whole excerpts and lower-priority collections are omitted while JSON remains valid; affected sources become incomplete and `output.omitted_items` records counts.

## Failures and acceptance

Exit 0 means complete evidence for the declared selection scope, not a diagnosis. Exit 3 means valid partial, capped, drifted, or ongoing evidence. Exit 2 means invalid arguments/configuration or a job outside the selected attempt. Exit 1 means fatal collection/render/write failure. Safe errors never include raw `gh` stderr or API bodies.

Native tests use fake API responses to verify latest-attempt pinning, explicit old attempts, job membership, failed cleanup and expected-lint steps, matrices, pagination/duplicates/count drift, annotation permissions, unavailable logs, empty/canceled/skipped logs, prefix limits, unknown step mapping, deterministic excerpt fallback, redaction order, output-budget validity, 64-bit IDs, configuration precedence, interspersed flags, dry-run, version, and exit codes. The implementation uses portable Go and makes no assumptions about POSIX paths or `/tmp`.
