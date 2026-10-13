# AID.runtime validation

Validated on 2026-10-13. **Recommendation: approve a narrow static-preflight
v1 for implementation consideration.** No AID.runtime executable, installed
skill, or production implementation was created during this validation.

The user subsequently authorized the narrow build. It is implemented as
`AID.runtime` 0.1.0 in suite 0.6.0; see [usage](runtime.md). Production uses
Go's in-process ELF reader, so collecting facts launches no subprocess.

The broad runtime-diagnosis idea remains out of scope. The validated surface is
Linux/Android AArch64 ELF facts, an explicit host/path observation, and a compact
versioned packet with uncertainty preserved.

## Two case families

Historical summaries supplied the original failure descriptions; current
retained artifacts and relevant source were inspected read-only. Original
runtime failures were not rerun, and historical binary hashes were unavailable.
This is not proof that either failure still occurs today.

| Case | Current observation | What it can establish | What remains unknown |
| --- | --- | --- | --- |
| Opaque Linux/glibc CLI payload | AArch64 ELF requests `/lib/ld-linux-aarch64.so.1`, which is absent; an alternate packaged glibc loader exists | The declared interpreter path is absent in the inspected filesystem view | Whether the configured wrapper or execution hooks successfully route the actual launch |
| Native Git launched by another program | AArch64 ELF requests the existing `/system/bin/linker64`; declared dependencies and RUNPATH are readable | Binary/loader sanity facts; no architecture or interpreter-path mismatch was found | Historical child `EACCES` required launch-context/trace evidence; the current parent environment does not reconstruct that child |

The first case derives from the binary-only compatibility lessons summarized on
2026-05-27, session `019e6c3e-73dd-7561-9884-887e1dd1fd7c`. The second derives
from the 2026-09-21 child-execution investigation, session
`01a0c482-ab78-7882-9d22-78e9c763a2ca`.

Current source inspection also confirmed that the relevant Git subprocess
helpers replace their child environment with a small locale/configuration list.
That is useful follow-up evidence, not a current reproduction of the old error.

## Existing capabilities and remaining gap

The installed LLVM reader already emits machine-readable ELF metadata:

```sh
llvm-readobj --elf-output-style=JSON --file-header --program-headers \
  --needed-libs --dynamic-table /absolute/path/to/binary
patchelf --print-interpreter /absolute/path/to/binary
```

LLVM documents its [JSON output and inspection flags](https://llvm.org/docs/CommandGuide/llvm-readobj.html).
On this installation, adding `--string-dump=.interp` to the JSON invocation
produced mixed output that did not parse as JSON. The validated recipe kept
interpreter extraction separate. This observation is version-specific, not a
claim that all LLVM versions behave identically.

`file`, `readelf`/`llvm-readelf`, `llvm-readobj`, `patchelf`, and `objdump` are
installed. Existing repository helpers already validate expected architectures
and interpreters, route known loaders, and set wrapper environments. Those
policies remain repository-owned; a new generic collector should not replace
them. Runtime probe/benchmark runners execute targets and were not used here.

The remaining value is a small stable contract combining selected facts,
input provenance, requested-path presence, and explicit unknowns. It is not a
new binary-analysis engine or proof of successful dependency resolution.

## Disposable recipe experiment

The validation used two existing inspection commands per ELF, selected-field
normalization, a file hash, and a path-existence observation. A non-ELF control
used the inspector's failure response. All packets passed explicit field/type
checks, a 4 KiB output ceiling, and canonical JSON round-trip checks.

| Input | Existing command output | Normalized packet | Inspection commands |
| --- | ---: | ---: | ---: |
| Opaque payload | 4,890 bytes | 604 bytes | 2 |
| Native Git | 4,638 bytes | 626 bytes | 2 |
| Non-ELF control | 140 bytes | 183 bytes | 1 |

For the two ELF cases, serialized output was approximately 87% smaller. This
does not establish token-billing savings, faster execution, fewer underlying
commands, or improved diagnosis accuracy. The control packet is larger because
it supplies explicit structure. Single captures including hashing took about
0.88 and 0.20 seconds; these are not comparable performance benchmarks.

The local evidence packet is `dist/runtime-validation.json` (ignored build
artifacts). No target executable was run. No wrapper, binary, device setting,
or global agent guidance was modified.

## V1 acceptance contract

Input is one explicitly selected ELF file plus an allowlisted host/path
manifest. Do not discover or dump the entire environment. The first supported
scope is the AArch64 ELF behavior exercised here; other formats and platforms
need separate validation.

Freeze these fields before implementation:

- Version and declared observation scope; target size/hash and source-tool
  provenance.
- ELF format, machine, bitness, requested interpreter, declared dependencies,
  and RPATH/RUNPATH facts.
- Explicitly scoped host architecture and path observations, distinguishing
  present, missing, unreadable, and unchecked states.
- Unknowns for dependency resolution, child-launch environment, execution
  policy, and actual launch success.
- Bounded canonical output and structured unsupported/malformed-input handling;
  any omitted data must remain explicit.

Treat `DT_NEEDED` as declarations, not resolved libraries. A present interpreter
does not prove it can run the target; an absent declared interpreter does not
prove that a configured wrapper fails. No compatibility verdict, target
execution, full dependency resolver, mutation, wrapper policy, or model call
belongs in this v1.

The disposable experiment checked an explicit field shape, not a formal JSON
Schema validator or a complete production parser. Production acceptance needs
the two ELF fixtures, a malformed/non-ELF fixture, missing/unreadable path
distinction, input-change detection, output limits, and source provenance.

## Independent review

The first review requested revision because standalone value over existing
tools was unproven. After the three-input experiment, the independent reviewer
approved the narrow v1 recommendation with medium-high confidence: compact
normalized evidence and explicit unknowns are a demonstrated benefit despite
unchanged inspection command count. The general runtime-diagnosis proposal and
cross-platform claims remain unvalidated. Building the tool is a separate step.
