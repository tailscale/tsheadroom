package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
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
)

// workerReadyTimeout bounds how long startWorker waits for a worker's
// {"ready":true} line (import headroom + warm/preload the pipeline). It's a var
// so tests can shrink it; production never changes it.
var workerReadyTimeout = 60 * time.Second

// job is one unit of work handed to a slot goroutine. It carries no context:
// the client's fail-open deadline is enforced by Compress's own select, while
// the worker runs under the pool's hard cap (see runSlot).
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
// multiplexing a single stdio stream. Jobs are dispatched over an unbuffered
// channel, which doubles as backpressure: Compress blocks (until its deadline)
// when every slot is busy.
type Pool struct {
	jobs        chan job
	newCmd      func() *exec.Cmd // builds a fresh worker process
	maxCompress time.Duration    // hard cap on a single worker call before recycle
	log         *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
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
		jobs:        make(chan job),
		newCmd:      newCmd,
		maxCompress: maxCompress,
		log:         log,
		ctx:         ctx,
		cancel:      cancel,
	}
	for i := 0; i < size; i++ {
		p.wg.Add(1)
		go p.runSlot(i)
	}
	return p
}

// Compress submits a job and waits for the result, bounded by ctx (the client's
// fail-open deadline). If ctx fires — while waiting for a free slot or while the
// worker is still running — the caller gets an error and fails open, but the
// worker keeps running under the pool's hard cap (see runSlot). That lets a slow
// first call (e.g. a one-time model load) finish and leave the worker warm, so
// the next call succeeds — no restart needed.
func (p *Pool) Compress(ctx context.Context, req compressRequest) (*compressResult, error) {
	j := job{req: req, resp: make(chan jobResult, 1)}
	select {
	case p.jobs <- j:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, errors.New("pool shutting down")
	}
	select {
	case r := <-j.resp:
		return r.result, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Shutdown stops all slots and their workers. A slot mid-call finishes it first
// (bounded by the hard cap), then tears down its worker.
func (p *Pool) Shutdown() {
	p.cancel()
	p.wg.Wait()
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
		select {
		case <-p.ctx.Done():
			return
		case j := <-p.jobs:
			if w == nil {
				if w = p.spawn(idx); w == nil {
					j.resp <- jobResult{err: errors.New("worker unavailable")}
					return // spawn only returns nil on pool shutdown
				}
			}
			// Run under the pool's hard cap, NOT the client's deadline. The
			// client already fails open on its own ctx in Compress; here we let
			// the worker keep going so a slow call (e.g. a model load) finishes
			// and leaves the worker warm. Only a real error or the hard cap
			// (genuinely wedged) recycles the worker. j.resp is buffered, so the
			// send never blocks even after the client has given up.
			hardCtx, cancel := context.WithTimeout(p.ctx, p.maxCompress)
			res, err := w.do(hardCtx, j.req)
			cancel()
			if err != nil {
				p.log.Warn("worker request failed; recycling", "slot", idx, "err", err)
				w.kill()
				w = p.spawn(idx)
			}
			j.resp <- jobResult{result: res, err: err}
		}
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
	// Pdeathsig: the kernel SIGKILLs the worker if we (the parent) die, so no
	// orphans survive a supervisor crash. Setpgid: own process group, so we can
	// signal the whole tree on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}

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
