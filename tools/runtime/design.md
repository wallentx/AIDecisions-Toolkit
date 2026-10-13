# AID.runtime v1 boundary

`AID.runtime` accepts one explicit regular file. It supports ELF64,
little-endian AArch64 executables and shared objects on Linux or Android hosts.
It uses Go's in-process `debug/elf` parser after bounded structural checks, then
normalizes only file-header, program-header, dynamic-table, and needed-library
fields. Production collection does not start a subprocess.

The collector hashes and checks the target identity before and after
inspection. These checks are a best-effort boundary, not an atomic filesystem
snapshot. Interpreter bytes are read from the already opened target using the
validated `PT_INTERP` offset and a 4096-byte bound.

Host observation is opt-in through a strict 16 KiB manifest with at most 16
clean, absolute, unique paths. Availability uses `stat` on those paths and
records `present`, `missing`, or `unreadable`; `lstat` separately identifies
symlink kind. A dangling symlink is missing for availability purposes. These
observations do not resolve the loader's dependency graph.

Output is deterministic compact JSON with fixed field and array ordering. The
hard limit is 4096 bytes. Declared dynamic arrays may be omitted only with
explicit counts, `source.complete=false`, and exit 3. Mandatory provenance or
manifest observations that cannot fit fail clearly rather than producing
truncated JSON.

`elf.omission_counts_known` and `output.omission_count_known` distinguish exact
zero or exact omission counts from an early metadata-bound stop where the
remaining count is deliberately unknown.
