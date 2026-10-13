# AID.discover design intake

## Task and contract

Agents repeatedly assemble history scans, normalize records, remove recognizable
private data, inspect recurrence, and compare existing tools before proposing an
executable. `AID.discover` owns deterministic preparation and retrieval:

```text
Scoped Reflect records + criteria + metadata inventory
  -> bounded JSON evidence, coverage, lexical leads, opaque references
  -> current agent judges contracts -> independent reviewer challenges them
```

This is a bounded operation inside the larger discovery task described by
[TOOL-CRITERIA.md](../../TOOL-CRITERIA.md). It reduces repeated preparation;
the current agent supplies semantic judgment. The command does not automatically
discover ready-to-build tools.

## Realistic synthetic inputs

These acceptance scenarios are synthetic, not observations from real history.

| Input | Expected evidence and next action |
| --- | --- |
| Three dated sessions across two projects request failed build job collection and compiler error classification, with several retries for each run. | Dated lexical leads and evidence IDs let the agent verify run identity, count each incident once, and assess a bounded failure-summary contract. |
| Two sessions request dependency release notes already covered by an installed skill; a third describes equivalent intent with different wording. | Metadata yields an overlap hint. The third record remains available through `--records` even without a shared lead. The agent verifies the existing skill and may propose no new tool. |

The [usage guide](../../docs/discover.md) supplies normalized JSON and runnable
commands. Fixture clocks must put example dates inside the requested window.

## Input, output, and side effects

Input is scoped native history through Reflect, or an explicit normalized JSON
array. Native scans require Reflect; imported arrays do not. An explicit `.py`
helper also requires Python. Fixed subprocess arguments request JSON and tool
errors without semantic evaluation. No API key is required.

Output schema version 1 includes `mode`, `cache`, `coverage`, `page`,
`budget_bytes`, and `interpretation`. Lead mode adds criteria, inventory summary,
and lexical leads; record/evidence modes add excerpts. Opaque IDs and continuation
arguments support bounded retrieval. `source_truncated` distinguishes discarded
source text from output paging.

Own persistent writes are limited to cache directories, the local identity salt,
snapshots, and source-cache entries. Reflect reads native history. Inventory reads
only selected-provider skill frontmatter and current AID metadata, plus optional
explicit JSON. Dry runs do not scan or create cache files. Discovery does not
install tools, change guidance, implement candidates, publish, or read credentials.

Malformed JSON and oversized source input fail. Rejected rows, record/text limits,
possible replays, and inventory gaps remain visible. Empty or weak evidence is
not a positive recommendation. Stdout holds JSON; stderr holds progress/errors.
Help/version require no history; documented exit codes distinguish operational
failures from invalid requests or rendering limits.

## Determinism, privacy, and cost

Parsing, filtering, redaction, IDs, replay hints, lexical grouping, metadata
matching, and pagination are deterministic for fixed inputs, salt, and clock.
The executable makes zero model calls, with no per-request model cost. Local cost
is bounded source I/O, preparation, and metadata reads. TTL reuse avoids repeated
native scans but does not detect appended history until expiry or `--refresh`.

Cache files contain sanitized evidence, not raw transcripts. Salted IDs preserve
local correlation. Redaction may miss sensitive information, so the current agent
must screen evidence before creating an aggregate for independent review. Private
cache permissions do not make its contents suitable for sharing. Windows uses
inherited ACLs. Snapshots remain local.

No elapsed-time or token savings are claimed without a baseline. A future
synthetic experiment can compare repeated snapshot retrieval against full scans,
reporting source bytes, response bytes, helper invocations, and elapsed time.

## Acceptance and exclusions

1. Synthetic fixtures verify scope/dates, invalid rows, duplicate/possible replay
   handling, conservative counts, and access to ungrouped records.
2. Redaction fixtures verify recognized secrets/identifiers are removed, aliases
   correlate locally, and raw source bytes never reach cached snapshots.
3. Fake helpers verify fixed arguments, timeouts, failure privacy, cache reuse,
   explicit refresh, and frozen snapshot retrieval.
4. Temporary inventories verify frontmatter-only reads, AID metadata, bounded
   traversal, optional JSON, and incomplete/overlap reporting.
5. CLI/render checks verify bounded valid JSON, page/text continuations, truncation
   markers, invalid combinations, dry-run, and version contracts.

Tests use synthetic data and temporary directories, never real histories,
credentials, installed guidance, or billable services. Cross-compilation does not
establish runtime behavior on another platform.

Excluded: a predefined candidate shortlist, model evaluation inside the command,
exhaustive capability proof, full native command-trace claims, and automatic
implementation or guidance changes. Retention prunes only recognized owned cache
entries within the documented limits. Lexical leads can miss
semantic recurrence and create false matches. Review may reject every candidate.

## Evolution

Tool version `0.1.0` is independent of the suite release. Observable CLI/schema,
normalization, or cache-identity changes need versioned migration or explicit
rejection of incompatible entries. Future adapters/grouping must preserve coverage
and evidence references, pass synthetic fixtures, and demonstrate value before
expanding the contract. Candidate implementation remains separately authorized.
