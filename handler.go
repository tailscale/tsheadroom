// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// maxBody matches aperture's maxRequestBodySize (capture.go): a modified body
// can be as large as the request itself, so we accept what aperture accepts.
const maxBody = 50 << 20 // 50 MiB

// hookCallData is the subset of aperture's HookCallData we consume. The hook is
// configured with send: ["request_body"], so that is the only field we read.
type hookCallData struct {
	RequestBody json.RawMessage `json:"request_body"`
}

// guardrailResponse is aperture's GuardrailResponse. We only ever emit "allow"
// or "modify" — never "block" — and always with HTTP 200.
//
// Why never "block": Headroom's compress() is best-effort and has no "halt this
// request" signal. It never raises on an over-limit conversation; it just
// returns the most-compressed messages it can, even if they still exceed the
// model's context window. So nothing compress() returns ever means "refuse" —
// every compress error or shortfall collapses back to "allow" (forward the
// original, unchanged). If a conversation is genuinely too big even after
// compression, we forward it and the upstream provider returns its own native,
// model-tailored "prompt is too long" error, which request/response clients
// (Claude Code and friends) already recover from by auto-compacting and retrying.
//
// WHERE THIS WOULD BREAK — and why it can't today: the above assumes the client
// recovers from the provider's overflow error. A streaming/WebSocket client of
// the kind Headroom's own proxy deliberately fail-CLOSES for — one that decides
// when to compact from the upstream-reported token usage that our compression
// deflates, and that treats a mid-stream refuse (1009/413) as a fatal connection
// error — could instead lock up when we fail open on an oversized frame. We
// cannot hit that case: Aperture's hook protocol is request/response only (one
// discrete HTTP POST per call; no WebSocket, no incremental frame delivery), so
// no such client exists on this path. THERE ARE NONE TODAY. If Aperture ever
// grows WebSocket/streaming hook support, revisit this: we may then need a real
// "block" path that mimics the provider's overflow error shape so those clients
// compact instead of hanging.
type guardrailResponse struct {
	Action      string `json:"action"`
	RequestBody any    `json:"request_body,omitempty"`
}

// compressor is the dependency the handler needs from the worker pool. An
// interface (rather than *Pool) lets tests exercise every handler branch with a
// fake, without spawning workers.
type compressor interface {
	Compress(ctx context.Context, req compressRequest) (*compressResult, error)
}

// Handler implements the aperture pre_request guardrail contract: it compresses
// request_body.messages and returns a modify action, degrading to allow on any
// problem so it can never break a user's request.
type Handler struct {
	comp     compressor
	settings *settingsStore // current compress knobs, read per request
	log      *slog.Logger   // operational logs + warnings (stderr)

	verbose bool      // when set, emit a per-request summary to out
	out     io.Writer // destination for verbose summaries (stdout)
}

// summary holds the numbers reported in the -v per-request line.
type summary struct {
	inMessages int // number of messages received
	inBytes    int // serialized size of received messages
	outBytes   int // serialized size of returned messages

	workerMs   float64 // worker-reported compress() time (0 when no worker result)
	cold       bool    // worker's first real request (paid the cold model load)
	modelLimit int     // context-window limit the worker compressed against (0 when no result)
	reason     string  // why this action was chosen: modify / allow(noop|error|passthrough|read-error)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	resp, s := h.process(r)

	if h.verbose {
		fmt.Fprintf(h.out, "request in_msgs=%d in_bytes=%d out_bytes=%d dur_ms=%d worker_ms=%.0f cold=%t model_limit=%d -> %s\n",
			s.inMessages, s.inBytes, s.outBytes, time.Since(start).Milliseconds(), s.workerMs, s.cold, s.modelLimit, s.reason)
	}
	writeJSON(w, http.StatusOK, resp)
}

// process reads the hook call, runs compression, and returns the guardrail
// response plus a summary for logging. Every failure path degrades to allow so
// the handler can never break a user's request.
func (h *Handler) process(r *http.Request) (guardrailResponse, summary) {
	allow := guardrailResponse{Action: "allow"}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		h.log.Warn("read request body failed; allowing", "err", err)
		return allow, summary{reason: "allow(read-error)"}
	}

	var data hookCallData
	if err := json.Unmarshal(body, &data); err != nil || len(data.RequestBody) == 0 {
		return allow, summary{reason: "allow(passthrough)"}
	}

	// request_body must be a JSON object so we can splice messages back and
	// return the whole thing (aperture rejects non-object modified bodies).
	var reqBody map[string]any
	if err := json.Unmarshal(data.RequestBody, &reqBody); err != nil {
		return allow, summary{reason: "allow(passthrough)"}
	}

	// v1 handles only the `messages` shape (Anthropic/OpenAI). Anything else
	// (e.g. Gemini's `contents`, embeddings) passes through untouched.
	rawMessages, ok := reqBody["messages"]
	if !ok {
		return allow, summary{reason: "allow(passthrough)"}
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return allow, summary{reason: "allow(passthrough)"}
	}
	model, _ := reqBody["model"].(string) // absent/non-string -> worker default

	// Byte sizes are only used for the -v summary; skip the marshal otherwise
	// (messages can be multi-MB). The message count is cheap, so keep it.
	// For an allow result, output size equals input size (body unchanged).
	s := summary{inMessages: len(messages)}
	if h.verbose {
		s.inBytes = jsonLen(messages)
		s.outBytes = s.inBytes
	}

	// No tsheadroom-side timeout: the wait is bounded by the request context
	// (aperture hangs up at its hook timeout) and by the pool's hard cap (a
	// runaway worker is recycled). A soft deadline here would only fail open
	// *before* aperture would have — abandoning exactly the slow, large-context
	// compressions this tool exists to perform. The latency ceiling belongs to
	// aperture's per-hook `timeout`, which is owned by the caller.
	res, err := h.comp.Compress(r.Context(), compressRequest{
		Messages: messages,
		Model:    model,
		Config:   h.settings.get(),
	})
	if err != nil {
		h.log.Warn("compress failed; allowing", "err", err)
		s.reason = "allow(error)"
		return allow, s
	}
	// Worker timing/cold/limit are available for both noop and modify; record
	// them so the -v line shows them regardless of the outcome.
	s.workerMs = res.ElapsedMs
	s.cold = res.ColdFirstCall
	s.modelLimit = res.ModelLimit
	if res.TokensSaved <= 0 {
		// No-op: nothing meaningful to change, so don't rewrite the body.
		s.reason = "allow(noop)"
		return allow, s
	}

	// Modify: replace only messages; system, tools, and every other top-level
	// field pass through unchanged. Headroom compresses tool_use/tool_result
	// content inside messages, so tool calls are already covered here.
	reqBody["messages"] = res.Messages
	if h.verbose {
		s.outBytes = jsonLen(res.Messages)
	}
	s.reason = "modify"
	return guardrailResponse{Action: "modify", RequestBody: reqBody}, s
}

// jsonLen returns the serialized byte length of v, or 0 if it can't be marshaled.
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}
