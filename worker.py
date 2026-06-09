#!/usr/bin/env python3
# Copyright (c) Tailscale Inc & AUTHORS
# SPDX-License-Identifier: BSD-3-Clause

"""Headroom compression worker.

A long-lived process that the Go supervisor spawns and talks to over
stdin/stdout. It is a thin, aperture-agnostic wrapper around Headroom's
public one-function API (`from headroom import compress`): messages +
model in, compressed messages + metrics out. All aperture/guardrail
knowledge lives in the Go handler, never here.

Protocol (newline-delimited JSON, one object per line):

  startup, once ready:   {"ready": true}
  request  (Go -> here): {"id": <int>, "payload": {"messages": [...], "model": "...",
                                                    "config": {...CompressConfig knobs...}}}
  response (here -> Go): {"id": <int>, "ok": true,  "result": {...CompressResult...}}
                         {"id": <int>, "ok": false, "error": "...", "error_type": "..."}

`config` is optional and forwarded straight to compress() as kwargs. On startup,
if $TSHEADROOM_PRELOAD=1 (set by the Go parent when text compression is enabled),
the ML model is preloaded (see _warmup) so the first real request doesn't pay the
load. fd 1 is reserved exclusively for the protocol — see main().

Invariants:
  - exactly one request is in flight at a time (the stream is ordered);
    concurrency comes from running multiple workers, not from this loop.
  - we never crash on a bad request: malformed input becomes an `ok:false`
    reply so the Go side can fail open. EOF on stdin is a clean exit.

Run with `python3 -u worker.py` (unbuffered) using an interpreter whose
environment has `headroom` installed.
"""

from __future__ import annotations

import json
import os
import sys
import time
from typing import Any

# Kompress (the ML text compressor) loads ModernBERT via transformers'
# from_pretrained(), which — without local_files_only — revalidates the cache
# against the HuggingFace Hub on every cold load. That network round-trip shows
# up as "unauthenticated requests to the HF Hub", adds latency to the first
# request a worker serves, and risks anonymous rate-limiting across a pool. We
# tame it *before* importing headroom (so transformers sees the env at import).
_KOMPRESS_REPOS = ("answerdotai/ModernBERT-base", "chopratejas/kompress-base")


def _models_cached() -> bool:
    """True if the Kompress models are already in the local HF cache, so it's
    safe to run transformers offline (no network needed to load them)."""
    try:
        from huggingface_hub import scan_cache_dir

        repos = {r.repo_id for r in scan_cache_dir().repos}
    except Exception:  # noqa: BLE001 - hub missing/unscannable -> assume not cached
        return False
    return set(_KOMPRESS_REPOS).issubset(repos)


def _configure_hf_env() -> None:
    """Avoid a per-cold-load HF Hub round-trip on the model-load path.

    - If HF_TOKEN is set, stay online: the token raises rate limits and lets a
      fresh host download the model. We change nothing else.
    - Otherwise, if the models are already cached locally, force offline mode so
      from_pretrained never touches the network. If they're not cached yet
      (fresh host, no token), stay online so the first download can happen.

    Operator-set HF_HUB_OFFLINE / TRANSFORMERS_OFFLINE are respected (setdefault).
    """
    if os.environ.get("HF_TOKEN"):
        return
    if _models_cached():
        os.environ.setdefault("HF_HUB_OFFLINE", "1")
        os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")


_configure_hf_env()

from headroom import compress  # noqa: E402 - must follow _configure_hf_env()

# The protocol stream. main() repoints this at a private dup of the original
# stdout and redirects fd 1 to stderr, so library output (HF/transformers/torch
# progress, stray prints) can never corrupt the NDJSON the Go parent reads.
_proto = sys.stdout

# Flips to True after this worker serves its first real request, so we can flag
# the cold call (the one that may pay the lazy model load) to the Go side.
_served_a_request = False


def _result_to_dict(result: Any) -> dict[str, Any]:
    """Project a CompressResult onto the plain fields the Go side consumes."""
    return {
        "messages": result.messages,
        "tokens_before": result.tokens_before,
        "tokens_after": result.tokens_after,
        "tokens_saved": result.tokens_saved,
        "compression_ratio": result.compression_ratio,
        "transforms_applied": result.transforms_applied,
    }


def _compress(payload: dict[str, Any]) -> dict[str, Any]:
    """Run one compression. `messages` is required; `model` is optional
    (absent/empty -> let `compress` use its own default, which only drives
    tokenizer/limit selection); `config` carries the tunable CompressConfig
    knobs (compress_user_messages, target_ratio, protect_recent, etc.) sent by
    the Go side and forwarded straight through as kwargs."""
    messages = payload.get("messages")
    if not isinstance(messages, list):
        raise ValueError("payload.messages must be a list")

    config = payload.get("config")
    kwargs: dict[str, Any] = dict(config) if isinstance(config, dict) else {}

    model = payload.get("model")
    if model:
        kwargs["model"] = model

    global _served_a_request
    cold_first_call = not _served_a_request
    _served_a_request = True

    t0 = time.monotonic()
    result = compress(messages, **kwargs)
    elapsed_ms = (time.monotonic() - t0) * 1000.0

    out = _result_to_dict(result)
    # Diagnostics for the Go side (handler -v line / pool slow-call log). A cold
    # first call that's also slow is the lazy ML-model-load case.
    out["elapsed_ms"] = round(elapsed_ms, 1)
    out["cold_first_call"] = cold_first_call
    return out


def _emit(obj: dict[str, Any]) -> None:
    _proto.write(json.dumps(obj))
    _proto.write("\n")
    _proto.flush()


def _preload_requested() -> bool:
    """Whether the Go parent asked us to preload the ML model at startup. The
    'is text compression enabled' decision is made once, in Go, and passed via
    TSHEADROOM_PRELOAD so the two sides can't drift."""
    return os.environ.get("TSHEADROOM_PRELOAD") == "1"


def _warmup() -> None:
    """Warm the pipeline so the first real request doesn't pay the build cost.

    Always builds the (lazy) pipeline. When preload is requested, also runs a
    sizable user-message compress to force the ML model (Kompress) to load now —
    otherwise that multi-second load would land on the first real request. The
    load time is logged (to stderr, which the Go parent captures) so cold-start
    cost is visible. Best-effort: failures here never block serving."""
    try:
        if _preload_requested():
            t0 = time.monotonic()
            big = "The system processes the request and returns a response. " * 80
            compress([{"role": "user", "content": big}], compress_user_messages=True)
            dt = time.monotonic() - t0
            print(f"tsheadroom: ML model preload complete in {dt:.1f}s", file=sys.stderr, flush=True)
        else:
            compress([{"role": "user", "content": "warmup"}])
    except Exception as e:  # noqa: BLE001 - warmup is best-effort
        print(f"tsheadroom: warmup failed ({type(e).__name__}): {e}", file=sys.stderr, flush=True)


def main() -> int:
    # Isolate the protocol stream: keep a private handle to the real stdout for
    # NDJSON, then point fd 1 at stderr so any library that writes to stdout
    # can't inject a rogue line into the protocol.
    global _proto
    sys.stdout.flush()
    _proto = os.fdopen(os.dup(1), "w")
    os.dup2(2, 1)  # fd 1 -> stderr; print()/library stdout now go to stderr

    _warmup()
    _emit({"ready": True})

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue

        # Parse the envelope. If even the id is unrecoverable, reply with a
        # null id so the Go side can still correlate-or-fail-open.
        try:
            req = json.loads(line)
            req_id = req.get("id")
            payload = req.get("payload") or {}
        except Exception as e:  # noqa: BLE001 - report, never crash
            _emit({"id": None, "ok": False, "error": str(e), "error_type": type(e).__name__})
            continue

        try:
            result = _compress(payload)
            _emit({"id": req_id, "ok": True, "result": result})
        except Exception as e:  # noqa: BLE001 - report, never crash
            _emit({"id": req_id, "ok": False, "error": str(e), "error_type": type(e).__name__})

    return 0


if __name__ == "__main__":
    sys.exit(main())
