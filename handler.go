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
	deadline time.Duration
	log      *slog.Logger // operational logs + warnings (stderr)

	verbose bool      // when set, emit a per-request summary to out
	out     io.Writer // destination for verbose summaries (stdout)
}

// summary holds the numbers reported in the -v per-request line.
type summary struct {
	inMessages int // number of messages received
	inBytes    int // serialized size of received messages
	outBytes   int // serialized size of returned messages
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp, s := h.process(r)

	if h.verbose {
		fmt.Fprintf(h.out, "request in_msgs=%d in_bytes=%d out_bytes=%d -> %s\n",
			s.inMessages, s.inBytes, s.outBytes, resp.Action)
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
		return allow, summary{}
	}

	var data hookCallData
	if err := json.Unmarshal(body, &data); err != nil || len(data.RequestBody) == 0 {
		return allow, summary{}
	}

	// request_body must be a JSON object so we can splice messages back and
	// return the whole thing (aperture rejects non-object modified bodies).
	var reqBody map[string]any
	if err := json.Unmarshal(data.RequestBody, &reqBody); err != nil {
		return allow, summary{}
	}

	// v1 handles only the `messages` shape (Anthropic/OpenAI). Anything else
	// (e.g. Gemini's `contents`, embeddings) passes through untouched.
	rawMessages, ok := reqBody["messages"]
	if !ok {
		return allow, summary{}
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return allow, summary{}
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

	ctx, cancel := context.WithTimeout(r.Context(), h.deadline)
	defer cancel()

	res, err := h.comp.Compress(ctx, compressRequest{
		Messages: messages,
		Model:    model,
		Config:   h.settings.get(),
	})
	if err != nil {
		h.log.Warn("compress failed; allowing", "err", err)
		return allow, s
	}
	if res.TokensSaved <= 0 {
		// No-op: nothing meaningful to change, so don't rewrite the body.
		return allow, s
	}

	// Modify: replace only messages; system, tools, and every other top-level
	// field pass through unchanged. Headroom compresses tool_use/tool_result
	// content inside messages, so tool calls are already covered here.
	reqBody["messages"] = res.Messages
	if h.verbose {
		s.outBytes = jsonLen(res.Messages)
	}
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
