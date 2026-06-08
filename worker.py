#!/usr/bin/env python3
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
from typing import Any

from headroom import compress

# The protocol stream. main() repoints this at a private dup of the original
# stdout and redirects fd 1 to stderr, so library output (HF/transformers/torch
# progress, stray prints) can never corrupt the NDJSON the Go parent reads.
_proto = sys.stdout


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

    result = compress(messages, **kwargs)
    return _result_to_dict(result)


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
    otherwise that multi-second load would land on the first real request.
    Best-effort: failures here never block serving."""
    try:
        if _preload_requested():
            big = "The system processes the request and returns a response. " * 80
            compress([{"role": "user", "content": big}], compress_user_messages=True)
        else:
            compress([{"role": "user", "content": "warmup"}])
    except Exception:
        pass


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
