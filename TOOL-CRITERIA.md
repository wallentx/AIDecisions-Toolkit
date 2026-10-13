# AIDecisions-Toolkit tool criteria

An ideal AID tool is **a recurring piece of agent work that can become one
dependable command with a clear input/output contract.**

The ideal shape is:

```text
Evidence -> bounded operation -> structured result -> agent's next action
```

It should save repeated investigation, command construction, parsing, or
judgment. It can use deterministic code, an LLM, or both. Repetition within one
valuable workflow is enough; reuse across projects makes it stronger.

## Discovery prompt

Append this prompt to a Reflect history-scan request:

```text
Scan all projects over the last 365 days for opportunities to create
tools in my AIDecisions-Toolkit (`AID`) suite.

An ideal AID tool captures a recurring, bounded operation that agents
currently reconstruct through shell commands, file inspection,
manual parsing, or repeated semantic judgment.

Look for recurring operations inside larger tasks. For example,
“investigate a failed CI run” is broad; “collect failed jobs, extract
relevant errors, and return a classified failure summary” is bounded.

Prioritize candidates that:
- Have identifiable inputs and a useful, testable output contract.
- Return compact structured evidence that informs an agent's next step.
- Reduce repeated tool calls, context consumption, latency, or mistakes.
- Can preserve uncertainty, missing evidence, and failures explicitly.
- Provide value beyond existing tools, scripts, or skills.

Inspect actual tool-call sequences where available, not just repeated
wording in user requests. State any history coverage limitations.
Count retries within one incident as one occurrence.

Use deterministic logic where sufficient. Propose model evaluation
only for the parts that require semantic judgment.

Return up to five ranked candidates. For each, provide:

1. Proposed AID.<name> and a one-sentence input -> output contract.
2. Evidence: independent occurrences, affected projects, and dated
   session references. Distinguish observed counts from estimates.
3. Example invocation, representative JSON output, and the downstream
   action that output would help an agent choose.
4. Existing alternatives, the remaining gap, and whether implementation
   should be deterministic, model-assisted, or hybrid.
5. Expected benefit, implementation/maintenance effort, and a small
   validation experiment using historical examples.

Keep this discovery-only. Have an independent reviewer challenge
recurrence, scope, novelty, and whether a dedicated executable is
justified. Return fewer candidates if the evidence is weak.
```
