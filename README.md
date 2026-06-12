# tsheadroom

Transparent, org-wide LLM context compression for [Aperture](https://github.com/tailscale/aperture).

Coding agents and RAG pipelines send the model the same bulky, low-value payloads over and over: multi-thousand-line tool outputs, search dumps, build logs, file listings. You pay for every one of those tokens on every turn, and they crowd out the context window. tsheadroom strips that waste out *before* the request reaches the provider, for every team in your org at once, with no client, SDK, or app changes. It plugs into Aperture as a `pre_request` hook.

It runs [Headroom](https://github.com/chopratejas/headroom)'s `compress()` function, which crushes bulky content while leaving prompts, instructions, and recent turns intact. If there's nothing worth compressing (or anything goes wrong), the request passes through unchanged. tsheadroom can never block or break a request.

If you run Aperture, you already run a [Tailscale](https://tailscale.com/) tailnet. tsheadroom joins that tailnet as a device (using [`tsnet`](https://tailscale.com/kb/1244/tsnet)) and Aperture calls out to it. No public endpoint, no API keys: access is gated by your tailnet's [grants](https://tailscale.com/docs/features/access-control/grants).

## What you get

On a representative tool-heavy request (a 400-entry file listing returned as a tool result):

| | Before | After |
|---|---|---|
| The bulky tool result | ~41 KB | ~25 KB (~40% smaller) |
| Full request body | ~47 KB | ~25 KB |
| The user's prompt, system prompt, recent turns | n/a | untouched |

- **Token + cost savings** scale with how much tool/log/search output your traffic carries. Chat-only and short requests are left alone (see [What gets compressed](#what-gets-compressed)).
- **Transparent.** Enforced at the Aperture grant level. Users and apps don't change anything and don't even know it's there.
- **Fail-safe by design.** If it's unreachable, slow, or crashed, tsheadroom always answers `allow` and the original request proceeds. It is structurally incapable of blocking a request.

## What it costs you

Decide whether this fits before you wire it in:

- **Memory**: text compression uses an ML model held resident in each worker (~600 MB/worker; ~4.8 GB at the default pool size of 8). Tool-output-only compression (the base install) needs far less. Size `-pool-size` deliberately.
- **Latency**: warm requests add single-digit milliseconds. The *first* request after startup can block while the ML model loads (and, on a fresh host, downloads). That one-time load is ~60s; see [ML model loading](#ml-model-loading-and-timeouts). You set the ceiling through Aperture's hook `timeout`.
- **Ops**: one long-lived process running as a tailnet device, ideally under `systemd`. Targets Linux (see [Requirements](#requirements)).
- **Prompt cache**: compressing *historical* context changes the cached prefix, which can cause a prompt-cache miss on the next turn. See the [note below](#hook-response-format).

## How it works

```
Aperture --(pre_request hook: POST /)--> tsheadroom (tsnet :80)
                                              │
                                              ├─ pool of persistent Python workers
                                              │     each runs: headroom.compress(messages, model, **config)
                                              │
                                              └─ reply: {"action":"modify","request_body":{…}}
                                                     or  {"action":"allow"}
```

The tsheadroom Go binary owns a small pool of long-lived Python worker processes (so the Headroom pipeline is built once, not per request) and supervises them (auto-restart on crash, fail-open on timeout). Compression runs out-of-process, so a slow or faulty `compress()` call can't take down the listener.

For each request Aperture forwards, tsheadroom hands the `messages` array to `compress()` and returns a `modify` action with the rewritten body, or `allow` if there's nothing to gain or anything goes wrong.

## Requirements

**On the Aperture side:**

- A running Aperture instance with at least one LLM provider configured, and access to edit its configuration (`config.hujson`) and/or your tailnet policy file.
- A Tailscale tailnet (you have one if you run Aperture).

**On the host that runs tsheadroom:**

- **Linux**, for the cloud hosts this is meant to run on. The binary also builds and runs on macOS, which is convenient for local development and testing.
- **Go 1.26.4+** to build the binary (required by the pinned Tailscale dependency).
- **Python with `headroom-ai` installed.** Use a version `headroom-ai` ships a prebuilt wheel for — Python 3.10–3.13 as of this writing. The newest Python release often has no wheel yet (3.14 at the time of writing), and pip then falls back to a Rust source build that fails. If unsure which versions are covered, check `Requires: Python` and the wheel filenames on [headroom-ai's PyPI page](https://pypi.org/project/headroom-ai/).
- **A Tailscale [auth key](https://tailscale.com/docs/features/access-control/auth-keys)** (`TS_AUTHKEY`) so the device can join your tailnet unattended. Generate one in the Tailscale admin console under **Settings** > **Keys**.

### Choose tool-output or text compression

`headroom-ai` has two compression paths, and which extras you install decides what actually compresses:

| You install | What compresses | Memory | Rust toolchain |
|---|---|---|---|
| `headroom-ai` (base) | Tool outputs (SmartCrusher, pure Python) | low | not needed |
| `headroom-ai[ml]` | Tool outputs + text/prose (Kompress, ML) | ~600 MB/worker | not needed |

Install `[ml]` if you want text/prose compression, or if you want the default configuration to work as advertised: `compress_system_messages` is on by default and Headroom also uses the ML model as its fallback for mixed content, so under defaults the model is exercised by ordinary traffic. With only the base wheel, the text-targeting knobs (`compress_user_messages`, `target_ratio`) change routing but produce no savings: only tool-output compression is active. See [Tune compression](#tune-compression-runtime-config).

## Install

```shell
# HTTPS (no SSH key needed):
git clone https://github.com/tailscale/tsheadroom.git
cd tsheadroom
make                      # produces ./build/tsheadroom (Linux)
```

Cross-compiling from a Mac for a Linux host:

```shell
GOOS=linux GOARCH=amd64 go build -o build/tsheadroom .   # or GOARCH=arm64
```

Set up the Python interpreter (a virtualenv is recommended). `headroom-ai` ships prebuilt wheels on supported Python versions, so no Rust toolchain is needed:

```shell
python3.13 -m venv /opt/tsheadroom/venv                   # a supported version, called explicitly (3.13 shown)
/opt/tsheadroom/venv/bin/python --version                 # confirm it matches a version with a wheel
/opt/tsheadroom/venv/bin/pip install 'headroom-ai[ml]'    # or just 'headroom-ai' for tool-output only
```

Name the interpreter explicitly (`python3.13`, or whichever supported version you have) rather than using bare `python3`, which may resolve to a release too new to have a wheel — common on an up-to-date Homebrew or a pyenv default. A venv on an unsupported version makes the `pip install` fail trying to build `headroom-ai` from Rust source (see [Troubleshooting](#install-fails-building-headroom-ai)). If you do not have a supported interpreter, install one with `brew install python@3.13`, `pyenv install 3.13`, or your platform's package manager — substituting the version number for whatever `headroom-ai` currently ships wheels for.

Copy the worker script next to wherever you'll run the service:

```shell
cp worker.py /opt/tsheadroom/worker.py
```

You now have the three things tsheadroom needs at runtime: the binary, a Python interpreter with `headroom-ai`, and `worker.py`.

### Install fails building headroom-ai

If `pip install 'headroom-ai[ml]'` fails with `maturin failed` and an error like:

```
error: the configured Python interpreter version (3.14) is newer than PyO3's maximum supported version (3.13)
```

your virtualenv is on a Python release newer than `headroom-ai`'s latest published wheel (the exact versions in the message will change over time). With no matching wheel, pip tries to compile `headroom-ai` from Rust source and PyO3 rejects the build. Confirm the version with `/opt/tsheadroom/venv/bin/python --version`. To fix it, delete the venv and recreate it with a supported interpreter (3.13 shown here — use whatever [headroom-ai](https://pypi.org/project/headroom-ai/) currently ships a wheel for):

```shell
rm -rf /opt/tsheadroom/venv
python3.13 -m venv /opt/tsheadroom/venv
/opt/tsheadroom/venv/bin/python --version                 # confirm a supported version
/opt/tsheadroom/venv/bin/pip install 'headroom-ai[ml]'
```

A correct install downloads prebuilt `.whl` files (their names contain the Python tag, e.g. `cp313`); if you instead see `Building wheel for headroom-ai` or `maturin`, you are still on an unsupported Python version.

## Run

```shell
TS_AUTHKEY=tskey-auth-xxxx ./build/tsheadroom \
  -hostname tsheadroom \
  -python /opt/tsheadroom/venv/bin/python \
  -worker /opt/tsheadroom/worker.py \
  -state-dir /var/lib/tsheadroom \
  -config /var/lib/tsheadroom/tsheadroom.config.json
```

This joins the tailnet as `tsheadroom` and listens for hook calls on `:80` at path `/`. Other tailnet devices (including Aperture) reach it at `http://tsheadroom.<your-tailnet>.ts.net/` (find `<your-tailnet>`, your [tailnet name](https://tailscale.com/kb/1217/tailnet-name), in the Tailscale admin console).

> **First run on a fresh host**: with `[ml]` installed, the workers load (and on a brand-new host, download ~600 MB of model from HuggingFace) at startup. The very first compression request can block for up to ~60s while that happens; after that, requests are milliseconds. To avoid a slow cold start in production, warm the cache once before serving traffic, or set `HF_TOKEN` (see [ML model loading](#ml-model-loading-and-timeouts)).

`TS_AUTHKEY` is only needed on first start. Once `-state-dir` is populated you can omit it on restarts.

### Run it as a service

For durability, and for the cloud/Linux hosts this is meant for, wrap it in a service manager. A minimal `systemd` unit:

```
[Unit]
Description=tsheadroom (Aperture context-compression hook)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/tsheadroom/tsheadroom \
  -python /opt/tsheadroom/venv/bin/python \
  -worker /opt/tsheadroom/worker.py \
  -state-dir /var/lib/tsheadroom \
  -config /var/lib/tsheadroom/tsheadroom.config.json
Environment=TS_AUTHKEY=tskey-auth-xxxx
StateDirectory=tsheadroom
Restart=always

[Install]
WantedBy=multi-user.target
```

`StateDirectory=tsheadroom` provisions `/var/lib/tsheadroom` with the right ownership. After the device has authenticated once, you can drop the `TS_AUTHKEY` line.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-python` | `python3` | Interpreter with `headroom-ai` installed (used to launch workers). |
| `-worker` | `worker.py` | Path to the worker script. |
| `-hostname` | `tsheadroom` | Device name on the tailnet. |
| `-pool-size` | `8` | Number of persistent Python workers. Each holds a resident copy of the ML model (~600 MB) when text compression is active (~4.8 GB at 8), so raise it deliberately. |
| `-max-compress` | `60s` | Hard cap on a single worker call before the worker is recycled; the sole worker-side timeout. Covers one-time ML model loads. |
| `-addr` | `:80` | Listen address on the tsnet device. |
| `-state-dir` | tsnet default | tsnet state directory. Persist this: it holds the device identity (`tailscaled.state`). See note below. |
| `-config` | `tsheadroom.config.json` | Path to the tunable compression configuration file. Loaded at startup, rewritten on `PUT /config`. |
| `-local-addr` | off | Serve plain HTTP here instead of tsnet, for local testing only. |
| `-v` | off | Log a one-line per-request summary to stdout. See [Verify](#verify-its-working). |
| `-no-affinity` | off | Disable session-affinity worker routing; dispatch every request to any free worker. See [Worker affinity](#worker-affinity). |

> **State (`-state-dir`)**: must be a stable, writable, persistent path. It contains `tailscaled.state`, the device's private key. Treat it as a secret and keep it on durable storage, or the device re-authenticates as a *new* device on every restart.
>
> **Config path (`-config`)**: the default is resolved against the current working directory. In a service, always set an absolute path (for example, under your state dir) so a different launch CWD doesn't silently read/write a different file.

## Wire it into Aperture

tsheadroom is configured as a `pre_request` hook in two places in your Aperture configuration (`config.hujson`).

### 1. Define the hook

Add it to the top-level `hooks` map, pointing at your tsheadroom device:

```json
"hooks": {
  "headroom": {
    "url": "http://tsheadroom.<your-tailnet>.ts.net/",
    "fail_policy": "fail_open", // if tsheadroom is unreachable, send uncompressed
    "timeout": "30s",           // tsheadroom's only client-facing latency ceiling
  },
},
```

| Field | Required | Notes |
|---|---|---|
| `url` | yes | Your tsheadroom device's tailnet URL, path `/`. |
| `fail_policy` | no (default `fail_open`) | Keep `fail_open`. See below. |
| `timeout` | no (default `5s`) | Raise to `30s`. This is the entire latency budget; the `5s` default will cut off cold/large compressions. |
| `preference` | no (default `0`) | Set only if you stack tsheadroom with other hooks. Higher runs first; ties break alphabetically. A `modify` is visible to later hooks. |

tsheadroom needs no `apikey` or `authorization`. It's reached only over your tailnet and gated by your tailnet's [grants](https://tailscale.com/docs/features/access-control/grants).

### 2. Attach it to a grant

A hook does nothing until a grant references it through `send_hooks`:

```json
{
  "src": ["*"],
  "app": {
    "tailscale.com/cap/aperture": [
      {
        "models": "**",            // compress all models, or scope, for example "anthropic/**"
        "send_hooks": [
          {
            "name": "headroom",
            "events": ["pre_request"],
            "send": ["request_body"],
          },
        ],
      },
    ],
  },
},
```

> [!NOTE]
> Where you put this grant changes the `dst` rule. The example above is the Aperture configuration file (`config.hujson`) form, where `dst` is not used, so omit it. If you instead place the grant in your [tailnet policy file](https://tailscale.com/docs/reference/syntax/policy-file), you must add an explicit `dst` (for example, `"dst": ["tag:aperture"]`, where `tag:aperture` is a [tag](https://tailscale.com/docs/features/tags) you've applied to your Aperture device). A tag gives a non-user device a stable identity at authentication time and doubles as a selector you can target in grants, so it's the durable way to name your Aperture device here. Omitting `dst` makes the grant silently apply to nothing and your hook never fires.

Notes:

- `send: ["request_body"]` is required and is the only input tsheadroom uses. A `modify` hook replaces the *entire* request body, so Aperture must send it. (The model name is read from inside the body; tsheadroom doesn't need `user_message`; it operates on the whole `messages` array.)
- Keep `fail_open`. tsheadroom always answers `200` with `allow`/`modify` and never blocks; `fail_open` ensures that even if the device is *unreachable* (or a compression runs past the `timeout`), requests still proceed (uncompressed). `fail_closed` would instead block with HTTP 503, not what you want here.
- The hook `timeout` is the only client-facing latency ceiling. tsheadroom has no soft timeout of its own: it waits for the compression and returns, bounded only by this `timeout` (after which Aperture forwards the original) and by its internal `-max-compress` worker cap. Set it to how long you're willing to make a request wait. `30s` matches Headroom's own compression budget and comfortably fits large, late-session requests. There's no separate tsheadroom knob to keep in sync.
- Scope `models` (FQN glob, for example `"anthropic/**"`, or `"**"` for everything) if you don't want to compress all providers.

Full hook/grant field reference: [Aperture configuration documentation](https://tailscale.com/docs/aperture/configuration).

### Hook response format

tsheadroom returns one of two [`GuardrailResponse`](https://tailscale.com/docs/aperture/configuration) actions, always with HTTP `200`:

```json
{ "action": "allow" }
```

Nothing changed. The original request proceeds. Returned for short/chat-only requests, bodies with no `messages` array, errors, or timeouts.

```json
{ "action": "modify", "request_body": { "model": "…", "messages": [ … ] } }
```

The `messages` array was compressed; every other field of the body is preserved unchanged. The modified body replaces the original wholesale.

tsheadroom never returns `"block"`.

> [!NOTE]
> Prompt-cache interaction. tsheadroom compresses *older* context (it protects the most recent turns), so a `modify` changes the cached prefix and can trigger a prompt-cache miss on the next turn. For tool/log-heavy traffic the token savings typically dominate; for cache-heavy chat workloads, weigh this and scope `models`/knobs accordingly.

## Verify it's working

### 1. Locally, before touching the tailnet

Run with `-local-addr` to serve plain HTTP (no tailnet) and `-v` for per-request logging:

```shell
./build/tsheadroom -local-addr 127.0.0.1:8080 -v \
  -python /opt/tsheadroom/venv/bin/python -worker ./worker.py
```

Send a request with a large tool result, exactly the shape Headroom is built to compress:

```shell
python3 - <<'PY' | curl -s -X POST http://127.0.0.1:8080/ -H 'Content-Type: application/json' -d @-
import json
big = json.dumps([{"id": i, "path": f"/src/m{i}.py", "status": "unchanged",
                   "hash": "deadbeef"*4} for i in range(400)])
print(json.dumps({"request_body": {"model": "claude-sonnet-4-5-20250929", "messages": [
    {"role": "user", "content": "list files"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "ls", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": big}]},
    {"role": "user", "content": "summarize"}]}}))
PY
# -> {"action":"modify","request_body":{…}}
```

> The first request may take ~60s (the `[ml]` model is loading/downloading). It isn't hung. Subsequent requests return in milliseconds.

The proof is in the `-v` log line. Cold first request, then warm:

```
request in_msgs=4 in_bytes=47227 out_bytes=24885 dur_ms=56956 worker_ms=42 cold=true  model_limit=200000 -> modify
request in_msgs=4 in_bytes=47227 out_bytes=24885 dur_ms=12    worker_ms=11 cold=false model_limit=200000 -> modify
```

| Field | Meaning |
|---|---|
| `in_bytes` / `out_bytes` | Request body size before/after. `out_bytes < in_bytes` on a `modify` is compression happening. |
| `dur_ms` / `worker_ms` | Total handler time / time inside the Python worker. A large gap with `cold=true` is the one-time model load. |
| `cold=true` | This request paid the model load. Expect it once per worker. |
| `-> modify` / `-> allow(...)` | The action returned, with a reason (`modify`, `allow(noop)`, `allow(error)`, `allow(passthrough)`, `allow(read-error)`). |

Confirm the inverse too. A short chat returns `allow`:

```shell
curl -s -X POST http://127.0.0.1:8080/ -H 'Content-Type: application/json' \
  -d '{"request_body":{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hi"}]}}'
# -> {"action":"allow"}
```

### 2. End-to-end through Aperture

Once the hook + grant are live:

1. Make a real LLM call through your Aperture endpoint that includes a substantial tool result (or run a coding-agent session that does).
1. On the tsheadroom device, watch the `-v` output (or `journalctl -u tsheadroom -f` under systemd). A request that reaches it and compresses logs `-> modify` with `out_bytes < in_bytes`.

If you see the request arrive and log `modify`, the hook is firing and compressing in production.

### Nothing compressing? Checklist

All `allow`, no `modify`? Work down this list:

| Symptom | Check |
|---|---|
| No requests reach the device at all | The hook is firing? In a tailnet policy-file grant, is `dst` set (see the note above)? Does the grant `src` match your users and `models` match your traffic? |
| Requests arrive but always `allow(passthrough)` | The body has no `messages` array (for example, Gemini `contents` or embeddings), so those pass through by design. |
| Requests arrive but always `allow(noop)` | Nothing worth compressing: short chat, prose-only, or no substantial tool result. This is correct behavior. See [What gets compressed](#what-gets-compressed). |
| Text/prose isn't shrinking | The `[ml]` extra isn't installed, or `compress_user_messages` is off. Install `headroom-ai[ml]` and check `GET /config`. |
| Occasional `allow(error)` under load or on first request | A compression exceeded the Aperture hook `timeout` and failed open. Raise `timeout`, or pre-warm the model (cold start). |

## Tune compression (runtime config)

tsheadroom exposes Headroom's compression knobs as live, persisted configuration, with no restart needed. Settings load from the `-config` file at startup (defaults if missing), and a small HTTP API on the same device reads and changes them on the fly; every change is written back to the file, so the service restarts with your last state.

```shell
# Read the current settings
curl -s http://tsheadroom.<your-tailnet>.ts.net/config

# Change one or more (partial updates merge onto current values)
curl -s -X PUT http://tsheadroom.<your-tailnet>.ts.net/config \
  -H 'Content-Type: application/json' \
  -d '{"compress_user_messages": true, "target_ratio": 0.3}'
```

The default configuration (what `GET /config` returns out of the box):

```json
{
  "compress_user_messages": false,
  "compress_system_messages": true,
  "protect_recent": 4,
  "protect_analysis_context": true,
  "target_ratio": null,
  "min_tokens_to_compress": 250,
  "kompress_model": null
}
```

Changes take effect on the next request. Invalid values are rejected with `400` and leave the current configuration untouched (for example, `target_ratio: 5` → `target_ratio must be in (0, 1] or null`). You can also edit the `-config` JSON file directly and restart.

Tunable parameters (these mirror Headroom's `CompressConfig`):

| field | type | default | effect |
|---|---|---|---|
| `compress_user_messages` | bool | `false` | Also compress user-message content (off = protect it). Turn on for prose/RAG-heavy inputs. *(Needs `[ml]`.)* |
| `compress_system_messages` | bool | `true` | Compress system messages. |
| `protect_recent` | int | `4` | Leave the last N messages untouched. `0` = compress everything. |
| `protect_analysis_context` | bool | `true` | Detect "analyze/review" intent and protect code. |
| `target_ratio` | float \| null | `null` | Keep-ratio for text compression: `null` = model decides (~aggressive), `0.5` = keep 50%. Must be in `(0, 1]`. *(Needs `[ml]`.)* |
| `min_tokens_to_compress` | int | `250` | Skip messages shorter than this. |
| `kompress_model` | string \| null | `null` | Override the Kompress model id; `null` = default. |

> **Access**: the `/config` endpoint is gated only by your [tailnet policy file](https://tailscale.com/docs/reference/syntax/policy-file): anyone who can reach the device can read and change its configuration. Lock the device down accordingly (see [Security](#security-and-data-handling)).

## Metrics (`/metrics`)

tsheadroom exposes a Prometheus endpoint at `GET /metrics` on the same device. It uses the standard text exposition format, so any existing scraper works:

```sh
curl -s http://tsheadroom.<your-tailnet>.ts.net/metrics
```

Two families are emitted. The `headroom_*` metrics reuse the names Headroom's own proxy emits, so if you already scrape a Headroom proxy you can point the same dashboards at this endpoint:

| metric | type | meaning |
|---|---|---|
| `headroom_requests_total` | counter | Compression hook calls processed. |
| `headroom_requests_failed_total` | counter | Calls where compression errored and we failed open. |
| `headroom_tokens_input_total` | counter | Input tokens seen (`tokens_before`). |
| `headroom_tokens_saved_total` | counter | Tokens saved by compression (`tokens_saved`). |
| `headroom_overhead_ms_{sum,count,min,max}` | counter/gauge | tsheadroom processing time per request, in ms (excludes the upstream LLM — that's what Headroom calls "overhead"). |
| `headroom_requests_by_provider{provider}` | counter | Requests per provider (from Aperture metadata). |
| `headroom_requests_by_model{model}` | counter | Requests per model (from Aperture metadata). |
| `headroom_inbound_requests_total` / `_completed_total` / `_active` | counter/gauge | Inbound HTTP requests to the hook handler; `_active` is the live in-flight count. |

The `tsheadroom_*` metrics have no Headroom analog and describe this service specifically:

| metric | type | meaning |
|---|---|---|
| `tsheadroom_pool_slots_total` / `tsheadroom_pool_slots_busy` | gauge | Worker-pool size and how many slots are compressing right now. When `busy` sits at `total`, the pool is saturated and new requests queue (see [ML model loading and timeouts](#ml-model-loading-and-timeouts)). |
| `tsheadroom_affinity_hits_total` / `tsheadroom_affinity_spills_total` | counter | Session-affinity routing outcomes. A *hit* is a request sent to its conversation's warm worker; a *spill* is one whose worker was busy and went elsewhere (a likely cold recompress). A falling hit rate signals worker contention. See [Worker affinity](#worker-affinity). |
| `tsheadroom_requests_by_outcome{outcome}` | counter | Requests by outcome: `modify`, `noop`, `error`, `passthrough`, `read_error`. |
| `tsheadroom_cold_starts_total` | counter | Worker first-real-call events that paid the one-time ML model load. |
| `tsheadroom_tokens_after_total` | counter | Output tokens after compression (`tokens_after`), so you can compute the realized ratio. |

> **Why a subset?** tsheadroom is a `pre_request` hook, not a proxy: it runs `headroom.compress()` and never sees the upstream response. So Headroom proxy metrics that depend on the response or on prompt caching — completion tokens, TTFB, end-to-end latency, cache reads/writes and busts, per-transform timing, waste signals — have no source here and are deliberately omitted rather than reported as misleading zeros.

> **Access**: like `/config`, `/metrics` is gated only by your tailnet policy file. Anyone who can reach the device can read it; it exposes aggregate counts and model/provider names, not request contents.

## Worker affinity

Headroom's `compress()` keeps a per-process cache of already-compressed content (a process-global pipeline with a content-keyed cache, ~30-minute TTL). So within one worker, a growing conversation does **not** recompress its unchanged prefix every turn — only genuinely new content runs through the model. In practice the same large tool result costs tens of milliseconds the first time a worker sees it and ~1 ms on later turns.

That cache only helps if a conversation's turns keep landing on the **same** worker. tsheadroom therefore routes by conversation: each request is dispatched to a worker chosen from its `session_id` (from Aperture's hook metadata; when absent, a stable hash of the opening messages). Each worker has a one-deep affinity queue, so back-to-back turns reliably reuse the warm worker; if that worker is already busy with a queued job, the request **spills** to any free worker (a likely cold recompress) rather than blocking. `tsheadroom_affinity_hits_total` / `tsheadroom_affinity_spills_total` expose the split.

This is best-effort by design: under sustained load, spills route to cold workers and erode the benefit. The planned next step is bounded per-worker queueing so a turn waits a short, capped time for its warm worker instead of spilling. Pass `-no-affinity` to disable routing entirely and dispatch every request to any free worker.

### ML model loading and timeouts

The ML text compressor loads a ~600 MB model on first use (one-time, then cached on disk and resident in each worker). This load takes several seconds; a large, late-session conversation can itself take a few seconds to compress. There is one worker timeout, and the client-facing wait is owned by Aperture:

- **`-max-compress` (60s)** bounds the *worker*: a single call may run this long before the worker is treated as wedged and recycled. tsheadroom imposes no shorter deadline: a slow call runs to completion (the model finishes loading, the worker stays warm) and the result is used if Aperture is still waiting.
- **Aperture's hook `timeout`** bounds the *client*: when it fires, Aperture (with `fail_open`) forwards the original request uncompressed. This is the only client-facing latency ceiling, and it lives in the Aperture configuration. There is no tsheadroom-side deadline to keep in sync.

The result: a cold worker pays at most one slow (or, behind Aperture's timeout, uncompressed `allow`) request while the model loads, then it works, with no restart needed. To avoid even that, workers preload the model at startup whenever the persisted configuration could route content through the ML compressor. With `headroom-ai[ml]` installed this is the default: `compress_system_messages` is on and Headroom uses the ML model as its fallback for tool/mixed content, so it's needed under ordinary traffic, and workers come up warm. (Each preloaded worker holds its own resident copy of the model, which is why `-pool-size` defaults low.)

The model downloads from HuggingFace on first ever use (cached under `~/.cache/huggingface` thereafter). transformers revalidates that cache against the Hub on each cold load; tsheadroom skips that network round-trip (and its anonymous rate-limiting) by running workers offline once the model is cached (`HF_HUB_OFFLINE`/`TRANSFORMERS_OFFLINE`, set automatically; your own values are respected). To stay online with higher rate limits (for example, to let a fresh host download the model), set `HF_TOKEN` in the environment; it takes precedence over the automatic offline mode. A worker has ~60s to report ready, so a slow *cold* download on a fresh host can exceed that and make workers retry: warm the cache once (start a worker with the cache empty, or run a single `compress` under the worker's interpreter) before serving production traffic.

## What gets compressed

By default Headroom uses a conservative coding-agent profile: it compresses tool outputs (`tool_result` blocks / `role:"tool"` messages) and other bulky content, but protects user messages, the system prompt, and the most recent turns. So short chats (even long *prose* in a user message) return `allow`. You see `modify` when a request carries a substantial tool result.

tsheadroom only inspects the `messages` array (Anthropic/OpenAI shape). A request body without one (for example, Gemini's `contents`, or an embeddings call) passes through unchanged (`allow`); only `messages` is ever rewritten.

## Security and data handling

- **What tsheadroom sees**: the full plaintext request body of every matched LLM call, including prompts, tool results, and everything in `messages`. It processes them in memory and does not persist request content to disk.
- **Reachability**: tsheadroom has no auth of its own. Anyone who can reach the device over your tailnet can call the hook and read/modify its `/config`. Restrict access with [grants](https://tailscale.com/docs/features/access-control/grants), and give the device a [tag](https://tailscale.com/docs/features/tags) so you can target it: a tag is the device's identity at authentication time and serves as the selector grants match on (for example, `tag:tsheadroom` in a grant's `src`/`dst`).
- **Egress**: the only outbound network call is the one-time HuggingFace model download, which you can eliminate by pre-seeding the cache and running offline (see above). Compression itself makes no network calls.
- **Secrets**: `-state-dir` holds the device's private key, so keep it on durable, protected storage.

## Development

```shell
make            # build ./build/tsheadroom (Linux)
make test-go    # Go tests only: fake worker, no Python needed
make test       # Go tests + Python tests (Python tests run without headroom-ai;
                # the real-headroom integration test self-skips unless installed)
make test PYTHON=/opt/tsheadroom/venv/bin/python  # also runs the real-headroom integration test
```

Go tests use a fake worker (no Python needed). Python tests use a fake `headroom` by default and run the real-headroom integration test only when `$PYTHON` has `headroom-ai` installed.

## Bugs

tsheadroom is not affiliated with [Headroom](https://github.com/chopratejas/headroom). File bugs about the `compress()` function and its compression behavior there. For bugs in tsheadroom (Aperture integration, request/response parsing, or program behavior), use the [tsheadroom issue tracker](https://github.com/tailscale/tsheadroom/issues).

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). We require a DCO `Signed-off-by` line on commits (`git commit -s`), and every source file carries the Tailscale copyright/SPDX header (enforced by `TestLicenseHeaders`). Follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report security issues privately per [SECURITY.md](SECURITY.md).

## License

BSD 3-Clause. See [LICENSE](LICENSE).
