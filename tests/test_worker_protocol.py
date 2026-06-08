"""Black-box test of worker.py's stdin/stdout protocol loop.

Runs the real worker.py as a subprocess, but with the fake headroom fixture on
PYTHONPATH so compression is deterministic and dependency-free. Exercises the
actual main() loop: the ready handshake, success framing, and the never-crash
error replies for bad payloads and malformed lines.
"""

import json
import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(__file__)
ROOT = os.path.abspath(os.path.join(HERE, ".."))
FIXTURES = os.path.join(HERE, "fixtures")
WORKER = os.path.join(ROOT, "worker.py")


def run_worker(lines, timeout=60):
    env = dict(os.environ)
    # Prepend the fake headroom so it shadows any real install.
    env["PYTHONPATH"] = FIXTURES + os.pathsep + env.get("PYTHONPATH", "")
    proc = subprocess.run(
        [sys.executable, "-u", WORKER],
        input="\n".join(lines) + "\n",
        capture_output=True,
        text=True,
        timeout=timeout,
        env=env,
    )
    out = [json.loads(line) for line in proc.stdout.splitlines() if line.strip()]
    return out, proc


class ProtocolTest(unittest.TestCase):
    def test_ready_then_responses(self):
        reqs = [
            json.dumps({"id": 1, "payload": {"messages": [{"role": "user", "content": "hi"}], "model": "m"}}),
            json.dumps({"id": 2, "payload": {"messages": "not-a-list"}}),
            "this is not json",
            json.dumps({"id": 4, "payload": {"messages": [{"role": "user", "content": "x"}]}}),
        ]
        out, proc = run_worker(reqs)

        self.assertEqual(out[0], {"ready": True}, msg=proc.stderr)

        # 1: valid -> ok with result fields
        self.assertEqual(out[1]["id"], 1)
        self.assertTrue(out[1]["ok"])
        result = out[1]["result"]
        for key in ("messages", "tokens_before", "tokens_after", "tokens_saved",
                    "compression_ratio", "transforms_applied"):
            self.assertIn(key, result)
        self.assertEqual(result["tokens_saved"], 50)  # fixture halves 100 tokens

        # 2: messages not a list -> ok:false, id preserved
        self.assertEqual(out[2]["id"], 2)
        self.assertFalse(out[2]["ok"])
        self.assertEqual(out[2]["error_type"], "ValueError")

        # 3: malformed json line -> ok:false with null id
        self.assertIsNone(out[3]["id"])
        self.assertFalse(out[3]["ok"])

        # 4: valid without model -> still ok
        self.assertEqual(out[4]["id"], 4)
        self.assertTrue(out[4]["ok"])

    def test_clean_exit_on_eof(self):
        out, proc = run_worker([json.dumps({"id": 1, "payload": {"messages": [{"role": "user", "content": "x"}]}})])
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertEqual(out[0], {"ready": True})


if __name__ == "__main__":
    unittest.main()
