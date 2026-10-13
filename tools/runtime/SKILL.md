---
name: aid-runtime
description: Collect bounded, read-only static facts for one Linux or Android AArch64 ELF executable or shared object, including declared loader and libraries plus optional allowlisted host paths. Use for a compact runtime preflight; it does not execute the target, resolve dependencies, or decide compatibility.
---

# Collect static runtime facts

Use this tool when a runtime investigation needs a small, reproducible fact packet before considering execution or tracing.

```sh
{{AID_COMMAND}} --json /absolute/path/to/program
{{AID_COMMAND}} --host-manifest host.json --text /absolute/path/to/program
```

The optional manifest is strict JSON and is the only way to observe host paths:

```json
{"schema_version":1,"observe_host":true,"paths":["/system/bin/linker64","/lib/ld-linux-aarch64.so.1"]}
```

Read `status`, `complete`, `target.stability`, `provenance.parser`, `source`, and `output` before using the facts. Check `elf.omission_counts_known` and `output.omission_count_known` before treating zero as an exact omission count. Treat `elf.needed` as declarations. A present interpreter path does not prove a successful launch, and a missing path does not account for wrappers or execution hooks. The always-present `unknowns` list names the launch questions this tool does not answer.

Exit 0 means the supported static facts were completely collected within the fixed 4 KiB packet. Exit 3 still emits valid JSON for unsupported or malformed targets, unavailable input, changed input, timeouts, or output omissions. Exit 2 means usage, configuration, or manifest validation failed. Exit 1 means the required packet could not fit or output failed.

`--dry-run` reads and validates configuration and the optional manifest, but it does not read the target or stat allowlisted host paths. The tool uses Go's in-process `debug/elf` parser and never starts a subprocess. It never executes or mutates the target, scans unrelated strings, runs `ldd`, resolves dependencies, dumps the environment, or issues a compatibility verdict.
