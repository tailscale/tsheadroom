package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

	emit(map[string]any{"ready": true})

	r := bufio.NewReader(os.Stdin)
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
		switch req.Payload.Model {
		case "CRASH":
			os.Exit(1) // die without responding
		case "HANG":
			time.Sleep(30 * time.Second) // longer than any test deadline
		case "ERROR":
			emit(responseEnvelope{ID: req.ID, OK: false, Error: "boom", ErrorType: "RuntimeError"})
			continue
		}
		emit(responseEnvelope{ID: req.ID, OK: true, Result: &compressResult{
			Messages:          req.Payload.Messages,
			TokensBefore:      100,
			TokensAfter:       40,
			TokensSaved:       60,
			CompressionRatio:  0.4,
			TransformsApplied: []string{"fake"},
		}})
	}
}

// helperCmd builds a worker command that re-executes this test binary as the
// fake worker.
func helperCmd() func() *exec.Cmd {
	return func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
}

func testPool(t *testing.T, size int) *Pool {
	t.Helper()
	p := newPool(size, helperCmd(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestPool_DeadlineFailsOpen(t *testing.T) {
	p := testPool(t, 1)
	_, err := mustCompress(t, p, compressRequest{Model: "HANG"}, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// The hung worker must be recycled so the pool keeps serving.
	if _, err := mustCompress(t, p, compressRequest{Messages: []any{"x"}}, 5*time.Second); err != nil {
		t.Fatalf("pool did not recover after deadline: %v", err)
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

func TestPool_ShutdownIsPrompt(t *testing.T) {
	p := newPool(2, helperCmd(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
