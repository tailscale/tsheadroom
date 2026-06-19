// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// metricLine returns the value rendered for an exact metric line (name plus
// optional label set), or "" if the line is absent. It matches on the text up
// to the value so callers can assert exact emitted numbers.
func metricLine(t *testing.T, export, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(export, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, prefix+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics // never constructed — mirrors a Handler built without metrics
	// None of these may panic.
	m.inboundStart()
	m.record(summary{reason: "modify"}, 5)
	m.inboundDone()
	if got := m.export(); got != "" {
		t.Fatalf("nil export = %q, want empty", got)
	}
}

func TestMetrics_RecordAndExport(t *testing.T) {
	m := newMetrics()
	m.poolStats = func() (int, int) { return 8, 3 }

	// A successful modify, a no-op, and a failed (fail-open) request.
	m.record(summary{
		reason: "modify", provider: "anthropic", model: "claude-x",
		tokensBefore: 100, tokensSaved: 40, tokensAfter: 60, cold: true,
	}, 12.5)
	m.record(summary{
		reason: "allow(noop)", provider: "anthropic", model: "claude-x",
		tokensBefore: 10, tokensSaved: 0, tokensAfter: 10,
	}, 2)
	m.record(summary{reason: "allow(error)", provider: "openai", model: "gpt-x"}, 1)

	out := m.export()

	checks := map[string]string{
		"headroom_requests_total":                             "3",
		"headroom_requests_failed_total":                      "1",
		"headroom_tokens_input_total":                         "110",
		"headroom_tokens_saved_total":                         "40",
		"headroom_overhead_ms_count":                          "3",
		"headroom_inbound_requests_total":                     "0", // no inbound* recorded here
		"tsheadroom_tokens_after_total":                       "70",
		"tsheadroom_cold_starts_total":                        "1",
		"tsheadroom_pool_slots_total":                         "8",
		"tsheadroom_pool_slots_busy":                          "3",
		`headroom_requests_by_provider{provider="anthropic"}`: "2",
		`headroom_requests_by_provider{provider="openai"}`:    "1",
		`headroom_requests_by_model{model="claude-x"}`:        "2",
		`tsheadroom_requests_by_outcome{outcome="modify"}`:    "1",
		`tsheadroom_requests_by_outcome{outcome="noop"}`:      "1",
		`tsheadroom_requests_by_outcome{outcome="error"}`:     "1",
	}
	for prefix, want := range checks {
		if got := metricLine(t, out, prefix); got != want {
			t.Errorf("%s = %q, want %q\n--- export ---\n%s", prefix, got, want, out)
		}
	}

	// Every emitted family must carry HELP/TYPE headers.
	for _, name := range []string{"headroom_requests_total", "headroom_requests_by_provider", "tsheadroom_pool_slots_busy"} {
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing # TYPE header for %s", name)
		}
	}
}

func TestMetrics_UnknownLabelFallback(t *testing.T) {
	m := newMetrics()
	m.record(summary{reason: "allow(passthrough)"}, 1) // no provider/model
	out := m.export()
	if got := metricLine(t, out, `headroom_requests_by_provider{provider="unknown"}`); got != "1" {
		t.Errorf("empty provider should fall back to unknown; export:\n%s", out)
	}
	if got := metricLine(t, out, `headroom_requests_by_model{model="unknown"}`); got != "1" {
		t.Errorf("empty model should fall back to unknown; export:\n%s", out)
	}
}

func TestMetrics_InboundGauge(t *testing.T) {
	m := newMetrics()
	m.inboundStart()
	m.inboundStart()
	m.inboundDone() // one still in flight
	out := m.export()
	if got := metricLine(t, out, "headroom_inbound_requests_total"); got != "2" {
		t.Errorf("inbound_total = %q, want 2", got)
	}
	if got := metricLine(t, out, "headroom_inbound_requests_completed_total"); got != "1" {
		t.Errorf("inbound_completed = %q, want 1", got)
	}
	if got := metricLine(t, out, "headroom_inbound_requests_active"); got != "1" {
		t.Errorf("inbound_active = %q, want 1", got)
	}
}

func TestMetrics_PoolGaugesOmittedWhenUnset(t *testing.T) {
	m := newMetrics() // poolStats left nil
	if strings.Contains(m.export(), "tsheadroom_pool_slots_total") {
		t.Error("pool gauges must be omitted when poolStats is nil")
	}
}

func TestMetrics_HTTPEndpoint(t *testing.T) {
	m := newMetrics()
	m.record(summary{reason: "modify", provider: "anthropic", model: "claude-x", tokensSaved: 5}, 1)

	// GET serves the exposition.
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain...", ct)
	}
	if !strings.Contains(rec.Body.String(), "headroom_tokens_saved_total 5") {
		t.Errorf("body missing recorded metric:\n%s", rec.Body.String())
	}

	// Non-GET is rejected.
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", rec.Code)
	}
}

// TestMetrics_HandlerIntegration drives a request through the Handler and
// confirms the provider/model from aperture metadata land in /metrics.
func TestMetrics_HandlerIntegration(t *testing.T) {
	h := newTestHandler(func(_ context.Context, req compressRequest) (*compressResult, error) {
		if req.Model != "claude-from-metadata" {
			t.Errorf("compress model = %q, want metadata model", req.Model)
		}
		return &compressResult{Messages: json.RawMessage(`["x"]`), TokensBefore: 50, TokensSaved: 20, TokensAfter: 30}, nil
	})
	h.metrics = newMetrics()

	body := `{"metadata":{"provider":"anthropic","model":"claude-from-metadata"},
		"request_body":{"messages":[{"role":"user","content":"big"}]}}`
	resp := doHook(t, h, body)
	if resp.Action != "modify" {
		t.Fatalf("action = %q, want modify", resp.Action)
	}

	out := h.metrics.export()
	if got := metricLine(t, out, `headroom_requests_by_provider{provider="anthropic"}`); got != "1" {
		t.Errorf("provider from metadata not recorded; export:\n%s", out)
	}
	if got := metricLine(t, out, `headroom_requests_by_model{model="claude-from-metadata"}`); got != "1" {
		t.Errorf("model from metadata not recorded; export:\n%s", out)
	}
	if got := metricLine(t, out, "headroom_tokens_saved_total"); got != "20" {
		t.Errorf("tokens_saved = %q, want 20", got)
	}
	if got := metricLine(t, out, "headroom_inbound_requests_active"); got != "0" {
		t.Errorf("inbound_active = %q, want 0 after request completes", got)
	}
}
