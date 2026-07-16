// Package client provides an interactive, multi-turn session with the Claude
// CLI over its bidirectional stream-json protocol. Unlike runner (one process
// per call), a Client keeps a single `claude` process alive and feeds it user
// turns on stdin, reading streamed events on stdout — the Go analogue of the
// Python SDK's ClaudeSDKClient. It supports mid-session interrupts and full
// conversational context across turns.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/internal/cliout"
	"github.com/tggo/claude-agent-go/internal/procgroup"
	"github.com/tggo/claude-agent-go/transport"
)

// Config configures a Client. Zero values get sane defaults in New.
type Config struct {
	Binary          string              // default "claude" (feeds the default transport)
	Transport       transport.Transport // how the binary is launched; default transport.Local{Binary}
	Model           string              // default "sonnet"
	WorkDir         string              // subprocess working dir
	SystemPrompt    string              // --system-prompt
	AllowedTools    []string            // repeated --allowedTools
	MCPConfigPath   string              // --mcp-config
	PermissionMode  string              // --permission-mode
	SkipPermissions bool                // --dangerously-skip-permissions (default true)
	MaxTurns        int                 // --max-turns (default 50)
	Entrypoint      string              // CLAUDE_CODE_ENTRYPOINT
	Env             []string            // extra env
	ExtraArgs       []string            // verbatim passthrough
	StartTimeout    time.Duration       // bound on process startup (default 30s)

	// IncludePartialMessages enables token-level streaming
	// (--include-partial-messages): Query's onEvent additionally receives
	// "stream_event" lines whose TextDelta()/ThinkingDelta() carry increments.
	IncludePartialMessages bool

	// IncludeHookEvents enables --include-hook-events: hook lifecycle events are
	// surfaced in the stream for observation.
	IncludeHookEvents bool

	// ControlTimeout bounds how long a control request (Initialize/Interrupt)
	// waits for its acknowledgement (default 30s).
	ControlTimeout time.Duration

	// CallbackTimeout bounds how long a CanUseTool / hook callback may run
	// before the SDK abandons it and lets the turn proceed (default 60s). A
	// hung callback would otherwise stall the agent indefinitely.
	CallbackTimeout time.Duration

	// CanUseTool, when set, is invoked for every tool call the agent attempts,
	// to allow/deny it (the can_use_tool control callback). Setting it switches
	// the session into permission-routing mode: --permission-prompt-tool stdio
	// is added and --dangerously-skip-permissions is dropped.
	CanUseTool CanUseToolFunc

	// Hooks registers hook callbacks keyed by event name ("PreToolUse",
	// "PostToolUse", "Stop", …). Declared to the CLI via the initialize
	// handshake; the CLI then calls them over the control protocol.
	Hooks map[string][]HookMatcher

	// Agents declares custom subagents inline (keyed by name), sent via the
	// initialize handshake. The main agent can delegate to them via the Task
	// tool — no .claude/agents files needed.
	Agents map[string]AgentDefinition

	// Skills is an allowlist of skill names available to the session, sent via
	// initialize. Nil means no restriction (all available skills).
	Skills []string

	// StderrFunc, when set, is called for each line the CLI writes to stderr, as
	// it is written, and the SDK stops forwarding stderr to the parent process.
	// This is the seam for capturing CLI diagnostics into a structured logger.
	// When nil, stderr is forwarded to os.Stderr — which a caller that doesn't
	// tail its own stderr will never see. It runs inline on the stderr reader —
	// keep it quick. (The session's exit error carries a stderr snippet either
	// way; see Close.)
	StderrFunc func(line string)

	Logger *slog.Logger
}

// Turn is the outcome of a single Query.
type Turn struct {
	// Text is the assistant's final result text for this turn.
	Text string
	// SessionID is the session this turn belongs to.
	SessionID string
	// TotalCostUSD is the cumulative cost reported at the turn's result.
	TotalCostUSD float64
	// NumTurns is the agentic turn count reported by the CLI.
	NumTurns int
	// IsError is true when the CLI reported a result-level error.
	IsError bool
	// Events are all stream events observed during this turn, in order.
	Events []claudecli.StreamEvent
}

// Client is an interactive Claude session. It is NOT safe for concurrent
// Query calls — turns are sequential — but Interrupt and Close may be called
// from other goroutines.
type Client struct {
	cfg        Config
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stderrTail *tailBuffer
	events     chan *claudecli.StreamEvent

	writeMu sync.Mutex // serializes writes to stdin
	queryMu sync.Mutex // single-flight Query

	reqCounter atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan *controlResponse

	hookCallbacks map[string]HookCallback // callback_id -> callback

	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
	closed   atomic.Bool
}

const (
	defaultBinary  = "claude"
	defaultModel   = "sonnet"
	defaultTurns   = 50
	defaultStartTO = 30 * time.Second
	scanBuf        = 64 * 1024 * 1024
)

// New starts a Claude CLI process in interactive stream-json mode.
func New(ctx context.Context, cfg Config) (*Client, error) {
	applyClientDefaults(&cfg)

	args := buildClientArgs(cfg)

	var env []string
	if cfg.Entrypoint != "" {
		env = append(env, "CLAUDE_CODE_ENTRYPOINT="+cfg.Entrypoint)
	}
	env = append(env, cfg.Env...)

	cmd := cfg.Transport.Command(ctx, args, transport.CommandOpts{
		WorkDir: cfg.WorkDir,
		Env:     env,
	})
	procgroup.Setup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Always retain the stderr tail so a dead session can say why it died; then
	// either hand lines to the caller's callback or keep the historical
	// passthrough to the parent's stderr.
	stderrTail := &tailBuffer{}
	if cfg.StderrFunc != nil {
		cmd.Stderr = io.MultiWriter(stderrTail, &cliout.LineWriter{Fn: cfg.StderrFunc})
	} else {
		cmd.Stderr = io.MultiWriter(stderrTail, os.Stderr)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	c := &Client{
		cfg:           cfg,
		cmd:           cmd,
		stdin:         stdin,
		stderrTail:    stderrTail,
		events:        make(chan *claudecli.StreamEvent, 64),
		pending:       make(map[string]chan *controlResponse),
		hookCallbacks: make(map[string]HookCallback),
		done:          make(chan struct{}),
	}

	go c.readLoop(stdout)

	// When permission/hook callbacks, inline agents, or a skills allowlist are
	// configured, the CLI must learn about them via the initialize handshake
	// before the first turn.
	if cfg.CanUseTool != nil || len(cfg.Hooks) > 0 || len(cfg.Agents) > 0 || cfg.Skills != nil {
		ictx, icancel := context.WithTimeout(ctx, cfg.StartTimeout)
		defer icancel()
		if _, err := c.Initialize(ictx); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("initialize control protocol: %w", err)
		}
	}

	return c, nil
}

func applyClientDefaults(cfg *Config) {
	if cfg.Binary == "" {
		cfg.Binary = defaultBinary
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = defaultTurns
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = defaultStartTO
	}
	if cfg.ControlTimeout == 0 {
		cfg.ControlTimeout = defaultControlTimeout
	}
	if cfg.Transport == nil {
		cfg.Transport = transport.Local{Binary: cfg.Binary}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Permission routing wins: with a can_use_tool callback the SDK must see
	// permission prompts, so never bypass them.
	if cfg.CanUseTool != nil {
		cfg.SkipPermissions = false
		if cfg.PermissionMode == "" {
			cfg.PermissionMode = "default"
		}
		return
	}
	// Otherwise SkipPermissions defaults to true unless a permission mode is
	// set — New cannot distinguish unset from false, so the documented default
	// is true and applied only when no permission mode is requested.
	if !cfg.SkipPermissions && cfg.PermissionMode == "" {
		cfg.SkipPermissions = true
	}
}

func buildClientArgs(cfg Config) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", cfg.Model,
		"--max-turns", strconv.Itoa(cfg.MaxTurns),
	}
	if cfg.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if cfg.CanUseTool != nil {
		// Route tool-permission prompts to us over the control protocol.
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if cfg.MCPConfigPath != "" {
		args = append(args, "--mcp-config", cfg.MCPConfigPath)
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if cfg.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	args = append(args, cfg.ExtraArgs...)
	return args
}

// readLoop scans stdout, routes control_response lines to the pending waiter
// for their request_id, and fans the rest out on c.events as stream events,
// until EOF. Then it records the process exit status and closes the channels.
func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, scanBuf), scanBuf)
	for scanner.Scan() {
		line := scanner.Bytes()
		cp := make([]byte, len(line))
		copy(cp, line)

		// Lightweight type probe before full parsing.
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(cp, &probe)

		if probe.Type == "control_response" {
			c.routeControlResponse(cp)
			continue
		}
		if probe.Type == "control_request" {
			// CLI is asking us to decide something (permission/hook). Handle
			// off the read loop so a slow callback doesn't stall stdout.
			go c.handleControlRequest(cp)
			continue
		}
		if ev := claudecli.ParseStreamLine(cp); ev != nil {
			c.events <- ev
		}
	}
	werr := c.cmd.Wait()
	c.waitOnce.Do(func() { c.waitErr = werr })
	c.failPending(fmt.Errorf("process ended"))
	close(c.events)
	close(c.done)
}

// userMessage is the stdin envelope for a user turn.
type userMessage struct {
	Type    string          `json:"type"`
	Message userMessageBody `json:"message"`
}
type userMessageBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *Client) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(b); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
}

// Query sends a user turn and blocks until the turn's result event, returning
// the accumulated Turn. onEvent, if non-nil, is called for every stream event
// of this turn (assistant deltas, system, result) as they arrive — the seam for
// live UI or heartbeats. Concurrent Query calls are serialized.
func (c *Client) Query(ctx context.Context, prompt string, onEvent func(claudecli.StreamEvent)) (*Turn, error) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()

	if c.closed.Load() {
		return nil, fmt.Errorf("client is closed")
	}

	if err := c.writeLine(userMessage{
		Type:    "user",
		Message: userMessageBody{Role: "user", Content: prompt},
	}); err != nil {
		return nil, err
	}

	turn := &Turn{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-c.events:
			if !ok {
				return nil, fmt.Errorf("claude process ended before result: %w", c.exitErr())
			}
			turn.Events = append(turn.Events, *ev)
			if onEvent != nil {
				onEvent(*ev)
			}
			if ev.IsResult() {
				turn.Text = ev.Result
				turn.SessionID = ev.SessionID
				turn.TotalCostUSD = ev.Cost()
				turn.NumTurns = ev.NumTurns
				turn.IsError = ev.IsError
				if turn.Text == "" {
					turn.Text = assistantTextOf(turn.Events)
				}
				return turn, nil
			}
		}
	}
}

// Close shuts the session down: closes stdin (signalling EOF), waits briefly
// for graceful exit, then kills the process group. Safe to call multiple times.
// A non-nil error reports a non-zero exit, with the tail of the CLI's stderr
// appended so the failure is diagnosable.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	_ = c.stdin.Close()

	select {
	case <-c.done:
		return c.exitErr()
	case <-time.After(5 * time.Second):
		procgroup.Kill(c.cmd)
		<-c.done
		return c.exitErr()
	}
}

// Wait blocks until the underlying process exits, returning its exit error.
func (c *Client) Wait() error {
	<-c.done
	return c.exitErr()
}

// exitErr reports how the session ended, attaching the CLI's stderr tail. Bare,
// cmd.Wait's error is just "exit status 1" — the process's own account of why it
// died is the whole diagnostic, and it is otherwise discarded.
func (c *Client) exitErr() error {
	c.waitOnce.Do(func() {}) // ensure waitErr is settled if readLoop set it
	if c.waitErr == nil {
		return nil
	}
	if s := c.stderrTail.snippet(); s != "" {
		return fmt.Errorf("%w: %s", c.waitErr, s)
	}
	return c.waitErr
}

func assistantTextOf(events []claudecli.StreamEvent) string {
	var s string
	for i := range events {
		if t := events[i].AssistantText(); t != "" {
			if s != "" {
				s += "\n"
			}
			s += t
		}
	}
	return s
}
