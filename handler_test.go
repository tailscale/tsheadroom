// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// fakeCompressor lets each test dictate the pool's behavior.
type fakeCompressor struct {
	fn func(ctx context.Context, req compressRequest) (*compressResult, error)
}

func (f fakeCompressor) Compress(ctx context.Context, req compressRequest) (*compressResult, error) {
	return f.fn(ctx, req)
}

func newTestHandler(fn func(ctx context.Context, req compressRequest) (*compressResult, error)) *Handler {
	return &Handler{
		comp:             fakeCompressor{fn: fn},
		settings:         loadSettings("", quietLog()),
		log:              quietLog(),
		out:              io.Discard,
		acceptCompressed: true,
	}
}

// doHook posts a hook body and returns the decoded guardrail response.
func doHook(t *testing.T, h *Handler, body string) guardrailResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (handler must always 2xx)", rec.Code)
	}
	var resp guardrailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
	}
	return resp
}

func TestHandler_ModifyOnSavings(t *testing.T) {
	var gotReq compressRequest
	h := newTestHandler(func(_ context.Context, req compressRequest) (*compressResult, error) {
		gotReq = req
		return &compressResult{
			Messages:    json.RawMessage(`[{"role":"user","content":"small"}]`),
			TokensSaved: 1234,
		}, nil
	})

	body := `{"request_body":{"model":"gpt-4o","system":"sys","tools":[{"name":"ls"}],
		"messages":[{"role":"user","content":"big"}]}}`
	resp := doHook(t, h, body)

	if resp.Action != "modify" {
		t.Fatalf("action = %q, want modify", resp.Action)
	}
	// Model was forwarded to the compressor.
	if gotReq.Model != "gpt-4o" {
		t.Errorf("forwarded model = %q, want gpt-4o", gotReq.Model)
	}
	// Non-messages fields must survive untouched; messages must be replaced.
	rb, ok := resp.RequestBody.(map[string]any)
	if !ok {
		t.Fatalf("request_body type = %T, want object", resp.RequestBody)
	}
	if rb["model"] != "gpt-4o" || rb["system"] != "sys" {
		t.Errorf("top-level fields not preserved: %+v", rb)
	}
	if _, ok := rb["tools"]; !ok {
		t.Errorf("tools field dropped: %+v", rb)
	}
	msgs, ok := rb["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages not replaced with compressed set: %+v", rb["messages"])
	}
	if m := msgs[0].(map[string]any); m["content"] != "small" {
		t.Errorf("messages not the compressed ones: %+v", m)
	}
}

func TestHandler_AllowBranches(t *testing.T) {
	cases := []struct {
		name string
		body string
		// result/err returned by the fake; nil fn means compressor must NOT be called
		fn func(context.Context, compressRequest) (*compressResult, error)
	}{
		{
			name: "no-op zero savings",
			body: `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}}`,
			fn: func(context.Context, compressRequest) (*compressResult, error) {
				return &compressResult{Messages: json.RawMessage(`[]`), TokensSaved: 0}, nil
			},
		},
		{
			name: "compressor error fails open",
			body: `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}}`,
			fn: func(context.Context, compressRequest) (*compressResult, error) {
				return nil, errors.New("worker exploded")
			},
		},
		{
			name: "no messages field (gemini contents)",
			body: `{"request_body":{"model":"gemini-2.5-pro","contents":[{"parts":[]}]}}`,
			fn:   nil,
		},
		{
			name: "messages not an array",
			body: `{"request_body":{"messages":"nope"}}`,
			fn:   nil,
		},
		{
			name: "no request_body",
			body: `{"metadata":{"model":"x"}}`,
			fn:   nil,
		},
		{
			name: "request_body not an object",
			body: `{"request_body":[1,2,3]}`,
			fn:   nil,
		},
		{
			name: "malformed json",
			body: `not json at all`,
			fn:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			fn := tc.fn
			h := newTestHandler(func(ctx context.Context, req compressRequest) (*compressResult, error) {
				called = true
				if fn == nil {
					t.Fatalf("compressor should not be called for %q", tc.name)
				}
				return fn(ctx, req)
			})
			resp := doHook(t, h, tc.body)
			if resp.Action != "allow" {
				t.Fatalf("action = %q, want allow", resp.Action)
			}
			if resp.RequestBody != nil {
				t.Errorf("allow must not carry a request_body, got %+v", resp.RequestBody)
			}
			if tc.fn == nil && called {
				t.Errorf("compressor unexpectedly called")
			}
		})
	}
}

func TestHandler_VerboseSummary(t *testing.T) {
	h := newTestHandler(func(context.Context, compressRequest) (*compressResult, error) {
		return &compressResult{Messages: json.RawMessage(`["x"]`), TokensSaved: 10}, nil
	})
	var buf bytes.Buffer
	h.verbose = true
	h.out = &buf

	doHook(t, h, `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"big"}]}}`)

	line := buf.String()
	if !strings.HasPrefix(line, "request ") {
		t.Errorf("uncompressed request should have plain %q prefix, got %q", "request ", line)
	}
	for _, want := range []string{"in_msgs=1", "in_bytes=", "out_bytes=", "dur_ms=", "read_ms=", "write_ms=", "worker_ms=", "slot=", "-> modify"} {
		if !strings.Contains(line, want) {
			t.Errorf("verbose line %q missing %q", line, want)
		}
	}

	// allow path should also be summarized
	buf.Reset()
	h2 := newTestHandler(func(context.Context, compressRequest) (*compressResult, error) {
		return &compressResult{TokensSaved: 0}, nil
	})
	h2.verbose = true
	h2.out = &buf
	doHook(t, h2, `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}}`)
	if !strings.Contains(buf.String(), "-> allow") {
		t.Errorf("verbose line %q missing %q", buf.String(), "-> allow")
	}
}

func TestHandler_ForwardsSettings(t *testing.T) {
	var gotConfig CompressSettings
	h := newTestHandler(func(_ context.Context, req compressRequest) (*compressResult, error) {
		gotConfig = req.Config
		return &compressResult{Messages: json.RawMessage(`["x"]`), TokensSaved: 1}, nil
	})
	// Tune the store; the handler should forward the snapshot per request.
	tuned := defaultSettings()
	tuned.CompressUserMessages = true
	tuned.ProtectRecent = 1
	if err := h.settings.set(tuned); err != nil {
		t.Fatalf("set settings: %v", err)
	}

	doHook(t, h, `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}}`)

	if !gotConfig.CompressUserMessages || gotConfig.ProtectRecent != 1 {
		t.Fatalf("settings not forwarded to compressor: %+v", gotConfig)
	}
}

func TestHandler_RejectsNonPost(t *testing.T) {
	h := newTestHandler(func(context.Context, compressRequest) (*compressResult, error) {
		t.Fatal("compressor must not run for GET")
		return nil, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestAffinityKey(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"you are a coding agent"}`),
		json.RawMessage(`{"role":"user","content":"fix the bug in pool.go"}`),
		json.RawMessage(`{"role":"assistant","content":"on it"}`),
	}

	// session_id, when present, is the key verbatim.
	if got := affinityKey("sess-123", msgs); got != "sess-123" {
		t.Errorf("with session_id: got %q, want sess-123", got)
	}

	// Without session_id, the key is derived from the opening messages and is
	// stable across turns of the same conversation (more messages appended).
	grown := append(append([]json.RawMessage{}, msgs...), json.RawMessage(`{"role":"user","content":"and add a test"}`))
	k1 := affinityKey("", msgs)
	k2 := affinityKey("", grown)
	if k1 == "" {
		t.Fatal("fallback key should be non-empty")
	}
	if k1 != k2 {
		t.Errorf("key not stable across turns: %q vs %q", k1, k2)
	}

	// Key order in the opening messages must not change the key (normalized).
	reordered := []json.RawMessage{
		json.RawMessage(`{"content":"you are a coding agent","role":"system"}`),
		json.RawMessage(`{"content":"fix the bug in pool.go","role":"user"}`),
	}
	if affinityKey("", reordered) != affinityKey("", msgs[:2]) {
		t.Errorf("key changed under object-key reordering")
	}

	// A different conversation (different first user message) gets a different key.
	other := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"you are a coding agent"}`),
		json.RawMessage(`{"role":"user","content":"write the README"}`),
	}
	if affinityKey("", other) == k1 {
		t.Errorf("distinct conversations collided on key %q", k1)
	}

	// No messages and no session_id -> empty (pool falls back to shared dispatch).
	if got := affinityKey("", nil); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}

func TestHandler_GzipResponse(t *testing.T) {
	makeH := func(gzipOn bool) *Handler {
		h := newTestHandler(func(_ context.Context, _ compressRequest) (*compressResult, error) {
			return &compressResult{Messages: json.RawMessage(`[{"role":"user","content":"small"}]`), TokensSaved: 5}, nil
		})
		h.gzipResponse = gzipOn
		return h
	}
	body := `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"big"}]}}`

	post := func(h *Handler, acceptGzip bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if acceptGzip {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec
	}

	// Enabled + client advertises gzip -> compressed; decompresses to modify.
	rec := post(makeH(true), true)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	var resp guardrailResponse
	if err := json.NewDecoder(gz).Decode(&resp); err != nil {
		t.Fatalf("decode gzipped body: %v", err)
	}
	if resp.Action != "modify" {
		t.Errorf("gzipped action = %q, want modify", resp.Action)
	}

	// Enabled but client doesn't advertise gzip -> plain JSON.
	rec = post(makeH(true), false)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty when client didn't accept gzip", got)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode plain body: %v", err)
	}

	// Disabled flag overrides Accept-Encoding -> plain JSON.
	rec = post(makeH(false), true)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty when gzip disabled", got)
	}
}

// gzipBytes and zstdBytes encode b with the named coding for request-body tests.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// TestHandler_InboundDecode covers the RFC 7694 receive side: a compressed
// request body is decoded before compression, an unadvertised coding is
// rejected with 415 + Accept-Encoding, a corrupt body fails open, and every
// response advertises what we accept.
func TestHandler_InboundDecode(t *testing.T) {
	const plain = `{"request_body":{"model":"gpt-4o","messages":[{"role":"user","content":"big"}]}}`

	// post sends body with the given Content-Encoding (empty = none) and
	// returns the recorder after ServeHTTP.
	post := func(h *Handler, encoding string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	modifyH := func() *Handler {
		return newTestHandler(func(_ context.Context, req compressRequest) (*compressResult, error) {
			// The worker must receive the decoded plaintext messages.
			if !json.Valid(req.Messages) {
				t.Errorf("worker got non-JSON messages: %q", req.Messages)
			}
			return &compressResult{Messages: json.RawMessage(`[{"role":"user","content":"small"}]`), TokensSaved: 5}, nil
		})
	}

	for _, tc := range []struct {
		name     string
		encoding string
		encode   func(*testing.T, []byte) []byte
	}{
		{"gzip", "gzip", gzipBytes},
		{"zstd", "zstd", zstdBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := modifyH()
			var buf bytes.Buffer
			h.verbose, h.out = true, &buf
			rec := post(h, tc.encoding, tc.encode(t, []byte(plain)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Accept-Encoding"); got != acceptEncoding {
				t.Errorf("Accept-Encoding = %q, want %q", got, acceptEncoding)
			}
			// The -v line tags the inbound format in parens on the prefix.
			if want := "request(" + tc.encoding + ") "; !strings.HasPrefix(buf.String(), want) {
				t.Errorf("verbose line %q missing prefix %q", buf.String(), want)
			}
			var resp guardrailResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Action != "modify" {
				t.Errorf("action = %q, want modify (decoded body must reach compressor)", resp.Action)
			}
		})
	}

	// Identity / no encoding still works and still advertises.
	t.Run("identity", func(t *testing.T) {
		rec := post(modifyH(), "", []byte(plain))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Accept-Encoding"); got != acceptEncoding {
			t.Errorf("Accept-Encoding = %q, want %q", got, acceptEncoding)
		}
	})

	// Unadvertised coding -> 415 with Accept-Encoding so aperture downgrades.
	t.Run("unsupported -> 415", func(t *testing.T) {
		h := newTestHandler(func(context.Context, compressRequest) (*compressResult, error) {
			t.Error("compressor must not be called on a rejected encoding")
			return nil, nil
		})
		rec := post(h, "br", []byte("whatever"))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", rec.Code)
		}
		if got := rec.Header().Get("Accept-Encoding"); got != acceptEncoding {
			t.Errorf("415 Accept-Encoding = %q, want %q", got, acceptEncoding)
		}
	})

	// Supported coding but a corrupt body -> fail open (allow, 200), never block.
	t.Run("corrupt body fails open", func(t *testing.T) {
		h := newTestHandler(func(context.Context, compressRequest) (*compressResult, error) {
			t.Error("compressor must not be called when the body can't be decoded")
			return nil, nil
		})
		rec := post(h, "gzip", []byte("not actually gzip"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (handler must always 2xx)", rec.Code)
		}
		var resp guardrailResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Action != "allow" {
			t.Errorf("action = %q, want allow on decode failure", resp.Action)
		}
	})

	// With advertising disabled, no Accept-Encoding header, but inbound bodies
	// are still decoded (robustness).
	t.Run("disabled flag still decodes", func(t *testing.T) {
		h := modifyH()
		h.acceptCompressed = false
		rec := post(h, "zstd", zstdBytes(t, []byte(plain)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Accept-Encoding"); got != "" {
			t.Errorf("Accept-Encoding = %q, want empty when advertising disabled", got)
		}
		var resp guardrailResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Action != "modify" {
			t.Errorf("action = %q, want modify (decode independent of advertise flag)", resp.Action)
		}
	})
}
