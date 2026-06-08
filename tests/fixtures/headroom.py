"""A fake `headroom` module for deterministic worker protocol tests.

Put this directory on PYTHONPATH ahead of any real install so that
`from headroom import compress` in worker.py resolves here. It mimics the
public surface worker.py depends on — a `compress(messages, model=...)` callable
returning an object with the CompressResult fields — without the real package's
Rust extension or dependencies.
"""

from __future__ import annotations

# Sentinel so tests can detect when worker.py omitted the model argument.
DEFAULT_MODEL = "FAKE-DEFAULT-MODEL"


class CompressResult:
    def __init__(
        self,
        messages,
        tokens_before=0,
        tokens_after=0,
        tokens_saved=0,
        compression_ratio=0.0,
        transforms_applied=None,
    ):
        self.messages = messages
        self.tokens_before = tokens_before
        self.tokens_after = tokens_after
        self.tokens_saved = tokens_saved
        self.compression_ratio = compression_ratio
        self.transforms_applied = transforms_applied or []


def compress(messages, model=DEFAULT_MODEL):
    if not messages:
        return CompressResult(messages=[])
    # Pretend we halved the token count, and echo a marker so tests can confirm
    # the worker returned the compressed messages (not the originals).
    return CompressResult(
        messages=[{"compressed": True, "model": model}],
        tokens_before=100,
        tokens_after=50,
        tokens_saved=50,
        compression_ratio=0.5,
        transforms_applied=["fake:crusher"],
    )
