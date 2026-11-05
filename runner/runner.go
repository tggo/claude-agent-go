package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/internal/cliout"
	"github.com/tggo/claude-agent-go/internal/procgroup"
	"github.com/tggo/claude-agent-go/recording"
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

	// killGrace is how long a cancelled/timed-out CLI gets between SIGTERM and
	// SIGKILL. A process that traps SIGTERM must not hang the caller.
	killGrace = 2 * time.Second
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

	// Subtype is the result event's subtype ("success", "error_max_turns",
	// "error_during_execution", ...), when reported. See IsMaxTurns.
	Subtype string

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
			procgroup.KillGraceful(cmd, killGrace, procDone)
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
	if in, err = r.beginRun(ctx, in); err != nil {
		return nil, err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modePlain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = r.stderrWriter(&stderr)

	r.cfg.Logger.Info("claude cli: run", "mode", "plain", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}
	err = cmd.Wait()
	close(procDone)
	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), Err: errors.Join(cmdCtx.Err(), err)}
		}
		if cmdCtx.Err() != nil {
			return nil, fmt.Errorf("claude cli cancelled: %w", cmdCtx.Err())
		}
		return nil, processError(cmd, stdout.Bytes(), stderr.Bytes(), err)
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
	if in, err = r.beginRun(ctx, in); err != nil {
		return nil, err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modeJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = r.stderrWriter(&stderr)

	r.cfg.Logger.Info("claude cli: run", "mode", "json", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}
	err = cmd.Wait()
	close(procDone)
	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return nil, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), Err: errors.Join(cmdCtx.Err(), err)}
		}
		if cmdCtx.Err() != nil {
			return nil, fmt.Errorf("claude cli cancelled: %w", cmdCtx.Err())
		}
		return nil, processError(cmd, stdout.Bytes(), stderr.Bytes(), err)
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
		res.Subtype = meta.Subtype
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
	if in, err = r.beginRun(ctx, in); err != nil {
		return nil, err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.ProcessTimeout)
	defer cancel()

	cmd := r.buildCmd(cmdCtx, in, modeStream)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = r.stderrWriter(&stderr)

	r.cfg.Logger.Info("claude cli: run", "mode", "stream", "model", r.modelOf(in), "work_dir", in.WorkDir, "prompt_len", len(in.Prompt))

	procDone, err := startTeardown(cmdCtx, cmd)
	if err != nil {
		return nil, &CLINotFoundError{Binary: r.cfg.Binary, Err: err}
	}

	rec := r.openRecorder(ctx, in, start)
	defer func() {
		if rec != nil {
			if cerr := rec.Close(); cerr != nil {
				r.cfg.Logger.Warn("claude cli: recording close", "err", cerr)
			}
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, r.cfg.MaxBufferSize), r.cfg.MaxBufferSize)

	var allLines [][]byte
	var resultEvent *claudecli.StreamEvent
	sessionID := ""
	eventNum := 0
	// abortErr is set when we stop the run ourselves (a line over the buffer
	// limit, a plugin vetoing an event). The process group is killed at once so
	// the agent stops spending; the partial result is returned with the error.
	var abortErr error
	abort := func(e error) {
		abortErr = e
		go procgroup.KillGraceful(cmd, killGrace, procDone)
	}
scan:
	for scanner.Scan() {
		line := scanner.Bytes()
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		allLines = append(allLines, lineCopy)
		eventNum++
		if rec != nil {
			if werr := rec.WriteLine(lineCopy); werr != nil {
				r.cfg.Logger.Warn("claude cli: recording write failed; continuing unrecorded", "err", werr)
				rec.Close()
				rec = nil
			}
		}
		ev := claudecli.ParseStreamLine(lineCopy)
		if ev == nil {
			continue
		}
		if ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		if ev.IsResult() {
			resultEvent = ev
		}
		if perr := r.pluginEvent(ctx, *ev); perr != nil {
			abort(perr)
			break scan
		}
		if progress != nil {
			progress(*ev, eventNum)
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		r.cfg.Logger.Warn("claude cli: scanner error", "err", scanErr)
		if errors.Is(scanErr, bufio.ErrTooLong) && abortErr == nil {
			// Deterministic, not a truncation. Stop the agent now instead of
			// draining output we cannot parse while it keeps running.
			abort(&LineTooLongError{Limit: r.cfg.MaxBufferSize, SessionID: sessionID, Err: scanErr})
		} else {
			// The scanner stopped reading; drain the pipe so the child is not
			// left blocked on a write until ProcessTimeout.
			_, _ = io.Copy(io.Discard, stdoutPipe)
		}
	}

	waitErr := cmd.Wait()
	close(procDone)
	if abortErr != nil {
		text, _ := claudecli.ExtractTextFromStream(allLines)
		partial := &Result{ResultText: text, SessionID: sessionID, Duration: time.Since(start)}
		var lt *LineTooLongError
		if errors.As(abortErr, &lt) {
			lt.PartialText = text
		}
		var pa *PluginAbortError
		if errors.As(abortErr, &pa) {
			pa.SessionID = sessionID
		}
		return partial, abortErr
	}
	if scanErr != nil && cmdCtx.Err() == nil {
		// Deterministic (e.g. bufio.ErrTooLong): not a truncation, not retryable.
		return nil, fmt.Errorf("claude cli: read stream (raise WithMaxBufferSize if a line is too long): %w", scanErr)
	}
	if waitErr != nil {
		if cmdCtx.Err() != nil {
			// Cancelled or timed out: hand back what the run produced so far
			// (text, session id) so the caller can resume or show it.
			text, _ := claudecli.ExtractTextFromStream(allLines)
			partial := &Result{ResultText: text, SessionID: sessionID, Duration: time.Since(start)}
			if cmdCtx.Err() == context.DeadlineExceeded {
				return partial, &TimeoutError{Timeout: r.cfg.ProcessTimeout.String(), SessionID: sessionID, Err: errors.Join(cmdCtx.Err(), waitErr)}
			}
			return partial, fmt.Errorf("claude cli cancelled: %w", cmdCtx.Err())
		}
		pe := processError(cmd, tailBytes(allLines, 20), stderr.Bytes(), waitErr)
		pe.SessionID = sessionID
		return nil, pe
	}

	resultText, finalEvent := claudecli.ExtractTextFromStream(allLines)
	if finalEvent != nil {
		resultEvent = finalEvent
	}

	res = &Result{ResultText: resultText, SessionID: sessionID, Duration: time.Since(start)}
	if resultEvent == nil {
		r.cfg.Logger.Warn("claude cli: stream ended without a result event", "events", eventNum, "session_id", sessionID)
		return res, &TruncatedStreamError{SessionID: sessionID, Events: eventNum, PartialText: resultText}
	}
	if resultEvent.SessionID != "" {
		res.SessionID = resultEvent.SessionID
	}
	res.TotalCostUSD = resultEvent.Cost()
	res.NumTurns = resultEvent.NumTurns
	res.IsError = resultEvent.IsError
	res.Subtype = resultEvent.Subtype

	r.cfg.Logger.Info("claude cli: done", "mode", "stream", "events", eventNum, "cost_usd", res.TotalCostUSD, "turns", res.NumTurns, "duration", res.Duration)
	return res, nil
}

// openRecorder opens a recording for a stream run, or returns nil when no
// Recorder is configured or it fails (logged; the run is not blocked).
func (r *Runner) openRecorder(ctx context.Context, in Input, start time.Time) *recording.Writer {
	if r.cfg.Recorder == nil {
		return nil
	}
	w, err := r.cfg.Recorder(recording.Header{
		Label:     in.Label,
		Prompt:    in.Prompt,
		Model:     r.modelOf(in),
		WorkDir:   in.WorkDir,
		StartedAt: start,
		Attempt:   attemptOf(ctx),
	})
	if err != nil {
		r.cfg.Logger.Warn("claude cli: recorder failed; running unrecorded", "err", err)
		return nil
	}
	return w
}

func attemptOf(ctx context.Context) int {
	n, _ := ctx.Value(attemptKey{}).(int)
	return n
}

func (r *Runner) modelOf(in Input) string {
	if in.Model != "" {
		return in.Model
	}
	return r.cfg.DefaultModel
}

// maxSnippet bounds how much of a stream survives into an error message.
const maxSnippet = 2000

// sanitizeOutput redacts embedded tokens and truncates long output for logs/errors.
func sanitizeOutput(output []byte) string {
	s := cliout.RedactTokens(string(output))
	if len(s) > maxSnippet {
		s = s[:maxSnippet] + "... (truncated)"
	}
	return s
}

// sanitizeTail is sanitizeOutput keeping the END of the output instead of the
// start. Used for stdout in errors: when a run dies, the last bytes written are
// the diagnostic — the beginning is just the transcript up to that point.
func sanitizeTail(output []byte) string {
	s := cliout.RedactTokens(string(output))
	if len(s) > maxSnippet {
		s = "(truncated) ..." + s[len(s)-maxSnippet:]
	}
	return s
}

// stderrWriter wires the child's stderr: always into buf, and additionally
// line-by-line to StderrFunc when the caller asked for it. Teeing it out as it
// is written means a run that dies mid-flight has already delivered its
// diagnostics, rather than depending on the final buffer surviving into an error.
func (r *Runner) stderrWriter(buf *bytes.Buffer) io.Writer {
	if r.cfg.StderrFunc == nil {
		return buf
	}
	return io.MultiWriter(buf, &cliout.LineWriter{Fn: r.cfg.StderrFunc})
}

// processError builds the error for a non-zero exit, carrying every diagnostic
// available: both streams and the OS's record of the process. cmd must already
// have been waited on, so cmd.ProcessState is populated.
func processError(cmd *exec.Cmd, stdout, stderr []byte, err error) *ProcessError {
	return &ProcessError{
		ExitCode:     exitCodeOf(err),
		Stderr:       sanitizeOutput(stderr),
		Stdout:       sanitizeTail(stdout),
		ProcessState: cmd.ProcessState,
		Err:          err,
	}
}

// tailBytes joins the last n lines, the stdout an aborted stream run got
// through. Bounded by line count because a single stream-json line can be
// megabytes; sanitizeTail then bounds the bytes.
func tailBytes(lines [][]byte, n int) []byte {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, []byte("\n"))
}
