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
import re
import sys
import time
from typing import Any

# Kompress (the ML text compressor) loads ModernBERT via transformers'
# from_pretrained(), which — without local_files_only — revalidates the cache
# against the HuggingFace Hub on every cold load. That network round-trip shows
# up as "unauthenticated requests to the HF Hub", adds latency to the first
# request a worker serves, and risks anonymous rate-limiting across a pool. We
# tame it *before* importing headroom (so transformers sees the env at import).
#
# ModernBERT is the Kompress tokenizer/encoder base, unchanged across versions.
_MODERNBERT_REPO = "answerdotai/ModernBERT-base"


def _headroom_version() -> str | None:
    """Installed headroom-ai version string, or None if unreadable.

    Read via importlib.metadata and never by importing headroom — importing it
    here would pull in transformers before we've set the offline env, which is
    exactly what this module avoids. Reported to the Go side at startup (see
    main) so it can gate version-specific config knobs in one place."""
    try:
        from importlib.metadata import version

        return version("headroom-ai")
    except Exception:  # noqa: BLE001 - missing/odd metadata -> unknown
        return None


def _kompress_weights_repo() -> str:
    """HF repo holding the Kompress weights for the *installed* headroom-ai.

    The default Kompress model changed in headroom-ai 0.24.0
    (chopratejas/kompress-base -> chopratejas/kompress-v2-base). To support
    hosts on either version (including ones mid-upgrade that still have only the
    old model cached), we resolve the repo from the installed package version
    rather than hardcoding one.

    On an unreadable/odd version we assume the current default (v2): the worst
    case is then a stale guess that loses the offline optimization, never one
    that forces offline against a model the installed version won't load."""
    ver = _headroom_version()
    if ver:
        try:
            major, minor = (int(p) for p in ver.split(".")[:2])
            if (major, minor) < (0, 24):
                return "chopratejas/kompress-base"
        except Exception:  # noqa: BLE001 - odd version -> assume current default
            pass
    return "chopratejas/kompress-v2-base"


def _models_cached() -> bool:
    """True if the Kompress models the installed headroom will load are already
    in the local HF cache, so it's safe to run transformers offline (no network
    needed to load them)."""
    try:
        from huggingface_hub import scan_cache_dir

        repos = {r.repo_id for r in scan_cache_dir().repos}
    except Exception:  # noqa: BLE001 - hub missing/unscannable -> assume not cached
        return False
    return {_MODERNBERT_REPO, _kompress_weights_repo()}.issubset(repos)


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

# Default context window when we can't resolve a model's real limit. Matches
# compress()'s own default; correct for most current Claude models.
_DEFAULT_MODEL_LIMIT = 200000

# tsheadroom-side context-window overrides, consulted BEFORE Headroom's registry.
# The bundled registry lists no current Claude 4.x model, so without these they
# fall to _DEFAULT_MODEL_LIMIT (200K) — which silently over-compresses a model
# whose real window is larger. Each entry is a precompiled case-insensitive regex
# matched with re.search (unanchored), so it matches anywhere in the model string
# and tolerates punctuation/prefix/suffix drift: r"claude-opus-?4.8" matches
# claude-opus-4-8, claude-opus4-8, claude-opus-4-8[1m], claude-opus-4.8, and a
# provider-qualified anthropic/claude-opus-4-8. Tighten a pattern if it would
# over-match a same-family model with a different window.
_MODEL_LIMIT_OVERRIDES: tuple[tuple[re.Pattern[str], int], ...] = (
    (re.compile(r"claude-opus-?4.8", re.IGNORECASE), 1_000_000),
)

try:
    from headroom.models.registry import ModelRegistry as _ModelRegistry
except Exception:  # noqa: BLE001 - older headroom without the registry
    _ModelRegistry = None


def _context_limit(model: str) -> int:
    """Resolve a model's context-window limit.

    compress() drives its aggressiveness off context_pressure =
    tokens_before / model_limit, but defaults model_limit to a flat 200000
    regardless of `model`. So passing the real limit matters: a big-context
    model (e.g. a 1M-token Gemini, or claude-opus-4-8) would otherwise be
    over-compressed, and a smaller one under-compressed. Resolution order:
    tsheadroom overrides (regex search) -> Headroom's registry -> the 200K
    default (used for models neither source knows, which is correct for the
    current 200K Claude models the registry doesn't list yet)."""
    for pat, limit in _MODEL_LIMIT_OVERRIDES:
        if pat.search(model):
            return limit
    if _ModelRegistry is not None:
        try:
            return int(_ModelRegistry.get_context_limit(model, default=_DEFAULT_MODEL_LIMIT))
        except Exception:  # noqa: BLE001 - registry shape changed; fall back
            pass
    return _DEFAULT_MODEL_LIMIT


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

    # Defense in depth for savings_profile (headroom >= 0.26.0). headroom applies
    # the named profile *before* compress()'s own fail-open try, so an unknown
    # name raises ValueError straight out of compress() — which would make the Go
    # pool recycle this worker on every such request. The Go config layer already
    # whitelists the value, but if a bad one ever reaches us, drop it and compress
    # at baseline rather than crash-looping the slot.
    profile = kwargs.get("savings_profile")
    if profile:
        try:
            from headroom.agent_savings import get_agent_savings_profile

            get_agent_savings_profile(profile)
        except Exception as e:  # noqa: BLE001 - unknown profile (or pre-0.26 headroom)
            print(
                f"tsheadroom: ignoring unusable savings_profile {profile!r} ({e})",
                file=sys.stderr,
                flush=True,
            )
            kwargs.pop("savings_profile", None)

    model = payload.get("model")
    if model:
        kwargs["model"] = model
        # Resolve the real context window so compression aggressiveness is sized
        # to the model, not compress()'s flat 200000 default. Don't override an
        # explicit model_limit from config.
        if "model_limit" not in kwargs:
            kwargs["model_limit"] = _context_limit(model)
    # 0 when neither config nor a model supplied one (compress() then uses its
    # own default); surfaced on the -v line for visibility.
    model_limit = int(kwargs.get("model_limit", 0))

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
    out["model_limit"] = model_limit
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
    _emit({"ready": True, "headroom_version": _headroom_version()})

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
