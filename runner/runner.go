package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/internal/procgroup"
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
}

// ProgressFunc is invoked for each parsed stream event during RunStream.
// eventNum is the 1-based count of lines read so far. It is the seam where
// callers attach side effects such as heartbeats or live UI — the
// Runner itself stays infrastructure-free. Must not block for long; it runs
// inline on the read loop.
type ProgressFunc func(ev claudecli.StreamEvent, eventNum int)

// prepareCmd builds the *exec.Cmd shared by all modes: argv, working dir,
// environment, stdin prompt. The caller wires stdout/stderr and runs it.
func (r *Runner) prepareCmd(ctx context.Context, in Input, mode outputMode) *exec.Cmd {
	args := r.buildArgs(in, mode)

	//nolint:gosec // binary path comes from trusted config, not user input.
	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Dir = in.WorkDir
	cmd.Stdin = strings.NewReader(in.Prompt)

	env := os.Environ()
	if r.cfg.Entrypoint != "" {
		env = append(env, "CLAUDE_CODE_ENTRYPOINT="+r.cfg.Entrypoint)
	}
	env = append(env, r.cfg.Env...)
	env = append(env, in.Env...)
	cmd.Env = env

	return cmd
}

// Run executes the CLI in plain-text mode (--print) and returns stdout.
// Use when you only need the final text and no cost/session metadata.
func (r *Runner) Run(ctx context.Context, in Input) (*Result, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()

	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.prepareCmd(cmdCtx, in, modePlain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.cfg.Logger.Info("claude cli: run", "mode", "plain", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	if err := cmd.Run(); err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude cli timeout after %v: %w", r.cfg.ProcessTimeout, err)
		}
		return nil, fmt.Errorf("claude cli execution failed: %w (output: %s)", err, sanitizeOutput(stdout.Bytes()))
	}

	return &Result{
		Response: strings.TrimSpace(stdout.String()),
		Duration: time.Since(start),
	}, nil
}

// RunJSON executes the CLI in JSON mode (--output-format json), buffering the
// full output and parsing it with claudecli.ParseOutput. Returns rich metadata
// (cost, tokens, session, model usage). Best for short-to-medium runs where
// streaming heartbeats are unnecessary.
func (r *Runner) RunJSON(ctx context.Context, in Input) (*Result, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()

	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.prepareCmd(cmdCtx, in, modeJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.cfg.Logger.Info("claude cli: run", "mode", "json", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	if err := cmd.Run(); err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude cli timeout after %v: %w", r.cfg.ProcessTimeout, err)
		}
		return nil, fmt.Errorf("claude cli execution failed: %w (stderr: %s)", err, sanitizeOutput(stderr.Bytes()))
	}

	resultText, meta, err := claudecli.ParseOutput(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("parse claude output: %w", err)
	}

	res := &Result{
		ResultText: resultText,
		Metadata:   meta,
		Duration:   time.Since(start),
	}
	if meta != nil {
		res.SessionID = meta.SessionID
		res.TotalCostUSD = meta.TotalCostUSD
		res.NumTurns = meta.NumTurns
	}
	return res, nil
}

// RunStream executes the CLI in stream-json mode, reading newline-delimited
// events as they arrive and invoking progress (if non-nil) per event. This is
// the mode for long-running autonomous tasks: it lets callers emit heartbeats
// so an outer deadline (an HTTP request timeout, a job heartbeat) does not fire. The
// whole process group is killed on context cancellation or timeout.
func (r *Runner) RunStream(ctx context.Context, in Input, progress ProgressFunc) (*Result, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	start := time.Now()

	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	// Use plain Command (not CommandContext) so we control teardown via the
	// process group — CommandContext would only kill the leader, orphaning
	// git/test children spawned by the agent.
	args := r.buildArgs(in, modeStream)
	//nolint:gosec // binary path comes from trusted config.
	cmd := exec.Command(r.cfg.Binary, args...)
	cmd.Dir = in.WorkDir
	cmd.Stdin = strings.NewReader(in.Prompt)
	env := os.Environ()
	if r.cfg.Entrypoint != "" {
		env = append(env, "CLAUDE_CODE_ENTRYPOINT="+r.cfg.Entrypoint)
	}
	env = append(env, r.cfg.Env...)
	env = append(env, in.Env...)
	cmd.Env = env
	procgroup.Setup(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	r.cfg.Logger.Info("claude cli: run", "mode", "stream", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude cli start failed: %w", err)
	}

	// Kill the whole group on cancellation/timeout.
	procDone := make(chan struct{})
	go func() {
		select {
		case <-cmdCtx.Done():
			procgroup.Kill(cmd)
		case <-procDone:
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, maxScanBuf), maxScanBuf)

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
			return nil, fmt.Errorf("claude cli timeout after %v: %w", r.cfg.ProcessTimeout, waitErr)
		}
		if cmdCtx.Err() != nil {
			return nil, fmt.Errorf("claude cli cancelled: %w", cmdCtx.Err())
		}
		return nil, fmt.Errorf("claude cli execution failed: %w (stderr: %s)", waitErr, sanitizeOutput(stderr.Bytes()))
	}

	resultText, finalEvent := claudecli.ExtractTextFromStream(allLines)
	if finalEvent != nil {
		resultEvent = finalEvent
	}

	res := &Result{
		ResultText: resultText,
		Duration:   time.Since(start),
	}
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
