package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// compressRequest is the payload sent to a worker. Messages is required;
// Model is optional (the worker lets headroom pick a default when empty).
type compressRequest struct {
	Messages []any  `json:"messages"`
	Model    string `json:"model,omitempty"`
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
	workerReadyTimeout = 60 * time.Second // import headroom + warm the pipeline
	shutdownGrace      = 5 * time.Second
	initialBackoff     = 200 * time.Millisecond
	maxBackoff         = 10 * time.Second
)

// job is one unit of work handed to a slot goroutine.
type job struct {
	ctx  context.Context
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
	jobs   chan job
	newCmd func() *exec.Cmd // builds a fresh worker process
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewPool starts `size` slot goroutines, each spawning and supervising a worker
// launched as `python -u script`.
func NewPool(size int, python, script string, log *slog.Logger) *Pool {
	return newPool(size, func() *exec.Cmd {
		return exec.Command(python, "-u", script)
	}, log)
}

// newPool is the testable core: it takes a command factory so tests can
// substitute a fake worker (e.g. the os/exec TestHelperProcess pattern) without
// requiring a Python interpreter.
func newPool(size int, newCmd func() *exec.Cmd, log *slog.Logger) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		jobs:   make(chan job),
		newCmd: newCmd,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
	}
	for i := 0; i < size; i++ {
		p.wg.Add(1)
		go p.runSlot(i)
	}
	return p
}

// Compress submits a job and waits for the result, bounded by ctx. A ctx
// deadline can fire either while waiting for a free slot or while a worker
// processes the request; both surface as an error so the caller fails open.
func (p *Pool) Compress(ctx context.Context, req compressRequest) (*compressResult, error) {
	j := job{ctx: ctx, req: req, resp: make(chan jobResult, 1)}
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

// Shutdown stops all slots and their workers. Slots blocked on a job finish it
// first (bounded by the job's deadline), then tear down their worker.
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
			res, err := w.do(j.ctx, j.req)
			if err != nil {
				// The worker is suspect (crash, timeout, or protocol error):
				// kill it and bring up a fresh one for the next job.
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

// awaitReady reads lines until the worker announces readiness, bounded by
// workerReadyTimeout.
func (w *worker) awaitReady() error {
	type lineRes struct {
		line []byte
		err  error
	}
	ch := make(chan lineRes, 1)
	go func() {
		line, err := w.stdout.ReadBytes('\n')
		ch <- lineRes{line, err}
	}()

	timer := time.NewTimer(workerReadyTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		return errors.New("timed out waiting for worker ready")
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("reading ready line: %w", r.err)
		}
		var msg struct {
			Ready bool `json:"ready"`
		}
		if err := json.Unmarshal(r.line, &msg); err != nil || !msg.Ready {
			return fmt.Errorf("unexpected first line from worker: %s", truncate(r.line))
		}
		return nil
	}
}

// do sends one request and reads its response, bounded by ctx. On ctx
// expiry the caller (slot) recycles the worker, which closes stdout and
// unblocks the read goroutine — so no goroutine leaks.
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

	type lineRes struct {
		line []byte
		err  error
	}
	ch := make(chan lineRes, 1)
	go func() {
		line, err := w.stdout.ReadBytes('\n')
		ch <- lineRes{line, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("read from worker: %w", r.err)
		}
		var resp responseEnvelope
		if err := json.Unmarshal(r.line, &resp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if !resp.OK {
			return nil, fmt.Errorf("worker error (%s): %s", resp.ErrorType, resp.Error)
		}
		return resp.Result, nil
	}
}

// shutdown asks the worker to exit cleanly (EOF on stdin), then kills the
// process group if it overstays the grace period.
func (w *worker) shutdown() {
	_ = w.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		w.kill()
	}
}

// kill SIGKILLs the worker's whole process group and reaps it.
func (w *worker) kill() {
	if w.cmd.Process != nil {
		// Negative pid → signal the process group created by Setpgid.
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = w.cmd.Wait()
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
