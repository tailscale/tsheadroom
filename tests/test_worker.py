# Copyright (c) Tailscale Inc & AUTHORS
# SPDX-License-Identifier: BSD-3-Clause

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
from importlib.metadata import PackageNotFoundError
from unittest import mock

# Make worker.py importable from the repo root.
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)

# Shadow headroom with a stub so `import worker` is fast and dependency-free.
# Tests override worker.compress directly, so the stub body is never used.
_stub = types.ModuleType("headroom")
_stub.compress = lambda messages, **kwargs: None  # noqa: E731
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

        def recorder(messages, **kwargs):
            self.calls.append({"messages": messages, "kwargs": kwargs})
            return fake_result(messages=[{"ok": True}], tokens_saved=7)

        worker.compress = recorder

    def tearDown(self):
        worker.compress = self._orig

    def last_kwargs(self):
        return self.calls[-1]["kwargs"]

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
        self.assertEqual(self.last_kwargs().get("model"), "gpt-4o")
        self.assertEqual(out["tokens_saved"], 7)

    def test_omits_model_when_absent(self):
        # No model kwarg forwarded -> headroom uses its own default.
        worker._compress({"messages": [{"role": "user"}]})
        self.assertNotIn("model", self.last_kwargs())

    def test_omits_model_when_empty(self):
        worker._compress({"messages": [{"role": "user"}], "model": ""})
        self.assertNotIn("model", self.last_kwargs())

    def test_forwards_config_kwargs(self):
        cfg = {"compress_user_messages": True, "protect_recent": 0, "target_ratio": 0.3}
        worker._compress({"messages": [{"role": "user"}], "model": "m", "config": cfg})
        k = self.last_kwargs()
        self.assertEqual(k.get("model"), "m")
        self.assertTrue(k["compress_user_messages"])
        self.assertEqual(k["protect_recent"], 0)
        self.assertEqual(k["target_ratio"], 0.3)

    def test_no_config_means_no_extra_kwargs(self):
        worker._compress({"messages": [{"role": "user"}]})
        self.assertEqual(self.last_kwargs(), {})

    def test_non_dict_config_ignored(self):
        worker._compress({"messages": [{"role": "user"}], "config": "nope"})
        self.assertEqual(self.last_kwargs(), {})

    def test_model_limit_override_regex_matches_variants(self):
        # claude-opus-4-8 (1M context) is unknown to Headroom's registry, so the
        # tsheadroom override supplies it. The override is a case-insensitive,
        # unanchored regex (claude-opus-?4.8), so it covers dated/[1m]/punctuation
        # variants, a provider-qualified prefix, and any case.
        for model in (
            "claude-opus-4-8",
            "claude-opus-4-8-20251101",
            "claude-opus-4-8[1m]",
            "claude-opus4-8",  # optional dash after "opus"
            "claude-opus-4.8",  # "." matches the dot too
            "anthropic/claude-opus-4-8",  # unanchored: matches mid-string
            "CLAUDE-OPUS-4-8",  # case-insensitive
        ):
            worker._compress({"messages": [{"role": "user"}], "model": model})
            self.assertEqual(self.last_kwargs().get("model_limit"), 1_000_000, model)

    def test_model_limit_override_does_not_overmatch(self):
        # A different Opus generation must not trip the opus-4-8 override.
        worker._compress({"messages": [{"role": "user"}], "model": "claude-opus-4-1-20250805"})
        self.assertEqual(self.last_kwargs().get("model_limit"), worker._DEFAULT_MODEL_LIMIT)

    def test_model_limit_defaults_when_unresolved(self):
        # Registry is stubbed out in these tests, so an unlisted model with no
        # override falls back to the 200K default.
        worker._compress({"messages": [{"role": "user"}], "model": "some-unknown-model"})
        self.assertEqual(self.last_kwargs().get("model_limit"), worker._DEFAULT_MODEL_LIMIT)

    def test_explicit_config_model_limit_wins(self):
        # An operator-set model_limit in config is never overridden by the lookup.
        worker._compress({
            "messages": [{"role": "user"}],
            "model": "claude-opus-4-8",
            "config": {"model_limit": 12345},
        })
        self.assertEqual(self.last_kwargs().get("model_limit"), 12345)

    def test_non_list_messages_raises(self):
        with self.assertRaises(ValueError):
            worker._compress({"messages": "not a list"})

    def test_missing_messages_raises(self):
        with self.assertRaises(ValueError):
            worker._compress({})


class WarmupTest(unittest.TestCase):
    def setUp(self):
        self._orig = worker.compress
        self._orig_env = os.environ.get("TSHEADROOM_PRELOAD")
        self.calls = []

        def recorder(messages, **kwargs):
            self.calls.append({"messages": messages, "kwargs": kwargs})
            return fake_result()

        worker.compress = recorder

    def tearDown(self):
        worker.compress = self._orig
        if self._orig_env is None:
            os.environ.pop("TSHEADROOM_PRELOAD", None)
        else:
            os.environ["TSHEADROOM_PRELOAD"] = self._orig_env

    def test_preload_requested_reads_env(self):
        os.environ.pop("TSHEADROOM_PRELOAD", None)
        self.assertFalse(worker._preload_requested())
        os.environ["TSHEADROOM_PRELOAD"] = "0"
        self.assertFalse(worker._preload_requested())
        os.environ["TSHEADROOM_PRELOAD"] = "1"
        self.assertTrue(worker._preload_requested())

    def test_warmup_preloads_when_requested(self):
        os.environ["TSHEADROOM_PRELOAD"] = "1"
        worker._warmup()
        # A sizable user-message compress with the text knob forces model load.
        self.assertTrue(self.calls[-1]["kwargs"].get("compress_user_messages"))
        self.assertGreater(len(self.calls[-1]["messages"][0]["content"]), 100)

    def test_warmup_light_when_not_requested(self):
        os.environ.pop("TSHEADROOM_PRELOAD", None)
        worker._warmup()
        # Just builds the pipeline; no text knob forced.
        self.assertNotIn("compress_user_messages", self.calls[-1]["kwargs"])


class KompressRepoTest(unittest.TestCase):
    """The default Kompress weights repo changed in headroom-ai 0.24.0
    (kompress-base -> kompress-v2-base); worker resolves it from the installed
    version so a single worker.py supports both, including mid-upgrade hosts."""

    def _repo_for(self, version_value):
        with mock.patch("importlib.metadata.version", return_value=version_value):
            return worker._kompress_weights_repo()

    def test_legacy_versions_use_kompress_base(self):
        for v in ("0.23.0", "0.23.5", "0.1.0"):
            self.assertEqual(self._repo_for(v), "chopratejas/kompress-base", v)

    def test_v024_plus_uses_v2_base(self):
        for v in ("0.24.0", "0.24.3", "0.25.0", "1.0.0"):
            self.assertEqual(self._repo_for(v), "chopratejas/kompress-v2-base", v)

    def test_unreadable_version_defaults_to_v2(self):
        with mock.patch("importlib.metadata.version", side_effect=PackageNotFoundError):
            self.assertEqual(worker._kompress_weights_repo(), "chopratejas/kompress-v2-base")

    def test_odd_version_defaults_to_v2(self):
        # A non-numeric component must not raise; fall back to the current default.
        self.assertEqual(self._repo_for("unknown"), "chopratejas/kompress-v2-base")

    def test_models_cached_tracks_resolved_repo(self):
        # Inject a fake huggingface_hub so this runs without the real dependency.
        def fake_hub(cached_ids):
            repos = [types.SimpleNamespace(repo_id=i) for i in cached_ids]
            mod = types.ModuleType("huggingface_hub")
            mod.scan_cache_dir = lambda: types.SimpleNamespace(repos=repos)
            return mod

        # 0.24 host with both required repos cached -> safe to go offline.
        both = {"answerdotai/ModernBERT-base", "chopratejas/kompress-v2-base"}
        with mock.patch.dict(sys.modules, {"huggingface_hub": fake_hub(both)}), \
                mock.patch("importlib.metadata.version", return_value="0.24.0"):
            self.assertTrue(worker._models_cached())

        # The upgrade trap: 0.24 installed but only the OLD model cached. We must
        # NOT force offline (we'd block the v2 download), so _models_cached=False.
        only_old = {"answerdotai/ModernBERT-base", "chopratejas/kompress-base"}
        with mock.patch.dict(sys.modules, {"huggingface_hub": fake_hub(only_old)}), \
                mock.patch("importlib.metadata.version", return_value="0.24.0"):
            self.assertFalse(worker._models_cached())


if __name__ == "__main__":
    unittest.main()
