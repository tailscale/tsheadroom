// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// compressRequest is the payload sent to a worker. Messages is required;
// Model is optional (the worker lets headroom pick a default when empty);
// Config carries the runtime-tunable compress knobs.
type compressRequest struct {
	Messages []any            `json:"messages"`
	Model    string           `json:"model,omitempty"`
	Config   CompressSettings `json:"config"`

	// AffinityKey routes a conversation to a consistent worker so headroom's
	// per-process compression cache (a process-global pipeline singleton) stays
	// warm across the conversation's turns. Go-internal only — never sent to the
	// worker (json:"-").
	AffinityKey string `json:"-"`
}

// compressResult mirrors the fields worker.py projects from headroom's
// CompressResult.
type compressResult struct {
	Messages          []any    `json:"messages"`
	TokensBefore      int      `json:"tokens_before"`
	TokensAfter       int      `json:"tokens_after"`
	TokensSaved       int      `json:"tokens_saved"`
	CompressionRatio  float64  `json:"compression_ratio"`
	TransformsApplied []string `json:"transforms_applied"`

	// Diagnostics added by worker.py (not part of headroom's CompressResult).
	ElapsedMs     float64 `json:"elapsed_ms"`      // worker-side compress() wall time
	ColdFirstCall bool    `json:"cold_first_call"` // this worker's first real request
	ModelLimit    int     `json:"model_limit"`     // context-window limit compressed against
}

// requestEnvelope / responseEnvelope are the NDJSON framing on the wire.
type requestEnvelope struct {
	ID      int             `json:"id"`
	Payload compressRequest `json:"payload"`
}

type responseEnvelope struct {
	ID        int             `json:"id"`
	OK        bool            `json:"ok"`
	Result    *compressResult `json:"result"`
	Error     string          `json:"error"`
	ErrorType string          `json:"error_type"`
}

const (
	shutdownGrace  = 5 * time.Second
	initialBackoff = 200 * time.Millisecond
	maxBackoff     = 10 * time.Second
	// slowCallLog is the worker-call duration above which we log a one-line
	// notice at Info (visible without -v). It's meant to surface the one-time
	// cold ML model load and any genuinely slow compress.
	slowCallLog = 2 * time.Second
)

// workerReadyTimeout bounds how long startWorker waits for a worker's
// {"ready":true} line (import headroom + warm/preload the pipeline). It's a var
// so tests can shrink it; production never changes it.
var workerReadyTimeout = 60 * time.Second

// job is one unit of work handed to a slot goroutine. It carries no context:
// the caller's request context (cancelled when aperture's hook timeout fires or
// the client disconnects) is enforced by Compress's own select, while the worker
// runs under the pool's hard cap (see runSlot).
type job struct {
	req  compressRequest
	resp chan jobResult
}

type jobResult struct {
	result *compressResult
	err    error
}

// Pool runs a fixed number of supervised Python workers. Each "slot" goroutine
// owns exactly one worker process at a time, so a worker is never touched
// concurrently — concurrency comes from the number of slots, not from
// multiplexing a single stdio stream.
//
// Dispatch is session-affinity-aware: each slot has its own affinity channel
// (buffered depth 1), plus one shared spillover channel all slots also serve.
// Compress routes a conversation to its affinity slot — so headroom's per-process
// compression cache stays warm across that conversation's turns — queuing at most
// one job deep on that slot, and spilling to the shared channel only when the slot
// already has a job waiting. The shared channel is unbuffered, so it doubles as
// backpressure: Compress blocks (until its deadline) when every slot is busy and
// the affinity slot's buffer is full.
type Pool struct {
	affinity []chan job // per-slot channels for affinity-routed jobs
	shared   chan job   // spillover: any idle slot serves these

	newCmd      func() *exec.Cmd // builds a fresh worker process
	maxCompress time.Duration    // hard cap on a single worker call before recycle
	log         *slog.Logger

	affinityEnabled bool // when false, every job uses the shared channel

	size int          // number of slots (for the saturation gauge)
	busy atomic.Int64 // slots currently running a compression (for the gauge)

	affinityHits   atomic.Int64 // jobs routed straight to their affinity slot
	affinitySpills atomic.Int64 // affinity-eligible jobs whose slot was busy -> shared

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// stats reports the pool's total and currently-busy slot counts for /metrics.
func (p *Pool) stats() (total, busy int) {
	return p.size, int(p.busy.Load())
}

// affinityStats reports affinity hit/spill counts for /metrics. hits+spills is
// the number of affinity-eligible jobs; hit rate = hits/(hits+spills).
func (p *Pool) affinityStats() (hits, spills int64) {
	return p.affinityHits.Load(), p.affinitySpills.Load()
}

// slotFor maps an affinity key to a slot index (stable for a given key/size).
func slotFor(key string, size int) int {
	if size <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(size))
}

// NewPool starts `size` slot goroutines, each spawning and supervising a worker
// launched as `python -u script`. maxCompress is the hard cap on a single
// worker call (see runSlot) — distinct from the caller's fail-open deadline.
// preload is consulted at each spawn: when it returns true the worker is told
// (via TSHEADROOM_PRELOAD=1) to load the ML model at startup, so the decision of
// what counts as "text compression enabled" lives in one place (Go), and a
// worker respawned after a runtime config change picks up the current answer.
func NewPool(size int, python, script string, preload func() bool, maxCompress time.Duration, log *slog.Logger) *Pool {
	return newPool(size, func() *exec.Cmd {
		cmd := exec.Command(python, "-u", script)
		v := "0"
		if preload != nil && preload() {
			v = "1"
		}
		cmd.Env = append(os.Environ(), "TSHEADROOM_PRELOAD="+v)
		return cmd
	}, maxCompress, log)
}

// newPool is the testable core: it takes a command factory so tests can
// substitute a fake worker (e.g. the os/exec TestHelperProcess pattern) without
// requiring a Python interpreter.
func newPool(size int, newCmd func() *exec.Cmd, maxCompress time.Duration, log *slog.Logger) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		affinity:        make([]chan job, size),
		shared:          make(chan job),
		newCmd:          newCmd,
		maxCompress:     maxCompress,
		log:             log,
		affinityEnabled: true,
		size:            size,
		ctx:             ctx,
		cancel:          cancel,
	}
	for i := 0; i < size; i++ {
		// Buffered (depth 1): a conversation can queue at most one job ahead on
		// its warm slot, so back-to-back turns reliably reuse the same worker
		// instead of racing the slot's re-park. Deeper contention spills.
		p.affinity[i] = make(chan job, 1)
		p.wg.Add(1)
		go p.runSlot(i)
	}
	return p
}

// Compress submits a job and waits for the result, bounded by ctx (the request
// context — cancelled when aperture's hook timeout fires or the client
// disconnects; tsheadroom sets no deadline of its own). If ctx fires — while
// waiting for a free slot or while the worker is still running — the caller gets
// an error and fails open, but the worker keeps running under the pool's hard cap
// (see runSlot). That lets a slow first call (e.g. a one-time model load) finish
// and leave the worker warm, so the next call succeeds — no restart needed.
func (p *Pool) Compress(ctx context.Context, req compressRequest) (*compressResult, error) {
	j := job{req: req, resp: make(chan jobResult, 1)}
	if err := p.dispatch(ctx, j); err != nil {
		return nil, err
	}
	select {
	case r := <-j.resp:
		return r.result, r.err
	case <-ctx.Done():
		// The request context was cancelled (aperture's hook timeout fired, or
		// the client went away) while the worker is still running. We return now
		// (the handler fails open and the request passes uncompressed), but the
		// worker keeps going under the hard cap and stays warm for the next call.
		// Log it so this "passthrough while warming" case — most likely on a cold
		// worker's first large request — is visible.
		p.log.Warn("request canceled before worker finished; failing open (worker continues, warming)", "err", ctx.Err())
		return nil, ctx.Err()
	}
}

// dispatch places j on a worker, bounded by ctx and pool shutdown. With affinity
// enabled and a key present, it tries a non-blocking send to the conversation's
// slot: this succeeds when the slot is idle (handed straight to the receiver) or
// when its depth-1 buffer is empty (queued one deep), so repeated turns of a
// conversation land on the same worker and reuse headroom's warm per-process
// cache, even back-to-back. If that slot is busy and its buffer is full, j spills
// to the shared channel, which any idle slot serves; the spill is counted as a
// likely (not certain) cache miss. A shutting-down pool fails fast on every path,
// so a caller never blocks on a job that will never be served.
func (p *Pool) dispatch(ctx context.Context, j job) error {
	// Fail fast if the pool is already shutting down — otherwise an affinity
	// send could buffer a job into a slot that has already exited, leaving the
	// caller waiting on j.resp forever. Shutdown also drains any job that slips
	// through the gap between this check and the send below.
	select {
	case <-p.ctx.Done():
		return errors.New("pool shutting down")
	default:
	}

	affined := p.affinityEnabled && j.req.AffinityKey != ""
	if affined {
		select {
		case p.affinity[slotFor(j.req.AffinityKey, p.size)] <- j:
			p.affinityHits.Add(1)
			return nil
		default:
			// preferred slot busy; fall through to spillover
		}
	}
	select {
	case p.shared <- j:
		if affined {
			p.affinitySpills.Add(1)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return errors.New("pool shutting down")
	}
}

// Shutdown stops all slots and their workers. A slot mid-call finishes it first
// (bounded by the hard cap), then tears down its worker. Any job left buffered in
// an affinity channel (sent just as its slot exited) is answered with an error so
// its Compress caller doesn't block on j.resp.
func (p *Pool) Shutdown() {
	p.cancel()
	p.wg.Wait()
	for _, ch := range p.affinity {
		drainBuffered(ch)
	}
}

// drainBuffered replies "pool shutting down" to every job still buffered in ch.
// Called only after all slot goroutines have exited (wg.Wait), so there is no
// competing receiver; j.resp is buffered, so the reply never blocks.
func drainBuffered(ch chan job) {
	for {
		select {
		case j := <-ch:
			j.resp <- jobResult{err: errors.New("pool shutting down")}
		default:
			return
		}
	}
}

// runSlot owns one worker for the slot's lifetime, respawning on death.
func (p *Pool) runSlot(idx int) {
	defer p.wg.Done()
	w := p.spawn(idx)
	defer func() {
		if w != nil {
			w.shutdown()
		}
	}()

	for {
		// Serve this slot's affinity-routed jobs and shared spillover equally;
		// an idle slot takes whichever is ready first.
		var j job
		select {
		case <-p.ctx.Done():
			return
		case j = <-p.affinity[idx]:
		case j = <-p.shared:
		}

		if w == nil {
			if w = p.spawn(idx); w == nil {
				j.resp <- jobResult{err: errors.New("worker unavailable")}
				return // spawn only returns nil on pool shutdown
			}
		}
		// Run under the pool's hard cap, NOT the request context. Compress
		// already fails open on that ctx; here we let the worker keep going
		// so a slow call (e.g. a model load) finishes
		// and leaves the worker warm. Only a real error or the hard cap
		// (genuinely wedged) recycles the worker. j.resp is buffered, so the
		// send never blocks even after the client has given up.
		start := time.Now()
		hardCtx, cancel := context.WithTimeout(p.ctx, p.maxCompress)
		p.busy.Add(1)
		res, err := w.do(hardCtx, j.req)
		p.busy.Add(-1)
		cancel()
		dur := time.Since(start)
		if err != nil {
			p.log.Warn("worker request failed; recycling", "slot", idx, "dur", dur, "err", err)
			w.kill()
			w = p.spawn(idx)
		} else if dur >= slowCallLog {
			// Surface slow calls without requiring -v; cold_first_call
			// pinpoints the one-time ML model load.
			p.log.Info("slow worker call", "slot", idx, "dur", dur,
				"cold_first_call", res.ColdFirstCall, "worker_ms", res.ElapsedMs)
		}
		j.resp <- jobResult{result: res, err: err}
	}
}

// spawn keeps trying to start a worker until one is ready or the pool shuts
// down (in which case it returns nil).
func (p *Pool) spawn(idx int) *worker {
	backoff := initialBackoff
	for {
		select {
		case <-p.ctx.Done():
			return nil
		default:
		}
		w, err := startWorker(p.newCmd, idx, p.log)
		if err == nil {
			p.log.Info("worker ready", "slot", idx, "pid", w.cmd.Process.Pid)
			return w
		}
		p.log.Error("worker spawn failed; retrying", "slot", idx, "err", err, "backoff", backoff)
		select {
		case <-p.ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// worker is a single Python child process. It is only ever accessed by its
// owning slot goroutine, so no locking is needed.
type worker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	log    *slog.Logger
	slot   int
	nextID int
}

// startWorker builds a worker process from newCmd, wires stdio, streams stderr
// to the log, and blocks until the worker emits its {"ready":true} line.
func startWorker(newCmd func() *exec.Cmd, slot int, log *slog.Logger) (*worker, error) {
	cmd := newCmd()
	// Run the worker in its own process group (so we can signal the whole tree
	// on shutdown) and, where the platform supports it, have the kernel kill it
	// if we die. The details are platform-specific; see procattr_*.go.
	setProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	// Surface worker stderr (headroom warnings, tracebacks) to our log.
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			log.Warn("worker stderr", "slot", slot, "line", sc.Text())
		}
	}()

	w := &worker{
		cmd:   cmd,
		stdin: stdin,
		// ReadBytes (below) handles arbitrarily long lines; a 50 MiB modified
		// body would overflow bufio.Scanner's token limit, so we never use it
		// for the protocol stream.
		stdout: bufio.NewReaderSize(stdout, 64*1024),
		log:    log,
		slot:   slot,
	}

	if err := w.awaitReady(); err != nil {
		w.kill()
		return nil, err
	}
	return w, nil
}

type lineRes struct {
	line []byte
	err  error
}

// readLine reads one newline-delimited line from the worker, bounded by ctx.
// The blocking read runs in a goroutine so ctx cancellation returns promptly;
// that goroutine is reclaimed when the stream finally yields a line or the
// stdout pipe closes (which the slot guarantees by killing a worker whose call
// was abandoned). Uses ReadBytes (not bufio.Scanner) so lines can be arbitrarily
// large — a modified body can approach the 50 MiB cap.
func (w *worker) readLine(ctx context.Context) ([]byte, error) {
	ch := make(chan lineRes, 1)
	go func() {
		line, err := w.stdout.ReadBytes('\n')
		ch <- lineRes{line, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.line, r.err
	}
}

// awaitReady blocks until the worker announces readiness, bounded by
// workerReadyTimeout.
func (w *worker) awaitReady() error {
	ctx, cancel := context.WithTimeout(context.Background(), workerReadyTimeout)
	defer cancel()

	line, err := w.readLine(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("timed out waiting for worker ready")
		}
		return fmt.Errorf("reading ready line: %w", err)
	}
	var msg struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || !msg.Ready {
		return fmt.Errorf("unexpected first line from worker: %s", truncate(line))
	}
	return nil
}

// do sends one request and reads its response, bounded by ctx. On ctx expiry
// the caller (slot) recycles the worker, which closes stdout and unblocks the
// read goroutine — so no goroutine leaks.
func (w *worker) do(ctx context.Context, req compressRequest) (*compressResult, error) {
	w.nextID++
	id := w.nextID

	buf, err := json.Marshal(requestEnvelope{ID: id, Payload: req})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	buf = append(buf, '\n')
	if _, err := w.stdin.Write(buf); err != nil {
		return nil, fmt.Errorf("write to worker: %w", err)
	}

	line, err := w.readLine(ctx)
	if err != nil {
		return nil, fmt.Errorf("read from worker: %w", err)
	}
	var resp responseEnvelope
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// Defend the one-in-flight invariant: a mismatched id means the stream
	// desynced (e.g. a stray line on stdout), so recycle rather than return a
	// response that belongs to a different request.
	if resp.ID != id {
		return nil, fmt.Errorf("response id mismatch: got %d, want %d", resp.ID, id)
	}
	if !resp.OK {
		return nil, fmt.Errorf("worker error (%s): %s", resp.ErrorType, resp.Error)
	}
	if resp.Result == nil {
		return nil, fmt.Errorf("worker reported ok but returned no result")
	}
	return resp.Result, nil
}

// shutdown asks the worker to exit cleanly (EOF on stdin), then SIGKILLs the
// process group if it overstays the grace period. cmd.Wait runs in exactly one
// place (the reaper goroutine) — signalKill never calls Wait, so there is no
// concurrent/double Wait on the same Cmd.
func (w *worker) shutdown() {
	_ = w.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(shutdownGrace):
	}
	w.signalKill() // force the group down; the reaper goroutine above reaps it
	<-done
}

// signalKill SIGKILLs the worker's whole process group without reaping it.
func (w *worker) signalKill() {
	if w.cmd.Process != nil {
		// Negative pid → signal the process group created by Setpgid.
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// kill SIGKILLs the worker and reaps it. Use only on the recycle path, where no
// other Wait is outstanding for this worker (shutdown() reaps via its own
// goroutine instead).
func (w *worker) kill() {
	w.signalKill()
	_ = w.cmd.Wait()
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
