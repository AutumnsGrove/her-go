"""Shared utilities for her-go Python skills.

Skills are standalone scripts that receive JSON on stdin, do work,
and write JSON to stdout. This module handles the boilerplate:
argument parsing, structured output, and HTTP requests.

This is the Python equivalent of the Go skillkit package. The API
is intentionally similar so switching between languages feels natural.

Usage:
    from skillkit import parse_args, output, error, log, http_client

    args = parse_args({"query": str, "limit": (int, 5)})
    results = do_search(args["query"], args["limit"])
    output({"results": results})

Zero dependencies — pure stdlib. This is deliberate: skillkit should
never add install overhead to a skill. If you need requests, put it
in the skill's own pyproject.toml.
"""

import argparse
import json
import os
import sys
import urllib.request

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------


def output(value):
    """Write a JSON-encoded value to stdout and exit.

    This is how a skill returns its result to the harness. The harness
    reads stdout, parses the JSON, and passes it back to the agent.

    In Go: skillkit.Output(v)
    """
    json.dump(value, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")
    sys.stdout.flush()


def error(msg):
    """Write a JSON error to stdout and exit with code 1.

    The harness checks for the "error" key to detect skill failures.
    Same as Go's skillkit.Error — stop immediately rather than
    producing partial or corrupt output.
    """
    output({"error": msg})
    sys.exit(1)


def log(msg):
    """Write a message to stderr for debug logging.

    The harness captures stderr for logging, but it never reaches
    the agent's context window — log freely without worrying about
    token costs.

    In Go: skillkit.Logf(...)
    """
    print(msg, file=sys.stderr, flush=True)


# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------


def parse_args(schema):
    """Parse arguments from stdin JSON or CLI flags.

    When the harness runs a skill, it pipes JSON to stdin. When a developer
    tests manually, they pass CLI flags. This function handles both
    transparently — same as Go's skillkit.ParseArgs.

    The schema is a dict mapping argument names to types or (type, default)
    tuples:

        schema = {
            "query": str,                # required string
            "limit": (int, 5),           # optional int, default 5
            "verbose": (bool, False),    # optional bool, default False
        }

    Returns a dict with parsed values.

    Stdin JSON mode: tries to read JSON from stdin if it's piped (not a
    terminal). Falls back to CLI flags if stdin is empty or a terminal.

    CLI flag mode: builds argparse flags from the schema. Required args
    (no default) become required flags; optional args become optional flags.
    """
    # Try stdin JSON first.
    if not sys.stdin.isatty():
        data = sys.stdin.read().strip()
        if data:
            try:
                parsed = json.loads(data)
                if not isinstance(parsed, dict):
                    error("input JSON must be an object")
                return _apply_defaults(parsed, schema)
            except json.JSONDecodeError as e:
                error(f"invalid input JSON: {e}")

    # Fall back to CLI flags.
    return _parse_cli_flags(schema)


def _apply_defaults(parsed, schema):
    """Fill in missing keys with defaults from the schema."""
    result = {}
    for name, spec in schema.items():
        if isinstance(spec, tuple):
            typ, default = spec
            raw = parsed.get(name, default)
        else:
            typ = spec
            raw = parsed.get(name)

        # Type coercion for values that came from JSON (already typed)
        # or from defaults. None stays None for required fields.
        if raw is not None:
            try:
                result[name] = typ(raw)
            except (ValueError, TypeError):
                result[name] = raw
        else:
            result[name] = raw

    return result


def _parse_cli_flags(schema):
    """Build an argparse parser from the schema and parse sys.argv."""
    parser = argparse.ArgumentParser(description="skill")

    for name, spec in schema.items():
        if isinstance(spec, tuple):
            typ, default = spec
            parser.add_argument(f"--{name}", type=typ, default=default)
        else:
            typ = spec
            parser.add_argument(f"--{name}", type=typ, required=True)

    args = parser.parse_args()
    return vars(args)


# ---------------------------------------------------------------------------
# HTTP client
# ---------------------------------------------------------------------------


def http_client():
    """Return a urllib opener that respects HTTP_PROXY env vars.

    Skills use this for all outbound HTTP requests. The proxy is set
    by the harness for untrusted skills — this client picks it up
    automatically via environment variables.

    In Go: skillkit.HTTPClient()

    Returns an opener you can use like:
        opener = http_client()
        resp = opener.open(req)

    For most cases, use the convenience functions http_get/http_post instead.
    """
    proxy_url = os.environ.get("HTTP_PROXY") or os.environ.get("HTTPS_PROXY")

    if proxy_url:
        proxy_handler = urllib.request.ProxyHandler({
            "http": proxy_url,
            "https": proxy_url,
        })
        return urllib.request.build_opener(proxy_handler)

    return urllib.request.build_opener()


def http_get(url, headers=None, timeout=30):
    """GET a URL and return the response body as bytes.

    Convenience wrapper around http_client() for simple GET requests.
    Raises urllib.error.URLError on failure.

        body = http_get("https://api.example.com/data")
        data = json.loads(body)
    """
    req = urllib.request.Request(url)
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)

    opener = http_client()
    with opener.open(req, timeout=timeout) as resp:
        return resp.read()


def http_post(url, data, headers=None, timeout=30):
    """POST JSON to a URL and return the response body as bytes.

    Convenience wrapper for the common pattern of posting JSON and
    reading a JSON response.

        body = http_post(
            "https://api.example.com/search",
            data={"query": "cats"},
            headers={"Authorization": "Bearer xxx"},
        )
        results = json.loads(body)
    """
    if isinstance(data, (dict, list)):
        body = json.dumps(data).encode()
    elif isinstance(data, str):
        body = data.encode()
    else:
        body = data

    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)

    opener = http_client()
    with opener.open(req, timeout=timeout) as resp:
        return resp.read()
