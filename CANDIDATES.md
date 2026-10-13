# AIDecisions-Toolkit candidate shortlist

The first published release resets all versions to `0.1.0`. Suite versions
below describe earlier local development snapshots, not published releases.

Updated 2026-10-13. This combines the earlier discovery and the recent three-day
scan. The earlier scan requested 365 days and observed July 10 through October
13: 2,541 records, 128 sessions, and 53 project directories. The recent scan
covered October 10 through October 13: 245 records, 13 sessions, and eight
project directories. These windows overlap; their counts must not be added.
History coverage was user/error records, not full command traces. Savings remain
unmeasured. Retries, copied conversations, and revisits were not counted as
independent incidents.

## Approved for design and implementation intake

Both candidates passed independent review, and the user authorized both
implementations on 2026-10-13. `AID.pr` version `0.1.0` shipped locally in suite
`0.4.0`; `AID.ci` version `0.1.0` is implemented in suite `0.5.0`.
See [PR usage](docs/pr.md), [CI usage](docs/ci.md), and their design intakes in
`tools/pr/design.md` and `tools/ci/design.md`. Runtime implementation and the
remaining proposal backlog are recorded below.

| Priority | Candidate | Input -> output | Earlier evidence | Recent evidence | Estimated effort |
| --- | --- | --- | --- | --- | --- |
| 1 | `AID.pr` | PR URL or repository/number -> current metadata, head/base identities, checks, files, paginated review threads and comments, source completeness | 33 supporting sessions across 17 directories | Four distinct PR contexts across two projects | 1-2 engineer-days |
| 2 | `AID.ci` | Run/job reference and exact attempt -> head/event identity, failed jobs/steps, annotations, bounded log excerpts and artifact metadata | Nine explicit troubleshooting sessions across eight directories | Three independent failure contexts across three projects | 2-3 engineer-days |

### AID.pr

Reuse `gh` authentication and API access. Existing PR-remediation skills already
describe the review procedure; this tool owns reliable evidence assembly rather
than reviewing code, posting comments, resolving threads, or approving merges.
Collect all pages of threads and nested comments, record head identities before
and after collection, and expose truncation and source errors. A stable head
does not make GitHub's independently changing sources an atomic snapshot.

Proposed invocation: `AID.pr --repo owner/repo --pr 42`.

Illustrative result:

```json
{"head_sha":"abc123","head_unchanged":true,"failed_checks":1,"unresolved_threads":2,"complete":true}
```

Validate against captured PR examples and fixtures for pagination, changing
heads, partial GraphQL responses, denied access, and output limits. Compare
manual collection completeness and command count before claiming savings.
Historical live state is not a replay unless evidence was saved at capture time.

### AID.ci

`gh run view --log-failed`, `actionstat`, and `get-artifact` already cover
individual operations. The remaining gap is an attempt-specific evidence packet
with annotations, source references, output bounds, and explicit unavailable-log
states. Core collection is deterministic; root-cause judgment stays with the
agent. Artifact support is bounded metadata only, without downloads, comparison,
reruns, cancellation, or remediation.

Proposed invocation: `AID.ci --repo owner/repo --run 123 --attempt 2`.

Illustrative result:

```json
{"attempt":2,"head_sha":"abc123","failed_jobs":[{"name":"build","step":"compile"}],"logs":"available","complete":true}
```

Validate retained historical contexts and synthetic matrix jobs, reruns,
post-checkout failures, nonblocking lint, expired logs, permission failures,
unknown steps, empty sources, and truncation. One recent incident includes
pasted analysis; this supports recurrence of a failure request, not its claimed
diagnosis or a measured manual-work cost.

## Implemented third candidate

`AID.runtime` passed independent validation on 2026-10-13 with a narrower
contract: Linux/Android AArch64 ELF static facts plus explicit host/path
observations, compact versioned JSON, and preserved unknowns. A disposable
existing-command recipe produced 604-byte and 626-byte packets from 4,890 and
4,638 bytes of inspector output, and handled a non-ELF control explicitly.
Command count was unchanged; no speed or runtime-compatibility claim is made.

The user subsequently authorized implementation. `AID.runtime` version `0.1.0`
is implemented in suite `0.6.0`, using Go's in-process ELF reader rather than
an external inspector. See [usage](docs/runtime.md) and
[validation evidence and v1 boundaries](docs/runtime-validation.md).
Broad runtime diagnosis and cross-platform inspection remain outside v1.

## Remaining candidate status

| Priority | Candidate | Narrow scope | Required proof |
| --- | --- | --- | --- |
| 3 | `AID.runtime` | Implemented AArch64 ELF static-fact packet in suite 0.6.0 | Preserve the tested fields, bounded output, and explicit unknown boundary |
| 4 | `AID.benchcmp` | Compare benchmark output through shared offline adapters | Demonstrate a useful common contract beyond existing analyzers |
| 5 | `AID.releasecheck` | Component manifests, checksums, and provenance | Establish a common manifest contract while reusing existing release/APK validators |
| 6 | `AID.sessioncheck` | Session and lock-state diagnostics | Verify the gap beyond `codex doctor --json` before depending on lock internals |

## Deferred and rejected leads

| Lead | Disposition | Reason |
| --- | --- | --- |
| Artifact-drift comparator | Rejected as a standalone tool | Current evidence favors repository-specific regression tests |
| Log-bundle comparison | Unvalidated | No inspected helper compares bundles, but independent recurrence of that comparison operation was not established |
| Generic runtime configuration collection | Fold into runtime validation | Device state, preferences, environment commands, and automatic reports already cover much of the operation |
| Release announcement synchronization | Deferred | Evidence comes from one continuing release campaign; external writes also require separate scope |
| Broad documentation claim review or handoff generation | Deferred | Open-ended agent work lacks a sufficiently narrow additional executable contract |

## Shared implementation direction

Use existing tools and AID infrastructure first. Share concrete GitHub request,
reference-validation, bounded-input, and evidence-completeness mechanics where
their semantics match. Keep PR collection separate from future CI collection;
neither tool should import another tool's implementation. Reuse the existing
suite catalog, configuration, builder, installer, and version contracts.

Avoid introducing a generic workflow engine, speculative adapters, or changing
working tools solely to make an abstraction look universal. Add shared code when
it has a clear responsibility now and a compatible future consumer. Every source
must retain identity, capture time, completeness, and failure information.

## Evidence references

Earlier PR examples: 2026-07-31 session
`019fb67e-643c-7793-b036-ea244d2c1146` and 2026-10-11 session
`01a125a5-7f28-73f1-aa3e-8ad2127fd4c7`. Earlier CI examples: 2026-09-22 session
`01a0c75d-a5d1-7fc1-9e65-094d30d5ca94` and 2026-09-27 session
`01a0dda6-1f28-7de1-ae8e-b6fc80a13382`.

Recent frozen snapshot:
`f3bc66bf116ee5a58695be22e9a25733b1b3fcfa9cfaad2c70784f358aa7a485`.
PR evidence includes `evidence-a536230e67ce141c9253c1c5`,
`evidence-ceafac978ce953363f0f939b`, `evidence-0894d1c3d774df33a98376bf`, and
`evidence-413e68cde70348ab8f1afb07`. CI evidence includes the first PR record,
`evidence-8cac6dcefda1b49cb76e8ab5`, and
`evidence-1bf661c7a0f4437375d59a37`. Cache retention may eventually remove the
snapshot; these references are not a promise of permanent archival storage.
