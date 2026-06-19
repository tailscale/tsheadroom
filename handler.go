// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBody matches aperture's maxRequestBodySize (capture.go): a modified body
// can be as large as the request itself, so we accept what aperture accepts.
const maxBody = 50 << 20 // 50 MiB

// hookCallData is the subset of aperture's HookCallData we consume. The hook is
// configured with send: ["request_body"], so request_body is gated on that; the
// metadata object is always sent regardless of send, which is where aperture
// puts the resolved provider and model.
type hookCallData struct {
	Metadata    hookMetadata    `json:"metadata"`
	RequestBody json.RawMessage `json:"request_body"`
}

// hookMetadata is the subset of aperture's always-present HookMetadata we use.
// provider and model are populated unconditionally by aperture (not gated by
// the hook's send config), so they're our authoritative source for both the
// per-provider/per-model metrics and the model passed to compress().
type hookMetadata struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
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
// Where this would break, and why it can't today: the above assumes the client
// recovers from the provider's overflow error. A streaming/WebSocket client of
// the kind Headroom's own proxy deliberately fail-closes for — one that decides
// when to compact from the upstream-reported token usage that our compression
// deflates, and that treats a mid-stream refuse (1009/413) as a fatal connection
// error — could instead lock up when we fail open on an oversized frame. We
// cannot hit that case: Aperture's hook protocol is request/response only (one
// discrete HTTP POST per call; no WebSocket, no incremental frame delivery), so
// no such client exists on this path. If Aperture ever
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
	metrics  *Metrics       // lifetime counters for /metrics (nil-safe; nil in tests)

	verbose bool      // when set, emit a per-request summary to out
	out     io.Writer // destination for verbose summaries (stdout)

	// gzipResponse, when set, gzips the modify/allow response if the caller
	// advertised Accept-Encoding: gzip. aperture's tsnet hook client uses a
	// standard http.Transport, which adds that header and transparently
	// decompresses the reply, so this needs no aperture-side change. Only the
	// response is compressed — the larger request body the hook receives arrives
	// uncompressed (aperture has no faculty to compress it).
	gzipResponse bool

	// acceptCompressed, when set, advertises Accept-Encoding (acceptEncoding) on
	// responses so aperture learns it accepts compressed request bodies and
	// compresses subsequent ones (RFC 7694). This is the lever that shrinks the
	// inbound transfer (read_ms) on warm calls. Decoding of an inbound body is
	// always attempted regardless of this flag — we never choke on a body
	// aperture sent — so clearing it only stops us advertising the capability.
	acceptCompressed bool
}

// summary holds the numbers reported in the -v per-request line and folded into
// /metrics.
type summary struct {
	inMessages int    // number of messages received
	inBytes    int    // serialized size of received messages
	outBytes   int    // serialized size of returned messages
	wireBytes  int    // bytes actually read off the wire (compressed, when the body arrived encoded)
	enc        string // inbound Content-Encoding we decoded ("" when identity/uncompressed)

	provider string // aperture-resolved provider (metrics label)
	model    string // aperture-resolved model (metrics label)

	tokensBefore int // res.TokensBefore (0 when no worker result)
	tokensSaved  int // res.TokensSaved (0 when no worker result)
	tokensAfter  int // res.TokensAfter (0 when no worker result)

	workerMs   float64 // worker-reported compress() time (0 when no worker result)
	cold       bool    // worker's first real request (paid the cold model load)
	modelLimit int     // context-window limit the worker compressed against (0 when no result)
	slot       int     // pool slot whose worker served this request (-1 when none ran)
	reason     string  // why this action was chosen: modify / allow(noop|error|passthrough|read-error)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.metrics.inboundStart()
	defer h.metrics.inboundDone()

	// Advertise which request-body codings we accept (RFC 7694). aperture reads
	// this off the response and compresses subsequent request bodies, shrinking
	// the inbound transfer that dominates warm-call latency. Set before any
	// write so it rides every response shape (200, 405, 415).
	if h.acceptCompressed {
		w.Header().Set("Accept-Encoding", acceptEncoding)
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Read the request body from aperture. Timed on its own: reading (and later
	// writing) the body is pure transfer cost, separate from compression.
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody))
	readMs := msSince(start)

	var resp guardrailResponse
	var s summary
	switch {
	case readErr != nil:
		h.log.Warn("read request body failed; allowing", "err", readErr)
		resp, s = guardrailResponse{Action: "allow"}, summary{reason: "allow(read-error)", slot: -1}
	default:
		// Decompress an inbound compressed body (RFC 7694). The Content-Encoding
		// is purely between aperture and us: on allow, aperture forwards its own
		// original body upstream, so a decode problem here can never harm the
		// user's request.
		wireBytes := len(body)
		enc := r.Header.Get("Content-Encoding")
		decoded, decErr := decodeRequestBody(body, enc)
		switch {
		case errors.Is(decErr, errUnsupportedEncoding):
			// We advertised a set aperture no longer matches (e.g. we dropped a
			// coding between calls). Reject with 415 + Accept-Encoding so it
			// downgrades to a coding we accept (or identity) and retries. This
			// is a cooperative downgrade signal, not a block: aperture proceeds
			// with the user's request uncompressed.
			h.log.Warn("rejecting unsupported request content-encoding", "encoding", enc)
			http.Error(w, "unsupported content-encoding", http.StatusUnsupportedMediaType)
			s = summary{reason: "reject(encoding)", enc: enc, wireBytes: wireBytes, slot: -1}
			if h.verbose {
				fmt.Fprintf(h.out, "%s wire_bytes=%d read_ms=%.0f -> %s\n",
					reqLabel(decodeName(enc)), wireBytes, readMs, s.reason)
			}
			h.metrics.record(s, msSince(start))
			return
		case decErr != nil:
			// Supported coding but the body won't decode (corruption): fail open.
			h.log.Warn("decode request body failed; allowing", "encoding", enc, "err", decErr)
			resp, s = guardrailResponse{Action: "allow"}, summary{reason: "allow(decode-error)", enc: enc, wireBytes: wireBytes, slot: -1}
		default:
			resp, s = h.process(r.Context(), decoded)
			s.enc = decodeName(enc)
			s.wireBytes = wireBytes
		}
	}

	// Write the response back to aperture — the other half of the transfer cost.
	writeStart := time.Now()
	h.writeHookResponse(w, r, resp)
	writeMs := msSince(writeStart)

	durMs := msSince(start) // total: read + process + write

	// read_ms (aperture -> us) and write_ms (us -> aperture) are the transfer
	// cost, split so a large gap localizes to the inbound or outbound side
	// (e.g. write_ms dominating points at back-pressure while aperture drains
	// our response). dur_ms - read_ms - write_ms - worker_ms is Go/IPC overhead.
	// The "request(<format>)" prefix tags the inbound compression when aperture
	// sent a compressed body; wire_bytes << in_bytes with a low read_ms is the
	// RFC 7694 path working.
	if h.verbose {
		fmt.Fprintf(h.out, "%s in_msgs=%d in_bytes=%d out_bytes=%d wire_bytes=%d dur_ms=%.0f read_ms=%.0f write_ms=%.0f worker_ms=%.0f slot=%d cold=%t model_limit=%d -> %s\n",
			reqLabel(s.enc), s.inMessages, s.inBytes, s.outBytes, s.wireBytes, durMs, readMs, writeMs, s.workerMs, s.slot, s.cold, s.modelLimit, s.reason)
	}
	h.metrics.record(s, durMs)
}

// reqLabel renders the -v line prefix, tagging the inbound compression format
// in parens when aperture sent a compressed body (e.g. "request(gzip)") and a
// plain "request" when the body arrived uncompressed.
func reqLabel(enc string) string {
	if enc == "" {
		return "request"
	}
	return "request(" + enc + ")"
}

// decodeName normalizes a Content-Encoding header value to the bare coding
// token we matched on (lowercased, q-value stripped), for the -v summary.
func decodeName(encoding string) string {
	enc := strings.ToLower(strings.TrimSpace(strings.SplitN(encoding, ";", 2)[0]))
	if enc == "identity" {
		return ""
	}
	return enc
}

// msSince returns milliseconds elapsed since t, with microsecond resolution.
func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

// writeHookResponse writes the guardrail response as JSON, gzipping it when
// enabled and the caller advertised gzip. The compressed bytes are built in a
// buffer first so a (vanishingly unlikely) encode error falls back to a plain
// response with nothing half-written. Always HTTP 200 — the action is in the body.
func (h *Handler) writeHookResponse(w http.ResponseWriter, r *http.Request, resp guardrailResponse) {
	w.Header().Set("Content-Type", "application/json")
	if h.gzipResponse && acceptsGzip(r) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		encErr := json.NewEncoder(gz).Encode(resp)
		closeErr := gz.Close()
		if encErr == nil && closeErr == nil {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf.Bytes())
			return
		}
		h.log.Warn("gzip response failed; sending uncompressed", "enc_err", encErr, "close_err", closeErr)
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// acceptsGzip reports whether the request's Accept-Encoding lists gzip.
func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		// Strip any q-value (e.g. "gzip;q=0.8") before comparing.
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(enc, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// process runs compression on an already-read hook body and returns the
// guardrail response plus a summary for logging. ctx bounds the compression
// wait (aperture's hook timeout / client disconnect). Every failure path
// degrades to allow so the handler can never break a user's request.
func (h *Handler) process(ctx context.Context, body []byte) (guardrailResponse, summary) {
	allow := guardrailResponse{Action: "allow"}

	var data hookCallData
	if err := json.Unmarshal(body, &data); err != nil || len(data.RequestBody) == 0 {
		return allow, summary{reason: "allow(passthrough)", slot: -1}
	}

	// request_body must be a JSON object so we can splice messages back and
	// return the whole thing (aperture rejects non-object modified bodies).
	// Parse only the top level — values stay as raw bytes, so untouched fields
	// (system, tools, ...) pass through verbatim and are never re-marshaled.
	var reqBody map[string]json.RawMessage
	if err := json.Unmarshal(data.RequestBody, &reqBody); err != nil {
		return allow, summary{reason: "allow(passthrough)", provider: data.Metadata.Provider, model: data.Metadata.Model, slot: -1}
	}

	// v1 handles only the `messages` shape (Anthropic/OpenAI). Anything else
	// (e.g. Gemini's `contents`, embeddings) passes through untouched. The
	// top-level array is split into raw elements (a shallow parse that does not
	// recurse into each message) — enough for the count and the affinity key,
	// while the bytes pass to the worker unparsed.
	rawMessages, ok := reqBody["messages"]
	if !ok {
		return allow, summary{reason: "allow(passthrough)", provider: data.Metadata.Provider, model: data.Metadata.Model, slot: -1}
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return allow, summary{reason: "allow(passthrough)", provider: data.Metadata.Provider, model: data.Metadata.Model, slot: -1}
	}
	// Prefer aperture's resolved metadata.model (always present, authoritative);
	// fall back to the request body's own model field. Empty -> worker default.
	model := data.Metadata.Model
	if model == "" {
		if raw, ok := reqBody["model"]; ok {
			_ = json.Unmarshal(raw, &model)
		}
	}

	// Byte sizes are only used for the -v summary. in_bytes is the original
	// messages length, free from the raw bytes we already hold.
	s := summary{inMessages: len(msgs), provider: data.Metadata.Provider, model: model, slot: -1}
	if h.verbose {
		s.inBytes = len(rawMessages)
		s.outBytes = s.inBytes
	}

	// No tsheadroom-side timeout: the wait is bounded by the request context
	// (aperture hangs up at its hook timeout) and by the pool's hard cap (a
	// runaway worker is recycled). A soft deadline here would only fail open
	// *before* aperture would have — abandoning exactly the slow, large-context
	// compressions this tool exists to perform. The latency ceiling belongs to
	// aperture's per-hook `timeout`, which is owned by the caller.
	//
	// Messages pass straight through as raw bytes — no parse into Go values and
	// no re-marshal; the worker receives the original JSON.
	res, err := h.comp.Compress(ctx, compressRequest{
		Messages:    rawMessages,
		Model:       model,
		Config:      h.settings.get(),
		AffinityKey: affinityKey(data.Metadata.SessionID, msgs),
	})
	if err != nil {
		h.log.Warn("compress failed; allowing", "err", err)
		s.reason = "allow(error)"
		return allow, s
	}
	// Worker timing/cold/limit, slot, and token counts are available for both
	// noop and modify; record them so the -v line and /metrics reflect them
	// regardless of the outcome.
	s.workerMs = res.ElapsedMs
	s.cold = res.ColdFirstCall
	s.modelLimit = res.ModelLimit
	s.slot = res.Slot
	s.tokensBefore = res.TokensBefore
	s.tokensSaved = res.TokensSaved
	s.tokensAfter = res.TokensAfter
	if res.TokensSaved <= 0 || len(res.Messages) == 0 {
		// No-op (or, defensively, an empty result that would null out the body):
		// don't rewrite the body.
		s.reason = "allow(noop)"
		return allow, s
	}

	// Modify: replace only messages; system, tools, and every other top-level
	// field pass through unchanged (as their original raw bytes). Headroom
	// compresses tool_use/tool_result content inside messages, so tool calls are
	// already covered. The worker's compressed messages are already JSON — splice
	// them in directly (no marshal), and out_bytes is their length for free.
	reqBody["messages"] = res.Messages
	if h.verbose {
		s.outBytes = len(res.Messages)
	}
	s.reason = "modify"
	return guardrailResponse{Action: "modify", RequestBody: reqBody}, s
}

// affinityKey returns a routing key that pins a conversation to one worker so
// headroom's per-process compression cache stays warm across its turns. It
// prefers aperture's session_id (always sent in metadata, stable per session);
// when absent it falls back to a hash of the opening messages (system + first
// user message), which is stable across a conversation's turns yet distinct
// between conversations. Empty only when there are no messages at all.
func affinityKey(sessionID string, messages []json.RawMessage) string {
	if sessionID != "" {
		return sessionID
	}
	if len(messages) == 0 {
		return ""
	}
	h := fnv.New64a()
	for i, m := range messages {
		if i >= 2 { // system + first user message is enough to distinguish
			break
		}
		// Normalize each of the opening 1-2 messages (parse + re-marshal sorts
		// object keys) so a client's incidental key reordering between turns
		// doesn't change the key. Cheap — it's at most two small messages, not
		// the whole array. Fall back to the raw bytes if a message won't parse.
		var v any
		if json.Unmarshal(m, &v) == nil {
			b, _ := json.Marshal(v)
			_, _ = h.Write(b)
		} else {
			_, _ = h.Write(m)
		}
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
