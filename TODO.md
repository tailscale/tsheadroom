# TODO

## Consider: stateful, delta-only workers (cut per-turn transfer cost)

**Status:** idea / not scheduled. Captured from a perf discussion (June 2026).

**Problem.** On a warm conversation the per-process compression cache makes
*compute* nearly free (`worker_ms ≈ 60ms` — only the 2 new messages actually
recompress), but every turn we still read, parse, ship, and re-encode the
*entire growing conversation* across two boundaries (Aperture↔handler and
handler↔worker). So warm-call latency tracks conversation **size**, not work
done, and climbs as the session grows (observed ~1s and rising on a ~1.2 MB
conversation, with `worker_ms` flat at ~60ms). The `json.RawMessage` passthrough
(done) removed the redundant Go-side parse/marshal, but the dominant costs
remain: the Aperture body read, the stdio pipe transfer, and the worker's
`json.loads`/`json.dumps` of the full body — all unavoidable while we send the
whole conversation each turn.

**Idea.** Make the worker hold the parsed/compressed conversation per session and
accept only the **delta** (new messages since last turn), returning only the
newly compressed tail. Per-turn transfer + parse drops from O(conversation) to
O(new messages).

**Why it's a big change (hence: not now):**
- The worker becomes **stateful and session-pinned** — affinity stops being a
  warm-cache optimization and becomes load-bearing correctness (a turn *must*
  reach the worker holding its prior state, or we fall back to a full resend).
- Needs cache lifecycle: per-session eviction/TTL, memory bounds, and a clean
  full-resync path when a worker is recycled, spills to another slot, or the
  client's prior-prefix doesn't match (edits, retries, branch/rewind).
- Protocol grows: send "base version + appended messages," handle resync.
- Overlaps heavily with the proxy-wrap rethink below (the proxy already keeps
  per-session state via CCR/prefix-cache) — if we go that direction, this comes
  with it rather than being built separately here.

**Cheaper interim levers if warm-call latency bites before then:** raise
`-pool-size` / add real queueing+backpressure; or revisit whether the full body
must cross the Aperture boundary each call.

## Consider: workers wrap the headroom *proxy* (`/v1/compress`) instead of calling `compress()`

**Status:** idea / not scheduled. Captured from a design discussion (June 2026).

### Motivation

tsheadroom workers currently call headroom's library `compress()`. That's the
stable, response-independent public API — but it is a *frozen subset* of what
headroom's own proxy does. The proxy keeps gaining compression power that
`compress()` never exposes. The clearest example: `savings_profile="agent-90"`.

- Via `compress()` it sets only six knobs (`compress_user_messages`,
  `compress_system_messages`, `protect_recent`, `protect_analysis_context`,
  `target_ratio`, `min_tokens_to_compress`).
- The proxy additionally applies `force_kompress`, `max_items_after_crush`,
  `smart_crusher_with_compaction`, and a tuned `ContentRouterConfig`.

`force_kompress` is the big one: it overrides per-block strategy selection so
every eligible block routes through the Kompress ML compressor
(`transforms/content_router.py:1031`, read from kwargs at `:2152`), which is the
main driver of agent-90's headline savings. `compress()` cannot reach it — it
builds a default `ContentRouter()` and forwards only a fixed kwarg list to
`pipeline.apply()` (see `headroom/compress.py`), and `force_kompress` is neither
in that list nor a `CompressConfig` field. The proxy gets it by building its own
pipeline and spreading `**proxy_pipeline_kwargs(self.config)` into
`apply()` per request (`headroom/proxy/handlers/anthropic.py:1069`).

The strategic appeal: stop being stuck on the `compress()` subset and instead
ride headroom's actively-developed compression surface.

### The seam that makes it feasible

headroom ships exactly the endpoint this needs:

- **`POST /v1/compress`** (`headroom/proxy/server.py:3438` →
  `headroom/proxy/handlers/openai.py:5857`, `handle_compress`).
  "Compress messages without calling an LLM." Runs the **full proxy pipeline**
  (`**proxy_pipeline_kwargs(self.config)`, force_kompress included) and
  **returns** `{messages, tokens_before, tokens_after, tokens_saved,
  transforms_applied, ccr_hashes}` instead of forwarding upstream. Has an
  `x-headroom-bypass: true` no-op escape hatch.

So workers could become thin HTTP clients to a local headroom proxy process
hitting `/v1/compress`, rather than NDJSON subprocesses calling `compress()`.

### CRITICAL CONSTRAINT: CCR must be OFF (compression-only mode)

The proxy can be more aggressive than `compress()` **only because it sits in the
response path.** Its headline feature, CCR (Compress-Cache-Retrieve), *replaces*
content with retrieval markers and relies on seeing the model's response to
resolve `headroom_retrieve` calls and serve cached content back. `/v1/compress`
returns `ccr_hashes`/`markers_inserted` for exactly that reason.

tsheadroom is an Aperture **pre_request** hook: we modify the request, Aperture
forwards it to the *real* upstream, and **we never see the response.** So any CCR
retrieval markers we let the proxy inject are **dangling** — the upstream model
is told to call a tool that isn't there, or content is dropped that was meant to
be retrievable. That can corrupt the conversation. This is precisely why
`compress()` exists as the "safe, stateless, response-independent" subset.

- Mitigation: 0.26.0 added **compression-only mode** (CLI opt-outs for CCR
  injection, headroom #823; `ccr_inject_marker` is a config flag). Run the proxy
  with CCR injection **off** → `/v1/compress` becomes a strict, response-
  independent *superset* of `compress()` (force_kompress + router tuning,
  no markers we can't honor).
- Hard line: the day we'd actually want CCR is the day we'd have to become a real
  in-band MITM proxy — which contradicts the never-block / fail-open /
  pre_request-guardrail identity. Don't cross that line.

### Architecture implications

- **Runtime model:** one shared local proxy process (uvicorn/FastAPI) + Go-side
  HTTP calls, instead of N supervised NDJSON subprocesses. The proxy runs its own
  bounded compression thread-pool (`_compression_executor`,
  `compression_max_workers`), so it manages concurrency.
- **Affinity/pool machinery largely dissolves.** Session affinity exists to keep
  one worker's per-process content cache warm for a conversation. With a single
  shared proxy process there's one warm cache for all sessions — better hit rate,
  and affinity becomes unnecessary. We'd delete a lot of `pool.go`.
- **Dependency contract shifts** from a Python function signature to an HTTP API.
  Arguably *more* stable and public than `compress()` — and it's the surface that
  actually receives the improvements.
- **fail-open stays trivial:** HTTP timeout/error → `allow` (passthrough).
- **Footprint grows:** need the full proxy stack (fastapi/uvicorn + deps), not
  just `headroom-ai[ml]`. Keep `HF_HUB_OFFLINE`-style env on the proxy process.

### Config implications (this argues *against*, or at least is a real cost)

The proxy's config is two-tier with a hard wall, and **there is no runtime
config-mutation endpoint** (proxy routes are health/stats/telemetry/retrieve/
compress only — config is frozen at process startup):

- **Per-request** (the only thing `/v1/compress` reads from the request body):
  just four knobs — `compress_user_messages`, `target_ratio`, `protect_recent`,
  `protect_analysis_context` (`handlers/openai.py:5958-5968`).
- **Process-level (startup CLI/env only):** `savings_profile`, `force_kompress`,
  `max_items_after_crush`, `smart_crusher_with_compaction`, `accuracy_guard`,
  `mode`, `min_tokens`, `kompress_model`.

Consequences for our live-tunable `/config`:

- The four per-request knobs → could be a thin opaque passthrough (shed type
  mirroring for those).
- The powerful knobs (`savings_profile`/`force_kompress`) → **no runtime path.**
  Changing them means relaunching the proxy with new `HEADROOM_*` env. Our atomic
  `PUT /config` swap would become a drain-and-restart orchestration.
- Net: wrapping the proxy *reduces* runtime config flexibility. Today every
  `CompressConfig` knob is live-tunable per request (we forward the full config
  dict to `compress()` each call). The proxy deliberately freezes policy at
  startup because it configures stateful machinery (the `ContentRouter` is built
  once with its config baked in).
- Coherent shape if pursued: make the **profile a deploy-time decision**
  (`--savings-profile agent-90`, compression-only), expose only the four
  per-request knobs at runtime. Defensible, but a different product stance than
  "everything is runtime-tunable."

Note: the current `compress()` "sync tax" (hand-adding each new `CompressConfig`
field to `CompressSettings`) is a *deliberate choice* for validation + 400s +
typed/persisted/discoverable API, not a requirement — `compress()` already
ignores unknown kwargs (`headroom/compress.py`: `if key in config_fields`). The
proxy would not eliminate this cleanly.

### Cheap way to derisk before committing

Spike, no architecture change:
1. Stand up `headroom proxy` locally in **compression-only mode** (CCR injection
   off), profile `agent-90`, `force_kompress` on.
2. `curl POST /v1/compress` with representative payloads (large tool outputs,
   long coding-agent conversations).
3. Diff the returned body against what our `compress()` worker produces on the
   same payload. Measure: realized `tokens_saved` delta, and confirm the output
   is **clean of retrieval markers** (`ccr_hashes` empty / no `headroom_retrieve`
   tool references).

That tells us the real savings upside and whether compression-only output is
safe for a fire-and-forget hook — before touching `pool.go`/`worker.py`.

### Open questions

- Does compression-only `/v1/compress` ever still emit retrieval markers, or set
  `ccr_hashes` non-empty? (Spike must verify empty.)
- Prefix-cache / `frozen_message_count` handling: the proxy computes this from
  its own state; what does `/v1/compress` do without that context, and does it
  matter for our payloads?
- Single shared proxy: CPU bottleneck vs. its internal thread-pool — does it hold
  up under our concurrency, or do we run a few proxy processes?
- Process supervision / restart-to-repolicy story vs. today's hot config swap.
