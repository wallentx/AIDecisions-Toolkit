import contextlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest

PYTHON_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PYTHON_ROOT))
import assess_change as app

ROOT = Path(__file__).resolve().parents[2]


@contextlib.contextmanager
def mock_api(body, status=200):
    requests = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            requests.append((self.path, dict(self.headers), payload))
            self.send_response(status)
            self.send_header("Location", "/redirected")
            self.end_headers()
            self.wfile.write(body.encode())

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/decisions", requests
    finally:
        server.shutdown()
        thread.join()
        server.server_close()


class APIContract(unittest.TestCase):
    def test_request_and_refusal(self):
        body = '{"answers":[{"name":"behavior","type":"predicate","probability":0.9},{"name":"security","type":"refusal"}]}'
        with mock_api(body) as (endpoint, requests):
            answers = app.assess_change(
                "+ changed", context="public API", api_key="test-key", endpoint=endpoint,
                checks={"behavior": "Behavior?", "security": "Sensitive?"},
            )
        self.assertEqual(answers["behavior"]["probability"], 0.9)
        self.assertEqual(answers["security"]["type"], "refusal")
        self.assertEqual(requests[0][1]["Authorization"], "Bearer test-key")
        self.assertEqual(json.loads(requests[0][2]["input"]), {"diff": "+ changed", "context": "public API"})
        self.assertEqual(requests[0][2]["model"], "gpt-6-luna")

    def test_invalid_answers(self):
        cases = [
            [],
            [{"name": "check", "type": "refusal"}] * 2,
            [{"name": "other", "type": "refusal"}],
            [{"name": "check", "type": "predicate"}],
            [{"name": "check", "type": "predicate", "probability": True}],
            [{"name": "check", "type": "predicate", "probability": 1.1}],
            [{"name": "check", "type": "choice"}],
        ]
        for answers in cases:
            with self.subTest(answers=answers), mock_api(json.dumps({"answers": answers})) as (endpoint, _):
                with self.assertRaises(RuntimeError):
                    app.assess_change("+ x", checks={"check": "Changed?"}, api_key="test-key", endpoint=endpoint)

    def test_http_error_does_not_echo_key(self):
        with mock_api("secret-key-in-error-body", 401) as (endpoint, _):
            with self.assertRaisesRegex(RuntimeError, "HTTP 401") as error:
                app.assess_change("+ x", api_key="secret-key-in-error-body", endpoint=endpoint)
        self.assertNotIn("secret-key", str(error.exception))

    def test_redirect_rejected(self):
        with mock_api("redirect", 307) as (endpoint, requests):
            with self.assertRaisesRegex(RuntimeError, "HTTP 307"):
                app.assess_change("+ x", api_key="test-key", endpoint=endpoint)
            self.assertEqual(len(requests), 1)

    def test_missing_key_and_invalid_inputs(self):
        with self.assertRaisesRegex(ValueError, "OPENAI_API_KEY"):
            app.assess_change("+ x", api_key="")
        for checks in ({}, {"": "question"}, {"name": None}, []):
            with self.subTest(checks=checks), self.assertRaises(ValueError):
                app.build_request("+ x", checks=checks)
        with self.assertRaises(ValueError):
            app.build_request(" \n")
        with self.assertRaisesRegex(ValueError, "printable ASCII") as error:
            app.assess_change("+ x", api_key="secret\nkey")
        self.assertNotIn("secret", str(error.exception))


class CLIContract(unittest.TestCase):
    def commands(self):
        commands = [[sys.executable, str(PYTHON_ROOT / "assess_change.py")]]
        return commands

    def invoke(self, command, *args, input="", cwd=ROOT):
        env = {key: value for key, value in os.environ.items() if key != "OPENAI_API_KEY"}
        return subprocess.run(command + list(args), input=input, text=True, capture_output=True,
                              cwd=cwd, env=env, timeout=10)

    def test_example_and_custom_checks_match(self):
        outputs = []
        for command in self.commands():
            with self.subTest(command=command):
                result = self.invoke(command, "--example", "--dry-run", "--checks", "examples/checks.json", "--context", "public API")
                self.assertEqual(result.returncode, 0, result.stderr)
                payload = json.loads(result.stdout)
                payload["input"] = json.loads(payload["input"])
                outputs.append(payload)
        for payload in outputs:
            self.assertEqual(payload, outputs[0])
            self.assertEqual(len(payload["questions"]), 2)

    def test_stdin_empty_diff_and_missing_key(self):
        for command in self.commands():
            with self.subTest(command=command):
                result = self.invoke(command, "--dry-run", input="+ changed")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(json.loads(result.stdout)["input"])["diff"], "+ changed")
                self.assertEqual(self.invoke(command, "--dry-run").returncode, 2)
                result = self.invoke(command, "--example")
                self.assertEqual(result.returncode, 1)
                self.assertIn("OPENAI_API_KEY", result.stderr)

    def test_conflicting_sources_and_null_checks(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "checks.json"
            path.write_text("null", encoding="utf-8")
            for command in self.commands():
                with self.subTest(command=command):
                    self.assertEqual(self.invoke(command, "--example", "--staged", "--dry-run").returncode, 2)
                    self.assertEqual(self.invoke(command, "--example", "--checks", str(path), "--dry-run").returncode, 2)

    def test_staged_in_unborn_repository(self):
        with tempfile.TemporaryDirectory() as temp:
            subprocess.run(["git", "init", "--quiet", temp], check=True)
            (Path(temp) / "sample.py").write_text("answer = 42\n", encoding="utf-8")
            subprocess.run(["git", "-C", temp, "add", "sample.py"], check=True)
            for command in self.commands():
                with self.subTest(command=command):
                    result = self.invoke(command, "--staged", "--dry-run", cwd=temp)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("+answer = 42", json.loads(json.loads(result.stdout)["input"])["diff"])


if __name__ == "__main__":
    unittest.main()
