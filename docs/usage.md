# AIDecisions-Toolkit configuration and usage

Configuration lives at `${XDG_CONFIG_HOME:-~/.config}/AID/config.yml`. AID also
accepts `AID_CONFIG` or `--config PATH`. Initialization creates a default file
if none exists; existing configuration is preserved.

```yaml
version: 1
global:
  api_key_file: "~/.ssh/tokens/codex"
  model: gpt-6-luna
  timeout_seconds: 30
  output: auto
updates:
  repository: "" # Set GitHub owner/repo before checking for releases
  install_dir: "" # Empty means the current AID executable directory
tools:
  diff:
    checks_file: ""
    context: ""
```

The credential above is a **file path**, never a copied API key. Change it to
where your own key is stored. A nonempty `OPENAI_API_KEY` takes precedence.
An export made in a separate agent shell call does not persist to later shell
calls; configuring the key file avoids depending on that process environment.

CLI options override tool settings; tool settings override global settings.
Relative config file references resolve relative to the YAML file, and `~/`
is expanded. A missing default config uses built-in defaults; an explicitly
selected missing or invalid config is an error. Use `output: json`, `text`, or
`auto`. Per-tool sections can include private fields decoded by that tool.

## Diff checks

`--checks FILE` accepts a nonempty JSON object mapping check names to questions.
The default checks cover behavior changes, compatibility, and security-related
code. `tools/diff/examples/checks.json` shows alternate questions.

```sh
AID.diff --diff change.diff --checks tools/diff/examples/checks.json
AID.diff --staged --output text
AID.diff --example --json --timeout 60
```

All checks are independent predicates. Probabilities are model estimates;
refusals remain unanswered. The tool does not approve changes or apply a pass
threshold. Calibrate automation against known examples. Diff/context inputs
are limited to 1 MiB each, and a checks file to 256 KiB. Filter generated or
irrelevant files before sending very large changes.

| Exit | Meaning |
| --- | --- |
| 0 | Preview or all requested answers received; no safety guarantee |
| 1 | File, Git, credential, network, or API failure |
| 2 | Invalid arguments, config, checks, or empty diff |
| 3 | At least one refusal; available answers still printed |

The standalone Python example remains at `tools/diff/python/assess_change.py`.
It uses only the standard library and intentionally does not load suite YAML.
Use the native executable for suite configuration and installation.

## Discovery evidence

See [AID.discover](discover.md) for history-based tool discovery. It prepares
evidence without API credentials or model calls and keeps reusable snapshots in
the local private cache. The current agent applies criteria and obtains
independent review; discovery does not install tools or change guidance.

## Install agent skills

```sh
AID --init
AID --init --list
AID --init --provider codex --dry-run
AID --init --provider codex --provider claude
```

The interactive picker supports arrows or j/k, Space to toggle, Enter to
review, u to update checked installed clients, and q/Esc to cancel. Installed
clients start checked; detected clients are displayed but not preselected.
The review screen defaults to Cancel. Unchecking an installed client plans its
removal; u updates checked clients without removing unchecked ones.

Explicit `--provider` selections allow scripted setup. `--dry-run` prints a
plan without writing skills or config. Local skills are installed for Codex,
Claude Code, Cursor, Gemini CLI, Antigravity CLI, OpenCode, and Copilot CLI.
Only the selected clients are modified. No hooks or external plugins are
required for the current tools.

Installer state is tracked at `${XDG_STATE_HOME:-~/.local/state}/AID/installer.json`.
Owned hashes protect user edits; conflicting unmanaged or edited files block
application. Files and parents must not be symlinks. Skill commands bind to
absolute sibling executables, so place the suite in its intended directory
before installing skills. After moving it, rerun initialization.

## Update on the current device

Choose `updates.repository` before using the updater:

```sh
AID --update --check
AID --update --dry-run
AID --update
```

`--repo owner/repo` overrides configuration. A newer stable GitHub release
triggers a fresh tag checkout in a temporary directory, native builds, version
verification, and replacement of installed binaries. The user's source
checkout is untouched. New tools are discovered from release manifests;
retired registered binaries are removed, while unrelated binaries are left
alone. Replacement failures trigger rollback.

Updates need Git, Go, network access, and a writable install directory. Windows
uses a temporary copied helper to replace executables after the launcher exits;
it reports the result-log path rather than claiming immediate completion.
After updating, rerun `AID --init` to refresh installed skills.

The updater has mock-server/build/replacement tests. No live GitHub release
update has been performed for this new, unpublished suite. Windows compilation
is checked; Windows runtime behavior is not validated on this device.
