// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: when invoked with
// GO_WANT_HELPER_PROCESS=1 it impersonates a Python worker, speaking the same
// NDJSON protocol as worker.py. Behavior is driven by the request's model field
// so a single fake covers the happy path, protocol errors, crashes, and hangs.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	w := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
	}

	// Simulate a worker that never finishes starting up (e.g. a stuck model
	// load): never emit ready, just block.
	if os.Getenv("HELPER_NO_READY") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	emit(map[string]any{"ready": true})

	r := bufio.NewReader(os.Stdin)
	calls := 0
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			os.Exit(0) // EOF on stdin: clean shutdown
		}
		var req requestEnvelope
		if err := json.Unmarshal(line, &req); err != nil {
			emit(responseEnvelope{OK: false, Error: "bad json", ErrorType: "ValueError"})
			continue
		}
		calls++
		switch req.Payload.Model {
		case "CRASH":
			os.Exit(1) // die without responding
		case "HANG":
			time.Sleep(30 * time.Second) // longer than any test deadline
		case "SLEEP":
			time.Sleep(400 * time.Millisecond) // pin a slot busy briefly
		case "SLOW_ONCE":
			if calls == 1 {
				// Slow first call (like a one-time model load), then fast.
				time.Sleep(800 * time.Millisecond)
			}
		case "ERROR":
			emit(responseEnvelope{ID: req.ID, OK: false, Error: "boom", ErrorType: "RuntimeError"})
			continue
		case "NIL_RESULT":
			emit(responseEnvelope{ID: req.ID, OK: true, Result: nil}) // ok but no result
			continue
		case "WRONG_ID":
			emit(responseEnvelope{ID: req.ID + 1, OK: true, Result: &compressResult{TokensSaved: 1}})
			continue
		}
		emit(responseEnvelope{ID: req.ID, OK: true, Result: &compressResult{
			Messages:          req.Payload.Messages,
			TokensBefore:      100,
			TokensAfter:       40,
			TokensSaved:       60,
			CompressionRatio:  0.4,
			TransformsApplied: []string{"fake", "pid:" + strconv.Itoa(os.Getpid())},
		}})
	}
}

// helperCmd builds a worker command that re-executes this test binary as the
// fake worker. extraEnv is appended to the child's environment.
func helperCmd(extraEnv ...string) func() *exec.Cmd {
	return func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Env = append(cmd.Env, extraEnv...)
		return cmd
	}
}

func testPool(t *testing.T, size int) *Pool {
	t.Helper()
	return testPoolCap(t, size, 30*time.Second)
}

func testPoolCap(t *testing.T, size int, maxCompress time.Duration) *Pool {
	t.Helper()
	p := newPool(size, helperCmd(), maxCompress, quietLog())
	t.Cleanup(p.Shutdown)
	return p
}

func mustCompress(t *testing.T, p *Pool, req compressRequest, timeout time.Duration) (*compressResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return p.Compress(ctx, req)
}

func TestPool_RoundTrip(t *testing.T) {
	p := testPool(t, 1)
	res, err := mustCompress(t, p, compressRequest{Messages: []any{"a", "b"}, Model: "gpt-4o"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if res.TokensSaved != 60 {
		t.Errorf("tokens_saved = %d, want 60", res.TokensSaved)
	}
	if len(res.Messages) != 2 {
		t.Errorf("messages len = %d, want 2 (echoed)", len(res.Messages))
	}
}

// A worker that reports ok:true but no result must not yield a nil result to
// the caller (which would nil-deref the handler and break the always-200
// guarantee); do() turns it into an error.
func TestPool_NilResultIsError(t *testing.T) {
	p := testPool(t, 1)
	res, err := mustCompress(t, p, compressRequest{Model: "NIL_RESULT"}, 5*time.Second)
	if err == nil || res != nil {
		t.Fatalf("expected error and nil result, got res=%v err=%v", res, err)
	}
	// Pool recovers for the next request.
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("pool did not recover after nil-result: %v", err)
	}
}

// A response whose id doesn't match the request means the stream desynced; do()
// must reject it rather than return another request's result.
func TestPool_IdMismatchIsError(t *testing.T) {
	p := testPool(t, 1)
	if _, err := mustCompress(t, p, compressRequest{Model: "WRONG_ID"}, 5*time.Second); err == nil {
		t.Fatal("expected error on response id mismatch")
	}
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("pool did not recover after id mismatch: %v", err)
	}
}

func TestPool_WorkerErrorFailsOpen(t *testing.T) {
	p := testPool(t, 1)
	if _, err := mustCompress(t, p, compressRequest{Model: "ERROR"}, 5*time.Second); err == nil {
		t.Fatal("expected error from worker ok:false response")
	}
	// The slot should recycle and serve the next request normally.
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("pool did not recover after worker error: %v", err)
	}
}

func TestPool_CrashRecovery(t *testing.T) {
	p := testPool(t, 1)
	if _, err := mustCompress(t, p, compressRequest{Model: "CRASH"}, 5*time.Second); err == nil {
		t.Fatal("expected error when worker crashes mid-request")
	}
	res, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second)
	if err != nil {
		t.Fatalf("pool did not recover after crash: %v", err)
	}
	if res.TokensSaved != 60 {
		t.Errorf("post-recovery tokens_saved = %d, want 60", res.TokensSaved)
	}
}

// A slow first call (e.g. a one-time model load) must fail the client open at
// its deadline WITHOUT killing the worker, so the worker finishes, stays warm,
// and the next call succeeds — no recycle, no restart.
func TestPool_SlowCallFailsOpenButWorkerWarms(t *testing.T) {
	p := testPoolCap(t, 1, 10*time.Second) // generous hard cap

	// Client deadline (200ms) is well under the worker's 800ms first call.
	_, err := mustCompress(t, p, compressRequest{Model: "SLOW_ONCE"}, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call err = %v, want DeadlineExceeded (fail open)", err)
	}

	// The same worker should now be warm: a generous-deadline call succeeds.
	res, err := mustCompress(t, p, compressRequest{Model: "SLOW_ONCE", Messages: []any{"x"}}, 5*time.Second)
	if err != nil {
		t.Fatalf("worker did not warm/recover after slow first call: %v", err)
	}
	if res.TokensSaved != 60 {
		t.Errorf("warm call tokens_saved = %d, want 60", res.TokensSaved)
	}
}

// A worker that blows the hard cap is genuinely wedged and must be recycled.
func TestPool_HardCapRecyclesWedgedWorker(t *testing.T) {
	p := testPoolCap(t, 1, 300*time.Millisecond) // tiny hard cap

	// HANG sleeps far longer than the hard cap; the call should error and the
	// worker should be recycled.
	_, err := mustCompress(t, p, compressRequest{Model: "HANG"}, 5*time.Second)
	if err == nil {
		t.Fatal("expected error when worker exceeds the hard cap")
	}
	// Fresh worker serves the next call.
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("pool did not recover after hard-cap recycle: %v", err)
	}
}

func TestPool_ConcurrentLoad(t *testing.T) {
	p := testPool(t, 4)
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 10*time.Second); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Compress failed: %v", err)
	}
}

// A worker that never signals ready must fail startWorker promptly (not hang)
// once the readiness timeout elapses, and return no worker handle (startWorker
// kills+reaps the process internally before returning the error).
func TestStartWorker_ReadyTimeout(t *testing.T) {
	orig := workerReadyTimeout
	workerReadyTimeout = 300 * time.Millisecond
	defer func() { workerReadyTimeout = orig }()

	log := quietLog()
	done := make(chan struct{})
	var w *worker
	var err error
	go func() {
		w, err = startWorker(helperCmd("HELPER_NO_READY=1"), 0, log)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // >> 300ms timeout; proves it didn't hang
		t.Fatal("startWorker hung past the readiness timeout")
	}
	if err == nil {
		w.kill()
		t.Fatal("expected startWorker to fail when the worker never readies")
	}
	if w != nil {
		t.Errorf("expected nil worker on failure, got %+v", w)
	}
}

func TestPool_ShutdownIsPrompt(t *testing.T) {
	p := newPool(2, helperCmd(), 30*time.Second, quietLog())
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("warmup compress: %v", err)
	}
	done := make(chan struct{})
	go func() { p.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return promptly")
	}
}

// workerPID extracts the serving worker's pid, which the fake worker stamps into
// TransformsApplied, so a test can tell which worker (slot) served a request.
func workerPID(t *testing.T, res *compressResult) string {
	t.Helper()
	for _, s := range res.TransformsApplied {
		if strings.HasPrefix(s, "pid:") {
			return s
		}
	}
	t.Fatalf("no pid stamp in transforms: %v", res.TransformsApplied)
	return ""
}

func TestSlotFor(t *testing.T) {
	// Deterministic and in range.
	for _, n := range []int{2, 4, 8} {
		first := slotFor("conv-xyz", n)
		for i := 0; i < 100; i++ {
			if got := slotFor("conv-xyz", n); got != first {
				t.Fatalf("slotFor not deterministic: %d vs %d", got, first)
			}
		}
		if first < 0 || first >= n {
			t.Errorf("slotFor out of range: %d for size %d", first, n)
		}
	}
	// Degenerate sizes never panic and pin to slot 0.
	if got := slotFor("k", 1); got != 0 {
		t.Errorf("slotFor size 1 = %d, want 0", got)
	}
	if got := slotFor("k", 0); got != 0 {
		t.Errorf("slotFor size 0 = %d, want 0", got)
	}
}

// Sequential (uncontended) requests sharing an affinity key must all land on the
// same worker, so headroom's per-process cache stays warm. Different keys are
// free to use other workers.
func TestPool_AffinitySameKeySameWorker(t *testing.T) {
	p := testPool(t, 4)
	var pid string
	for i := 0; i < 5; i++ {
		res, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}, AffinityKey: "conv-A"}, 5*time.Second)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		got := workerPID(t, res)
		if pid == "" {
			pid = got
		} else if got != pid {
			t.Fatalf("same key landed on different workers: %s vs %s", pid, got)
		}
	}
	if hits, spills := p.affinityStats(); hits != 5 || spills != 0 {
		t.Errorf("affinity stats = (hits=%d, spills=%d), want (5, 0)", hits, spills)
	}
}

// Under concurrency, a conversation's warm slot can hold at most one running plus
// one queued job; further same-key requests must spill to other workers rather
// than pile up unboundedly on the warm one.
func TestPool_AffinitySpillUnderContention(t *testing.T) {
	p := testPool(t, 3)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// SLEEP holds each slot ~400ms so the warm slot's buffer fills and
			// the rest are forced to spill.
			_, _ = mustCompress(t, p, compressRequest{Messages: []any{"x"}, Model: "SLEEP", AffinityKey: "conv-B"}, 10*time.Second)
		}()
	}
	wg.Wait()

	hits, spills := p.affinityStats()
	if hits+spills != n {
		t.Errorf("affinity-eligible count = %d, want %d (hits=%d, spills=%d)", hits+spills, n, hits, spills)
	}
	if hits == 0 {
		t.Errorf("expected at least one warm-slot hit, got 0")
	}
	if spills == 0 {
		t.Errorf("expected spills under contention, got 0 (hits=%d)", hits)
	}
}

// With affinity disabled, an affinity key is ignored: every request uses the
// shared channel, so the hit/spill counters stay at zero.
func TestPool_AffinityDisabledUsesShared(t *testing.T) {
	p := testPool(t, 2)
	p.affinityEnabled = false
	for i := 0; i < 3; i++ {
		if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}, AffinityKey: "conv-C"}, 5*time.Second); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if hits, spills := p.affinityStats(); hits != 0 || spills != 0 {
		t.Errorf("affinity disabled stats = (hits=%d, spills=%d), want (0, 0)", hits, spills)
	}
}

// A job buffered into a slot's affinity channel (its slot busy) must be answered
// with an error on shutdown, not orphaned — otherwise its Compress caller blocks
// on j.resp forever.
func TestPool_ShutdownAnswersBufferedAffinityJob(t *testing.T) {
	p := testPoolCap(t, 1, time.Second)

	// Occupy the single slot's worker with a long call on key "K".
	go func() {
		_, _ = mustCompress(t, p, compressRequest{Messages: []any{"x"}, Model: "HANG", AffinityKey: "K"}, 30*time.Second)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for p.busy.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never became busy")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A second same-key job buffers into the now-busy slot's affinity channel.
	// Use a non-cancelable context so only the pool can unblock it: if the job
	// were orphaned, this would hang.
	bufferedErr := make(chan error, 1)
	go func() {
		_, err := p.Compress(context.Background(), compressRequest{Messages: []any{"y"}, AffinityKey: "K"})
		bufferedErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the buffered send land

	go p.Shutdown()

	select {
	case err := <-bufferedErr:
		if err == nil {
			t.Fatal("buffered job returned nil error on shutdown, want an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("buffered affinity job hung on shutdown (never answered)")
	}
}
