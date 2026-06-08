"""In-process unit tests for worker.py logic.

These never touch the real headroom package: a stub is injected before importing
worker, and each test substitutes worker.compress with a recorder. They run on
any Python (no headroom-ai required), so regressions in the worker's request
handling are caught even in a minimal CI.
"""

import os
import sys
import types
import unittest

# Make worker.py importable from the repo root.
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)

# Shadow headroom with a stub so `import worker` is fast and dependency-free.
# Tests override worker.compress directly, so the stub body is never used.
_stub = types.ModuleType("headroom")
_stub.compress = lambda messages, model=None: None  # noqa: E731
sys.modules.setdefault("headroom", _stub)

import worker  # noqa: E402


class FakeResult:
    def __init__(self, **kw):
        self.__dict__.update(kw)


def fake_result(messages=None, **kw):
    base = dict(
        messages=messages if messages is not None else [],
        tokens_before=0,
        tokens_after=0,
        tokens_saved=0,
        compression_ratio=0.0,
        transforms_applied=[],
    )
    base.update(kw)
    return FakeResult(**base)


class WorkerLogicTest(unittest.TestCase):
    def setUp(self):
        self._orig = worker.compress
        self.calls = []

        def recorder(messages, model="SENTINEL_DEFAULT"):
            self.calls.append({"messages": messages, "model": model})
            return fake_result(messages=[{"ok": True}], tokens_saved=7)

        worker.compress = recorder

    def tearDown(self):
        worker.compress = self._orig

    def test_result_to_dict_projects_fields(self):
        r = fake_result(
            messages=[{"role": "user"}],
            tokens_before=10,
            tokens_after=4,
            tokens_saved=6,
            compression_ratio=0.4,
            transforms_applied=["t"],
        )
        self.assertEqual(
            worker._result_to_dict(r),
            {
                "messages": [{"role": "user"}],
                "tokens_before": 10,
                "tokens_after": 4,
                "tokens_saved": 6,
                "compression_ratio": 0.4,
                "transforms_applied": ["t"],
            },
        )

    def test_passes_model_when_present(self):
        out = worker._compress({"messages": [{"role": "user"}], "model": "gpt-4o"})
        self.assertEqual(self.calls[-1]["model"], "gpt-4o")
        self.assertEqual(out["tokens_saved"], 7)

    def test_falls_back_to_default_when_model_absent(self):
        worker._compress({"messages": [{"role": "user"}]})
        # model kwarg omitted -> recorder's own default observed
        self.assertEqual(self.calls[-1]["model"], "SENTINEL_DEFAULT")

    def test_falls_back_to_default_when_model_empty(self):
        worker._compress({"messages": [{"role": "user"}], "model": ""})
        self.assertEqual(self.calls[-1]["model"], "SENTINEL_DEFAULT")

    def test_non_list_messages_raises(self):
        with self.assertRaises(ValueError):
            worker._compress({"messages": "not a list"})

    def test_missing_messages_raises(self):
        with self.assertRaises(ValueError):
            worker._compress({})


if __name__ == "__main__":
    unittest.main()
