// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metrics accumulates lifetime counters for the /metrics endpoint. It is the
// Go-side synthesis of the per-request numbers we already receive (token
// savings, timing, outcome) plus live pool state — tsheadroom calls headroom's
// bare compress() in isolated workers, so headroom's own aggregate metrics
// (proxy/client Prometheus, OTel) are never populated by us and can't be
// surfaced. See the README "/metrics" section for which headroom_* names we
// emit faithfully and which proxy-only families we deliberately omit.
//
// All methods are safe on a nil *Metrics so tests (and any future caller) can
// construct a Handler without one. Every mutating method takes the mutex; the
// scrape (export) snapshots under the same lock.
type Metrics struct {
	mu sync.Mutex

	// Processed hook calls (POSTs we ran through compression).
	requestsTotal  int64
	requestsFailed int64 // compress returned an error (we failed open)
	tokensInput    int64 // Σ tokens_before
	tokensSaved    int64 // Σ tokens_saved
	tokensAfter    int64 // Σ tokens_after (post-compression)
	coldStarts     int64 // worker first-real-call (paid the ML model load)

	// Per-request overhead: our full processing time, the latency we add to a
	// request (headroom calls this "overhead" — optimization time, excludes LLM).
	overheadSumMs float64
	overheadMinMs float64
	overheadMaxMs float64
	overheadCount int64

	byProvider map[string]int64
	byModel    map[string]int64
	byOutcome  map[string]int64

	// Inbound HTTP requests to the hook handler (method-agnostic): total seen,
	// completed, and currently in flight. active doubles as a saturation signal.
	inboundTotal     int64
	inboundCompleted int64
	inboundActive    int64

	// poolStats, when set, returns (total slots, busy slots) for the live pool
	// gauges. nil in tests or before wiring; the gauges are then omitted.
	poolStats func() (total, busy int)
}

func newMetrics() *Metrics {
	return &Metrics{
		byProvider:    map[string]int64{},
		byModel:       map[string]int64{},
		byOutcome:     map[string]int64{},
		overheadMinMs: math.Inf(1),
	}
}

// inboundStart records a newly-accepted inbound request (any method).
func (m *Metrics) inboundStart() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inboundTotal++
	m.inboundActive++
	m.mu.Unlock()
}

// inboundDone records that an inbound request finished (any method/outcome).
func (m *Metrics) inboundDone() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inboundCompleted++
	if m.inboundActive > 0 {
		m.inboundActive--
	}
	m.mu.Unlock()
}

// record folds one processed hook call into the lifetime counters. durMs is the
// handler's full processing time for the request.
func (m *Metrics) record(s summary, durMs float64) {
	if m == nil {
		return
	}
	outcome := outcomeFromReason(s.reason)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.requestsTotal++
	m.byOutcome[outcome]++
	m.byProvider[labelOrUnknown(s.provider)]++
	m.byModel[labelOrUnknown(s.model)]++
	m.tokensInput += int64(s.tokensBefore)
	m.tokensSaved += int64(s.tokensSaved)
	m.tokensAfter += int64(s.tokensAfter)
	if s.cold {
		m.coldStarts++
	}
	if outcome == "error" {
		m.requestsFailed++
	}

	m.overheadSumMs += durMs
	m.overheadCount++
	if durMs < m.overheadMinMs {
		m.overheadMinMs = durMs
	}
	if durMs > m.overheadMaxMs {
		m.overheadMaxMs = durMs
	}
}

// outcomeFromReason maps the handler's verbose reason to a stable, low-
// cardinality outcome label for tsheadroom_requests_by_outcome.
func outcomeFromReason(reason string) string {
	switch reason {
	case "modify":
		return "modify"
	case "allow(noop)":
		return "noop"
	case "allow(error)":
		return "error"
	case "allow(read-error)":
		return "read_error"
	default: // allow(passthrough) and any future allow(...) shapes
		return "passthrough"
	}
}

func labelOrUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// ServeHTTP exposes the metrics in Prometheus text exposition format.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(m.export()))
}

// export renders the current counters as Prometheus text. It mirrors the metric
// names headroom's proxy emits (so existing scrapers/dashboards can be
// repointed) for the subset we can populate faithfully, then adds tsheadroom_*
// native metrics that have no headroom analog (live pool saturation, per-
// outcome counts, cold starts, post-compression tokens).
func (m *Metrics) export() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	requestsTotal := m.requestsTotal
	requestsFailed := m.requestsFailed
	tokensInput := m.tokensInput
	tokensSaved := m.tokensSaved
	tokensAfter := m.tokensAfter
	coldStarts := m.coldStarts
	overheadSum := m.overheadSumMs
	overheadCount := m.overheadCount
	overheadMin := m.overheadMinMs
	overheadMax := m.overheadMaxMs
	inboundTotal := m.inboundTotal
	inboundCompleted := m.inboundCompleted
	inboundActive := m.inboundActive
	byProvider := snapshotMap(m.byProvider)
	byModel := snapshotMap(m.byModel)
	byOutcome := snapshotMap(m.byOutcome)
	m.mu.Unlock()

	var b strings.Builder

	// --- headroom_* : faithful subset (repoint-compatible) ---
	scalar(&b, "headroom_requests_total", "counter", "Total compression hook calls processed", requestsTotal)
	scalar(&b, "headroom_requests_failed_total", "counter", "Hook calls where compression errored and we failed open", requestsFailed)
	scalar(&b, "headroom_tokens_input_total", "counter", "Total input tokens seen by compression (tokens_before)", tokensInput)
	scalar(&b, "headroom_tokens_saved_total", "counter", "Tokens saved by compression (tokens_saved)", tokensSaved)

	overheadMinOut := 0.0
	if overheadCount > 0 {
		overheadMinOut = round2(overheadMin)
	}
	scalar(&b, "headroom_overhead_ms_sum", "counter", "Sum of tsheadroom processing time in milliseconds (excludes upstream LLM)", round2(overheadSum))
	scalar(&b, "headroom_overhead_ms_count", "counter", "Count of observed tsheadroom overhead samples", overheadCount)
	scalar(&b, "headroom_overhead_ms_min", "gauge", "Minimum observed tsheadroom overhead in milliseconds", overheadMinOut)
	scalar(&b, "headroom_overhead_ms_max", "gauge", "Maximum observed tsheadroom overhead in milliseconds", round2(overheadMax))

	scalar(&b, "headroom_inbound_requests_total", "counter", "All inbound HTTP requests accepted by the hook handler", inboundTotal)
	scalar(&b, "headroom_inbound_requests_completed_total", "counter", "Inbound HTTP requests completed by the hook handler", inboundCompleted)
	scalar(&b, "headroom_inbound_requests_active", "gauge", "Inbound HTTP requests currently in flight in the hook handler", inboundActive)

	labeled(&b, "headroom_requests_by_provider", "counter", "Requests by provider", "provider", byProvider)
	labeled(&b, "headroom_requests_by_model", "counter", "Requests by model", "model", byModel)

	// --- tsheadroom_* : native metrics (no headroom analog) ---
	scalar(&b, "tsheadroom_tokens_after_total", "counter", "Total output tokens after compression (tokens_after)", tokensAfter)
	scalar(&b, "tsheadroom_cold_starts_total", "counter", "Worker first-real-call events that paid the one-time ML model load", coldStarts)
	labeled(&b, "tsheadroom_requests_by_outcome", "counter", "Requests by outcome", "outcome", byOutcome)

	if m.poolStats != nil {
		total, busy := m.poolStats()
		scalar(&b, "tsheadroom_pool_slots_total", "gauge", "Total worker slots in the pool", int64(total))
		scalar(&b, "tsheadroom_pool_slots_busy", "gauge", "Worker slots currently running a compression", int64(busy))
	}

	return b.String()
}

func snapshotMap(src map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// scalar appends one unlabeled metric (HELP/TYPE/value) in Prometheus format.
// value may be an integer or float; it is rendered without scientific notation.
func scalar(b *strings.Builder, name, typ, help string, value any) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %s\n\n", name, help, name, typ, name, fmtValue(value))
}

// labeled appends a metric family with one label dimension, sorted by label
// value for stable output. The family header is always emitted (even when
// empty) so dashboards can discover it on a fresh boot.
func labeled(b *strings.Builder, name, typ, help, label string, values map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %d\n", name, label, escapeLabel(k), values[k])
	}
	b.WriteString("\n")
}

func fmtValue(v any) string {
	switch n := v.(type) {
	case int64:
		return fmt.Sprintf("%d", n)
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", n), "0"), ".")
	default:
		return fmt.Sprintf("%v", n)
	}
}

func round2(f float64) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return math.Round(f*100) / 100
}

// escapeLabel escapes a Prometheus label value (backslash, newline, quote),
// matching headroom's exporter so identical label values render identically.
func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\n", "\\n")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	return v
}
