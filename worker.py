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
    tokenizer/limit selection)."""
    messages = payload.get("messages")
    if not isinstance(messages, list):
        raise ValueError("payload.messages must be a list")

    model = payload.get("model")

    # v1 calls compress bare. Config knobs are available as kwargs and mirror
    # Headroom's CompressConfig / the proxy's /v1/compress `config` field:
    #   compress_user_messages, target_ratio, protect_recent,
    #   protect_analysis_context (also model_limit for non-200k-context models).
    # We will surface these selectively as server flags in a later iteration.
    if model:
        result = compress(messages, model=model)
    else:
        result = compress(messages)

    return _result_to_dict(result)


def _emit(obj: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps(obj))
    sys.stdout.write("\n")
    sys.stdout.flush()


def main() -> int:
    # Warm the lazily-built singleton pipeline so the first real request does
    # not pay the import/build cost. A tiny dummy compress forces _get_pipeline().
    try:
        compress([{"role": "user", "content": "warmup"}])
    except Exception:
        # Warmup is best-effort; a failure here shouldn't prevent us from
        # serving (and would resurface per-request anyway).
        pass

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
