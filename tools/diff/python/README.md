# Python example

`assess_change.py` preserves the importable, standard-library-only Decisions
example. Its CLI supports diff files, stdin, staged changes, custom checks,
request previews, and JSON output. It reads `OPENAI_API_KEY` only.

Use `AID.diff` for suite configuration, `api_key_file`, automatic JSON output,
and the versioned agent workflow. This Python example deliberately does not
implement `~/.config/AID/config.yml` or the suite installer and updater.

From the repository root:

```sh
python tools/diff/python/assess_change.py --example --dry-run
python -m unittest discover -s tools/diff/python/tests -v
```

Keep the example with the parent `checks.json` and `examples/` files, which it
loads relative to its own path.
