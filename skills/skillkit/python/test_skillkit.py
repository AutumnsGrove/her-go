"""Tests for the Python skillkit module.

Run with: python -m pytest test_skillkit.py -v
Or simply: python test_skillkit.py
"""

import io
import json
import os
import sys
import unittest
from unittest.mock import patch

import skillkit


class TestOutput(unittest.TestCase):
    """Tests for output() — JSON encoding to stdout."""

    def test_dict(self):
        buf = io.StringIO()
        with patch.object(sys, "stdout", buf):
            skillkit.output({"echo": "hello"})
        self.assertEqual(json.loads(buf.getvalue()), {"echo": "hello"})

    def test_list(self):
        buf = io.StringIO()
        with patch.object(sys, "stdout", buf):
            skillkit.output([1, 2, 3])
        self.assertEqual(json.loads(buf.getvalue()), [1, 2, 3])

    def test_nested(self):
        buf = io.StringIO()
        data = {"results": [{"title": "Test", "score": 0.95}]}
        with patch.object(sys, "stdout", buf):
            skillkit.output(data)
        self.assertEqual(json.loads(buf.getvalue()), data)

    def test_unicode(self):
        """Non-ASCII characters should not be escaped."""
        buf = io.StringIO()
        with patch.object(sys, "stdout", buf):
            skillkit.output({"text": "café ñ 日本語"})
        raw = buf.getvalue()
        # ensure_ascii=False means literal unicode, not \u escapes
        self.assertIn("café", raw)
        self.assertIn("日本語", raw)


class TestError(unittest.TestCase):
    """Tests for error() — JSON error + exit."""

    def test_error_writes_json_and_exits(self):
        buf = io.StringIO()
        with patch.object(sys, "stdout", buf), \
             self.assertRaises(SystemExit) as ctx:
            skillkit.error("something broke")
        self.assertEqual(ctx.exception.code, 1)
        result = json.loads(buf.getvalue())
        self.assertEqual(result["error"], "something broke")


class TestLog(unittest.TestCase):
    """Tests for log() — stderr logging."""

    def test_log_writes_to_stderr(self):
        buf = io.StringIO()
        with patch.object(sys, "stderr", buf):
            skillkit.log("debug message")
        self.assertEqual(buf.getvalue().strip(), "debug message")


class TestParseArgsStdin(unittest.TestCase):
    """Tests for parse_args() in stdin JSON mode."""

    def test_all_fields(self):
        schema = {
            "query": str,
            "limit": (int, 5),
            "verbose": (bool, False),
        }
        stdin_data = '{"query": "cats", "limit": 10, "verbose": true}'
        with patch.object(sys, "stdin", io.StringIO(stdin_data)), \
             patch.object(sys.stdin, "isatty", return_value=False):
            args = skillkit.parse_args(schema)
        self.assertEqual(args["query"], "cats")
        self.assertEqual(args["limit"], 10)
        self.assertEqual(args["verbose"], True)

    def test_defaults_applied(self):
        schema = {
            "query": str,
            "limit": (int, 5),
        }
        stdin_data = '{"query": "dogs"}'
        with patch.object(sys, "stdin", io.StringIO(stdin_data)), \
             patch.object(sys.stdin, "isatty", return_value=False):
            args = skillkit.parse_args(schema)
        self.assertEqual(args["query"], "dogs")
        self.assertEqual(args["limit"], 5)

    def test_empty_object(self):
        schema = {
            "query": str,
            "limit": (int, 5),
        }
        stdin_data = '{}'
        with patch.object(sys, "stdin", io.StringIO(stdin_data)), \
             patch.object(sys.stdin, "isatty", return_value=False):
            args = skillkit.parse_args(schema)
        self.assertIsNone(args["query"])
        self.assertEqual(args["limit"], 5)

    def test_invalid_json_exits(self):
        schema = {"query": str}
        stdin_data = '{not valid}'
        buf = io.StringIO()
        with patch.object(sys, "stdin", io.StringIO(stdin_data)), \
             patch.object(sys.stdin, "isatty", return_value=False), \
             patch.object(sys, "stdout", buf), \
             self.assertRaises(SystemExit):
            skillkit.parse_args(schema)


class TestParseArgsCLI(unittest.TestCase):
    """Tests for parse_args() in CLI flag mode."""

    def test_all_flags(self):
        schema = {
            "query": str,
            "limit": (int, 5),
        }
        with patch.object(sys, "stdin", io.StringIO("")), \
             patch.object(sys.stdin, "isatty", return_value=True), \
             patch.object(sys, "argv", ["skill", "--query", "cats", "--limit", "10"]):
            args = skillkit.parse_args(schema)
        self.assertEqual(args["query"], "cats")
        self.assertEqual(args["limit"], 10)

    def test_defaults(self):
        schema = {
            "query": str,
            "limit": (int, 5),
            "score": (float, 0.8),
        }
        with patch.object(sys, "stdin", io.StringIO("")), \
             patch.object(sys.stdin, "isatty", return_value=True), \
             patch.object(sys, "argv", ["skill", "--query", "dogs"]):
            args = skillkit.parse_args(schema)
        self.assertEqual(args["query"], "dogs")
        self.assertEqual(args["limit"], 5)
        self.assertAlmostEqual(args["score"], 0.8)

    def test_empty_pipe_falls_back_to_flags(self):
        """Empty stdin pipe should fall back to CLI flags."""
        schema = {
            "query": str,
            "limit": (int, 5),
        }
        with patch.object(sys, "stdin", io.StringIO("")), \
             patch.object(sys.stdin, "isatty", return_value=False), \
             patch.object(sys, "argv", ["skill", "--query", "fallback"]):
            args = skillkit.parse_args(schema)
        self.assertEqual(args["query"], "fallback")
        self.assertEqual(args["limit"], 5)


class TestHTTPClient(unittest.TestCase):
    """Tests for http_client() — proxy-aware opener."""

    def test_returns_opener(self):
        opener = skillkit.http_client()
        self.assertIsNotNone(opener)
        # Should have an open method.
        self.assertTrue(callable(getattr(opener, "open", None)))

    def test_proxy_from_env(self):
        """When HTTP_PROXY is set, the opener should include a proxy handler."""
        with patch.dict("os.environ", {"HTTP_PROXY": "http://proxy:8080"}):
            opener = skillkit.http_client()
        # Check that a ProxyHandler is in the handler chain.
        handler_types = [type(h).__name__ for h in opener.handlers]
        self.assertIn("ProxyHandler", handler_types)

    def test_no_proxy(self):
        """When no proxy env vars are set, opener should still work."""
        with patch.dict("os.environ", {}, clear=True):
            # Clear both proxy vars.
            os.environ.pop("HTTP_PROXY", None)
            os.environ.pop("HTTPS_PROXY", None)
            opener = skillkit.http_client()
        self.assertIsNotNone(opener)


if __name__ == "__main__":
    unittest.main()
