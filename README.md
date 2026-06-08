# tsheadroom

A [Headroom](https://pypi.org/project/headroom-ai/) context-compression layer
exposed as an [Aperture](https://github.com/tailscale/aperture) **`pre_request`
guardrail hook**.

tsheadroom runs as a node on your tailnet (via
[`tsnet`](https://tailscale.com/kb/1244/tsnet)). For each LLM request Aperture
forwards to it, tsheadroom hands the request's `messages` to Headroom's
`compress()` — which crushes bulky, low-information content (large tool outputs,
search results, logs) while leaving prompts and recent turns intact — and
returns a `modify` action with the compressed body. If there's nothing
worthwhile to compress, or anything goes wrong, it returns `allow` and the
request passes through unchanged. **It never blocks a request.**

## How it works

```
Aperture --(pre_request hook: POST /)--> tsheadroom (tsnet :80)
                                              │
                                              ├─ pool of persistent Python workers
                                              │     each runs: headroom.compress(messages, model)
                                              │
                                              └─ reply: {"action":"modify","request_body":{…}}
                                                     or  {"action":"allow"}
```

The Go binary owns a small pool of long-lived Python worker processes (so the
Headroom pipeline is built once, not per request) and supervises them
(auto-restart on crash, fail-open on timeout). All Headroom calls happen
out-of-process, so a worker fault can never take down the listener.

## a) Dependencies

To run tsheadroom on a system you need:

1. **The `tsheadroom` binary.** Build it with **Go 1.26.4+** (required by the
   pinned Tailscale dependency):
   ```bash
   git clone git@github.com:tailscale/tsheadroom.git
   cd tsheadroom
   make            # produces ./build/tsheadroom
   ```
2. **Python 3 with `headroom-ai` installed.** This is the interpreter tsheadroom
   launches its workers under (the `-python` flag). A virtualenv is recommended:
   ```bash
   python3 -m venv /opt/tsheadroom/venv
   /opt/tsheadroom/venv/bin/pip install headroom-ai
   ```
   `headroom-ai` ships prebuilt wheels, so no Rust toolchain is needed.
3. **`worker.py`** from this repo (the `-worker` flag points at it).
4. **A Tailscale auth key** (`TS_AUTHKEY`) so the node can join your tailnet
   unattended. Generate one in the Tailscale admin console → Settings → Keys.

## b) Running tsheadroom

```bash
TS_AUTHKEY=tskey-auth-xxxx ./build/tsheadroom \
  -hostname tsheadroom \
  -python /opt/tsheadroom/venv/bin/python \
  -worker /opt/tsheadroom/worker.py \
  -state-dir /var/lib/tsheadroom
```

This joins the tailnet as `tsheadroom` and listens for hook calls on `:80` at
path `/`. Other tailnet nodes (including Aperture) reach it at
`http://tsheadroom.<your-tailnet>.ts.net/`.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-python` | `python3` | Interpreter with `headroom-ai` installed (used to launch workers). |
| `-worker` | `worker.py` | Path to the worker script. |
| `-hostname` | `tsheadroom` | Node name on the tailnet. |
| `-pool-size` | `max(4, GOMAXPROCS)` | Number of persistent Python workers. |
| `-deadline` | `4s` | Per-request fail-open deadline. Keep it under Aperture's hook `timeout`. |
| `-addr` | `:80` | Listen address on the tsnet node. |
| `-state-dir` | tsnet default | tsnet state directory. **Persist this** — it holds the node identity (`tailscaled.state`). See note below. |
| `-local-addr` | (off) | Serve plain HTTP here instead of tsnet — for local testing only. |
| `-v` | off | Log a one-line per-request summary (`in/out` sizes, `modify`/`allow`) to stdout. |

`TS_AUTHKEY` (environment) provides the tailnet auth key on first start.

**State:** `-state-dir` must be a stable, writable, persistent path (e.g. a
systemd `StateDirectory`). It contains `tailscaled.state`, the node's **private
key** — treat it as a secret and keep it on durable storage, or the node
re-authenticates as a new node on every restart.

### Quick local test (no tailnet)

```bash
./build/tsheadroom -local-addr 127.0.0.1:8080 -v \
  -python /opt/tsheadroom/venv/bin/python -worker ./worker.py
```

```bash
# A large tool result is what Headroom compresses (chat/prose is protected).
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

## c) Aperture configuration

tsheadroom is wired in as a `pre_request` hook in two places in your Aperture
config (`config.hujson`):

**1. Define the hook** in the top-level `hooks` map, pointing at your tsheadroom
node:

```hujson
"hooks": {
  "headroom": {
    "url": "http://tsheadroom.<your-tailnet>.ts.net/",
    "fail_policy": "fail_open", // default; if tsheadroom is unreachable, send uncompressed
    "timeout": "5s",            // must be >= tsheadroom's -deadline (4s)
  },
},
```

**2. Attach it to a grant** via `send_hooks`, firing on `pre_request` and sending
the request body (the only field tsheadroom reads):

```hujson
{
  "src": ["*"],
  "app": {
    "tailscale.com/cap/aperture": [
      {
        "models": "**",            // compress all models, or scope e.g. "anthropic/**"
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

Notes:
- **`send: ["request_body"]` is required** — it's the only input tsheadroom uses
  (the model name is read from inside the body).
- **`fail_open` is the right policy.** tsheadroom always answers `200` with
  `allow`/`modify` and never blocks; `fail_open` ensures that if the node is
  *unreachable*, requests still proceed (just uncompressed).
- Set the hook **`timeout` ≥ tsheadroom's `-deadline`** so tsheadroom's own
  fail-open fires before Aperture times the call out.
- Scope `models` to target specific providers, and use `preference` if you stack
  it with other guardrail hooks.

## What actually gets compressed

By default Headroom uses a conservative coding-agent profile: it compresses
**tool outputs** (`tool_result` blocks / `role:"tool"` messages) and other bulky
content, but **protects user messages, the system prompt, and the most recent
turns**. So short chats — even long *prose* in a user message — return `allow`.
You'll see `modify` when a request carries a substantial tool result.

## Development

```bash
make            # build ./build/tsheadroom
make test       # Go tests + Python tests
make test PYTHON=/opt/tsheadroom/venv/bin/python  # also runs the real-headroom integration test
```

Go tests use a fake worker (no Python needed); Python tests use a fake
`headroom` by default and run the real-headroom integration test only when
`$PYTHON` has `headroom-ai` installed.
