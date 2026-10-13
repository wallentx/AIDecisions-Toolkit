# Inspect static runtime facts

`AID.runtime` produces a small fact packet for AArch64 ELF files on Linux/Android hosts.
It uses Go's standard ELF reader in-process and launches no inspection subprocess.
The packet describes declarations and observations, with explicit unknowns for
dependency resolution, child-launch environment, execution policy, and actual
launch success.

```sh
AID.runtime /absolute/path/to/binary --json
AID.runtime --file /absolute/path/to/binary --host-manifest host.json --json
AID.runtime /absolute/path/to/binary --dry-run
```

The binary is opened as data. No `ldd`, target `--version`, runtime probe,
wrapper, shell command, or external inspector is invoked. LLVM was used as the
validation baseline and remains an independent comparison tool, not a runtime
dependency.

## Explicit host and path observations

Host/path inspection is opt-in. Supply a JSON manifest such as:

```json
{
  "schema_version": 1,
  "observe_host": true,
  "paths": [
    "/system/bin/linker64",
    "/lib/ld-linux-aarch64.so.1"
  ]
}
```

The manifest allows only these fields, at most 16 absolute unique paths, and at
most 16 KiB of input. It does not expand environment variables or globs.
`observe_host` requests the current host's OS and architecture. The listed
paths permit metadata observations only; their contents are not read.

The declared interpreter remains `unchecked` unless its path is explicitly
allowlisted. Path observations distinguish `present`, `missing`, `unreadable`,
and `unchecked`. Missing is a fact about the inspected filesystem view, not a
verdict that a configured launcher fails. A present interpreter likewise does
not establish that it can load the target.

Without a manifest, host/path observations remain unchecked. The command does
not discover or dump environment variables, inspect other processes, or infer
the environment of a failed child process.

## Facts, provenance, and boundaries

The packet includes file identity/hash, source-tool provenance, ELF format and
architecture, declared interpreter, declared dependencies, and RPATH/RUNPATH
facts. `DT_NEEDED` is not a resolved dependency graph. There is no compatibility
score, automatic fix, loader selection, or launch-success claim.

Input identity and content are checked around inspection. Detected changes
produce incomplete evidence. Those checks are best effort rather than an
atomic filesystem snapshot: an external process could replace and restore a
file between observations.

Go's [standard ELF reader](https://pkg.go.dev/debug/elf) supplies format decoding.
Structural bounds are checked before decoding metadata, including table ranges,
counts, and section-name-table size/compression. Dynamic and string metadata is
bounded; required strings must terminate within their validated table. Extended
numbering and unsupported metadata forms remain explicit outcomes in v1.

Interpreter bytes are read from the validated segment of the opened file.
Dependency declarations preserve their order and duplicates. A dynamic binary
whose section metadata is unavailable is reported as incomplete rather than as
having no dependencies.

## Limits and configuration

| Setting | Default | Maximum |
| --- | ---: | ---: |
| `max_file_bytes` | 512 MiB | 2 GiB |
| JSON packet size | 4 KiB | Fixed at 4 KiB |

`--max-file-bytes` controls the input bound. `--timeout` bounds the operation; new default
configuration sets `tools.runtime.timeout_seconds` to 120 seconds. Flags
override `tools.runtime`, which overrides common `global` settings.
`--config PATH` and `AID_CONFIG` select another configuration file.

The tool's private configuration key is `max_file_bytes`, plus the shared
timeout/output settings. Existing
configuration retains its own global timeout unless explicitly overridden.

Output stays valid JSON when optional evidence is omitted to meet the fixed
budget, and omissions remain explicit. Early metadata limits can prevent an
exact omitted count; `elf.omission_counts_known` and
`output.omission_count_known` then report `false`, and the text summary says
the count is unknown. If required identity/status fields
cannot fit, the command reports an output failure rather than emitting a broken
or misleading packet. Deterministic ordering does not imply that mutable input
files or host observations stay unchanged between captures.

## Exit status

| Exit | Meaning |
| --- | --- |
| 0 | Complete static facts for a supported input |
| 3 | Valid unsupported, malformed, changed, unavailable, or incomplete evidence |
| 2 | Invalid CLI, configuration, or manifest |
| 1 | Internal or output failure |

Use `--json` for evidence and `--text` for a readable summary. Help and version
do not load configuration. Dry-run validates options and the selected manifest,
but does not inspect the target or observe host paths.

The [validation report](runtime-validation.md) records why this narrow scope
was selected. Other architectures, formats, and runtime diagnoses require
separate validation.
