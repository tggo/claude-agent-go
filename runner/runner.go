package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/internal/procgroup"
	"github.com/tggo/claude-agent-go/transport"
)

// Default configuration values, applied for zero-valued Config fields in New.
const (
	defaultBinary    = "claude"
	defaultModel     = "sonnet"
	defaultMaxTurns  = 50
	defaultBudgetUSD = "5.00"
	defaultTimeout   = 15 * time.Minute

	// maxScanBuf bounds a single stream-json line (64 MiB) — assistant turns
	// can embed large tool results.
	maxScanBuf = 64 * 1024 * 1024
)

// Runner executes the Claude CLI. It is safe for concurrent use: each call
// builds its own subprocess and holds no per-invocation state.
type Runner struct {
	cfg Config
}

// New constructs a Runner. A bare New() is valid; options and an explicit
// Config override the defaults.
func New(opts ...Option) *Runner {
	cfg := Config{skipPermissions: true}
	for _, opt := range opts {
		opt(&cfg)
	}
	applyDefaults(&cfg)
	return &Runner{cfg: cfg}
}

// NewWithConfig constructs a Runner from an explicit Config, then applies any
// options on top, then fills defaults. skipPermissions is unexported and starts
// true; pass WithSkipPermissions(false) to disable it.
func NewWithConfig(cfg Config, opts ...Option) *Runner {
	cfg.skipPermissions = true
	for _, opt := range opts {
		opt(&cfg)
	}
	applyDefaults(&cfg)
	return &Runner{cfg: cfg}
}

func applyDefaults(cfg *Config) {
	if cfg.Binary == "" {
		cfg.Binary = defaultBinary
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = defaultModel
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = defaultMaxTurns
	}
	if cfg.MaxBudgetUSD == "" {
		cfg.MaxBudgetUSD = defaultBudgetUSD
	}
	if cfg.ProcessTimeout == 0 {
		cfg.ProcessTimeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBufferSize <= 0 {
		cfg.MaxBufferSize = maxScanBuf
	}
	// Default transport runs the binary locally. WithBinary / cfg.Binary feed
	// the local transport, so existing configs keep working unchanged.
	if cfg.Transport == nil {
		cfg.Transport = transport.Local{Binary: cfg.Binary}
	}
}

// Result is the unified output of any run method. Fields not relevant to a
// given mode are left zero (e.g. Run only fills Response and Duration).
type Result struct {
	// Response is the plain-text stdout (Run only).
	Response string

	// ResultText is the final assistant text (RunJSON/RunStream).
	ResultText string

	// SessionID is the Claude session ID, when reported.
	SessionID string

	// TotalCostUSD is the invocation cost, when reported.
	TotalCostUSD float64

	// NumTurns is the agentic turns used, when reported.
	NumTurns int

	// IsError is true when the CLI reported a result-level error.
	IsError bool

	// Metadata holds the full parsed execution metadata (RunJSON).
	Metadata *claudecli.ExecutionMetadata

	// Duration is the wall-clock time of the invocation.
	Duration time.Duration

	// Attempts is the number of tries it took (1 unless run with retry). Set by
	// the *WithRetry methods.
	Attempts int
}

// ProgressFunc is invoked for each parsed stream event during RunStream.
// eventNum is the 1-based count of lines read so far. It is the seam where
// callers attach side effects such as heartbeats or live UI — the Runner
// itself stays infrastructure-free. Must not block for long; it runs inline on
// the read loop.
type ProgressFunc func(ev claudecli.StreamEvent, eventNum int)

// buildCmd assembles the *exec.Cmd via the configured transport: argv, working
// dir, environment, and stdin prompt. The caller wires stdout/stderr and runs
// it. The command is NOT context-bound — teardown is owned by startTeardown.
func (r *Runner) buildCmd(ctx context.Context, in Input, mode outputMode) *exec.Cmd {
	args := r.buildArgs(in, mode)

	var env []string
	if r.cfg.Entrypoint != "" {
		env = append(env, "CLAUDE_CODE_ENTRYPOINT="+r.cfg.Entrypoint)
	}
	env = append(env, r.cfg.Env...)
	env = append(env, in.Env...)

	cmd := r.cfg.Transport.Command(ctx, args, transport.CommandOpts{
		WorkDir: in.WorkDir,
		Env:     env,
	})
	cmd.Stdin = strings.NewReader(in.Prompt)
	return cmd
}

// startTeardown puts cmd in its own process group, starts it, and kills the
// group when cmdCtx is done (timeout/cancel). The returned channel must be
// closed once cmd.Wait() returns, to stop the watcher. Works the same for any
// transport — it tears down the local process (and its group); remote cleanup
// is the transport's caveat.
func startTeardown(cmdCtx context.Context, cmd *exec.Cmd) (chan struct{}, error) {
	procgroup.Setup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	procDone := make(chan struct{})
	go func() {
		select {
		case <-cmdCtx.Done():
			procgroup.Kill(cmd)
		case <-procDone:
		}
	}()
	return procDone, nil
}

// Run executes the CLI in plain-text mode (--print) and returns stdout.
func (r *Runner) Run(ctx context.Context, in Input) (res *Result, err error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()
	defer func() { r.observe("plain", in, start, res, err) }()
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modePlain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.cfg.Logger.Info("claude cli: run", "mode", "plain", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}
	err = cmd.Wait()
	close(procDone)
	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), Err: err}
		}
		return nil, &ProcessError{ExitCode: exitCodeOf(err), Stderr: sanitizeOutput(stdout.Bytes()), Err: err}
	}

	return &Result{
		Response: strings.TrimSpace(stdout.String()),
		Duration: time.Since(start),
	}, nil
}

// RunJSON executes the CLI in JSON mode and parses cost/session/token metadata.
func (r *Runner) RunJSON(ctx context.Context, in Input) (res *Result, err error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()
	defer func() { r.observe("json", in, start, res, err) }()
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modeJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.cfg.Logger.Info("claude cli: run", "mode", "json", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}
	err = cmd.Wait()
	close(procDone)
	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), Err: err}
		}
		return nil, &ProcessError{ExitCode: exitCodeOf(err), Stderr: sanitizeOutput(stderr.Bytes()), Err: err}
	}

	resultText, meta, err := claudecli.ParseOutput(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("parse claude output: %w", err)
	}

	res = &Result{ResultText: resultText, Metadata: meta, Duration: time.Since(start)}
	if meta != nil {
		res.SessionID = meta.SessionID
		res.TotalCostUSD = meta.TotalCostUSD
		res.NumTurns = meta.NumTurns
		res.IsError = meta.IsError
	}
	return res, nil
}

// RunStream executes the CLI in stream-json mode, invoking progress per event.
func (r *Runner) RunStream(ctx context.Context, in Input, progress ProgressFunc) (res *Result, err error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()
	defer func() { r.observe("stream", in, start, res, err) }()
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modeStream)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	if r.cfg.StderrFunc != nil {
		cmd.Stderr = io.MultiWriter(&stderr, &lineWriter{fn: r.cfg.StderrFunc})
	} else {
		cmd.Stderr = &stderr
	}

	r.cfg.Logger.Info("claude cli: run", "mode", "stream", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, r.cfg.MaxBufferSize), r.cfg.MaxBufferSize)

	var allLines [][]byte
	var resultEvent *claudecli.StreamEvent
	eventNum := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		allLines = append(allLines, lineCopy)
		eventNum++
		ev := claudecli.ParseStreamLine(lineCopy)
		if ev == nil {
			continue
		}
		if ev.IsResult() {
			resultEvent = ev
		}
		if progress != nil {
			progress(*ev, eventNum)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		r.cfg.Logger.Warn("claude cli: scanner error", "err", scanErr)
	}

	waitErr := cmd.Wait()
	close(procDone)
	if waitErr != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), Err: waitErr}
		}
		if cmdCtx.Err() != nil {
			return nil, fmt.Errorf("claude cli cancelled: %w", cmdCtx.Err())
		}
		return nil, &ProcessError{ExitCode: exitCodeOf(waitErr), Stderr: sanitizeOutput(stderr.Bytes()), Err: waitErr}
	}

	resultText, finalEvent := claudecli.ExtractTextFromStream(allLines)
	if finalEvent != nil {
		resultEvent = finalEvent
	}

	res = &Result{ResultText: resultText, Duration: time.Since(start)}
	if resultEvent != nil {
		res.SessionID = resultEvent.SessionID
		res.TotalCostUSD = resultEvent.Cost()
		res.NumTurns = resultEvent.NumTurns
		res.IsError = resultEvent.IsError
	}

	r.cfg.Logger.Info("claude cli: done", "mode", "stream", "events", eventNum, "cost_usd", res.TotalCostUSD, "turns", res.NumTurns, "duration", res.Duration)
	return res, nil
}

func (r *Runner) modelOf(in Input) string {
	if in.Model != "" {
		return in.Model
	}
	return r.cfg.DefaultModel
}

// lineWriter calls fn for each complete '\n'-terminated line written to it,
// buffering any partial trailing line until the next write.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// sanitizeOutput redacts embedded tokens and truncates long output for logs/errors.
func sanitizeOutput(output []byte) string {
	s := string(output)
	if idx := strings.Index(s, "x-access-token:"); idx != -1 {
		if end := strings.Index(s[idx:], "@"); end != -1 {
			s = s[:idx] + "x-access-token:***" + s[idx+end:]
		}
	}
	if len(s) > 2000 {
		s = s[:2000] + "... (truncated)"
	}
	return s
}
