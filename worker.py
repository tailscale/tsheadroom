#!/usr/bin/env python3
"""Headroom compression worker.

A long-lived process that the Go supervisor spawns and talks to over
stdin/stdout. It is a thin, aperture-agnostic wrapper around Headroom's
public one-function API (`from headroom import compress`): messages +
model in, compressed messages + metrics out. All aperture/guardrail
knowledge lives in the Go handler, never here.

Protocol (newline-delimited JSON, one object per line):

  startup, once ready:   {"ready": true}
  request  (Go -> here): {"id": <int>, "payload": {"messages": [...], "model": "..."}}
  response (here -> Go): {"id": <int>, "ok": true,  "result": {...CompressResult...}}
                         {"id": <int>, "ok": false, "error": "...", "error_type": "..."}

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
import sys
from typing import Any

from headroom import compress


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
    sys.stdout.write(json.dumps(obj))
    sys.stdout.write("\n")
    sys.stdout.flush()


def _text_compression_enabled() -> bool:
    """Read the config file named by TSHEADROOM_CONFIG (if any) and report
    whether a text knob is on — i.e. whether the ML text compressor (Kompress)
    will be exercised, and therefore worth preloading at startup."""
    import os

    path = os.environ.get("TSHEADROOM_CONFIG")
    if not path:
        return False
    try:
        with open(path) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        return False
    return bool(cfg.get("compress_user_messages")) or cfg.get("target_ratio") is not None


def _warmup() -> None:
    """Warm the pipeline so the first real request doesn't pay the build cost.

    Always builds the (lazy) pipeline. When the config enables text compression,
    also runs a sizable user-message compress to force the ML model (Kompress)
    to load now — otherwise that multi-second load would land on the first real
    request. Best-effort: failures here never block serving."""
    try:
        if _text_compression_enabled():
            big = "The system processes the request and returns a response. " * 80
            compress([{"role": "user", "content": big}], compress_user_messages=True)
        else:
            compress([{"role": "user", "content": "warmup"}])
    except Exception:
        pass


def main() -> int:
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
