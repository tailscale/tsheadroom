"""Integration test against the REAL headroom package.

Skipped automatically unless the interpreter running the tests has headroom-ai
installed. Run it by pointing the suite at such an interpreter, e.g.:

    make test PYTHON=/path/to/venv/bin/python

It runs worker.py with the real compression pipeline (no fake on PYTHONPATH) and
asserts a well-formed, non-trivial result for a large tool-output payload.
"""

import json
import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(__file__)
ROOT = os.path.abspath(os.path.join(HERE, ".."))
WORKER = os.path.join(ROOT, "worker.py")


def headroom_available():
    # Check in an isolated subprocess so the in-process headroom stub installed
    # by test_worker.py cannot give a false positive/negative.
    return subprocess.run(
        [sys.executable, "-c", "import headroom"],
        capture_output=True,
    ).returncode == 0


@unittest.skipUnless(
    headroom_available(),
    "headroom not importable; set PYTHON to an interpreter with headroom-ai installed",
)
class RealHeadroomTest(unittest.TestCase):
    def test_compresses_large_tool_output(self):
        big = json.dumps([{"i": i, "x": "deadbeef" * 8, "note": "no changes"} for i in range(400)])
        req = json.dumps({
            "id": 1,
            "payload": {
                "model": "gpt-4o",
                "messages": [
                    {"role": "user", "content": "go"},
                    {"role": "tool", "content": big},
                    {"role": "user", "content": "summarize"},
                ],
            },
        })

        # Ensure no fake headroom shadows the real one.
        env = dict(os.environ)
        env.pop("PYTHONPATH", None)
        proc = subprocess.run(
            [sys.executable, "-u", WORKER],
            input=req + "\n",
            capture_output=True,
            text=True,
            timeout=180,
            env=env,
        )
        out = [json.loads(line) for line in proc.stdout.splitlines() if line.strip()]

        self.assertEqual(out[0], {"ready": True}, msg=proc.stderr)
        resp = out[1]
        self.assertTrue(resp["ok"], msg=f"worker error: {resp}\nstderr:\n{proc.stderr}")
        result = resp["result"]
        for key in ("messages", "tokens_before", "tokens_after", "tokens_saved",
                    "compression_ratio", "transforms_applied"):
            self.assertIn(key, result)
        self.assertIsInstance(result["messages"], list)
        # A 400-element repetitive tool result should compress.
        self.assertGreater(result["tokens_saved"], 0)


if __name__ == "__main__":
    unittest.main()
