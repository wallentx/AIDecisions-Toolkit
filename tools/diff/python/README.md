# Python example

`assess_change.py` preserves the importable, standard-library-only OpenAI
`/v1/decisions` reference example. Its CLI supports diff files, stdin, staged
changes, custom checks, request previews, and JSON output, reading
`OPENAI_API_KEY` from the environment.

Use the native `AID.diff` executable for multi-provider Decisions support
(`openai`, `typesafe`, `gemini`, `openrouter`, and `litellm`),
`~/.config/AID/config.yml` configuration, isolated `api_key_file` settings,
automatic JSON output, and the installed agent skill workflow.

From the repository root:

```sh
python tools/diff/python/assess_change.py --example --dry-run
python -m unittest discover -s tools/diff/python/tests -v
```

Keep the example alongside the parent `checks.json` and `examples/` files,
which it loads relative to its own path.
