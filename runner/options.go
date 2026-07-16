// Package runner executes the Claude Code CLI as a subprocess and parses its
// output. Infrastructure-specific concerns are kept out of the core and
// reached through neutral seams: a ProgressFunc callback for progress/heartbeats
// and a plain *slog.Logger for logging. Workspace/session/worktree management
// lives in the sibling workspace package; MCP config generation in the mcp
// package.
package runner

import (
	"log/slog"
	"time"

	"github.com/tggo/claude-agent-go/transport"
)

// Config holds defaults for a Runner. Zero values are replaced with sane
// defaults in New, so a bare Config{} is usable.
type Config struct {
	// Binary is the path to the Claude CLI binary (default: "claude"). Used to
	// build the default local Transport; ignored when Transport is set.
	Binary string

	// Transport decides how the binary is launched (local exec, docker exec,
	// ssh…). Defaults to transport.Local{Binary}. See the transport package.
	Transport transport.Transport

	// DefaultModel is used when an Input does not override it
	// (default: "sonnet"). Accepts CLI aliases (sonnet/opus/haiku) or full IDs.
	DefaultModel string

	// MaxTurns caps agentic turns per invocation (default: 50).
	MaxTurns int

	// MaxBudgetUSD caps spend per invocation, passed as --max-budget-usd in
	// JSON/stream modes (default: "5.00"). Empty disables the flag.
	MaxBudgetUSD string

	// ProcessTimeout bounds a single invocation (default: 15m).
	ProcessTimeout time.Duration

	// AllowedTools whitelists tools via repeated --allowedTools flags.
	// Nil/empty means all tools are allowed (combined with
	// --dangerously-skip-permissions, which Runner always passes for
	// non-interactive execution).
	AllowedTools []string

	// SkipPermissions controls the --dangerously-skip-permissions flag.
	// Defaults to true (required for headless runs). Set a pointer to false
	// via WithSkipPermissions to disable.
	skipPermissions bool

	// IncludePartialMessages enables --include-partial-messages in stream mode,
	// so RunStream's progress callback also receives "stream_event" lines
	// carrying token-level TextDelta()/ThinkingDelta() increments.
	IncludePartialMessages bool

	// IncludeHookEvents enables --include-hook-events in stream mode.
	IncludeHookEvents bool

	// MaxBufferSize bounds a single stream-json line in RunStream (default
	// 64 MiB). Values <= 0 are ignored and the default is used.
	MaxBufferSize int

	// StderrFunc, when set, is called for each line the CLI writes to stderr, as
	// it is written, in every run mode. Use it for live log capture during long
	// runs, and to keep diagnostics from a run that dies before its output is
	// collected. It runs inline on the stderr reader — keep it quick.
	StderrFunc func(line string)

	// Entrypoint sets CLAUDE_CODE_ENTRYPOINT for telemetry/attribution.
	// Empty leaves it unset.
	Entrypoint string

	// Env is extra environment appended to os.Environ() for every invocation.
	Env []string

	// Logger receives structured logs. Defaults to slog.Default().
	Logger *slog.Logger

	// Observer, if set, receives a RunRecord after every run (success or error)
	// — the seam for tracing/metrics. Dep-free: bridge it to OpenTelemetry,
	// Prometheus, or logs in a few lines (see docs). See WithObserver.
	Observer Observer
}

// Option mutates a Config. Use with New for ergonomic construction.
type Option func(*Config)

// WithBinary sets the CLI binary path (feeds the default local transport).
func WithBinary(path string) Option { return func(c *Config) { c.Binary = path } }

// WithTransport sets how the binary is launched (local, docker, ssh…).
func WithTransport(t transport.Transport) Option {
	return func(c *Config) { c.Transport = t }
}

// WithModel sets the default model.
func WithModel(model string) Option { return func(c *Config) { c.DefaultModel = model } }

// WithMaxTurns sets the agentic turn cap.
func WithMaxTurns(n int) Option { return func(c *Config) { c.MaxTurns = n } }

// WithMaxBudgetUSD sets the per-invocation spend cap (e.g. "5.00"). Empty disables.
func WithMaxBudgetUSD(usd string) Option { return func(c *Config) { c.MaxBudgetUSD = usd } }

// WithTimeout sets the per-invocation timeout.
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.ProcessTimeout = d } }

// WithAllowedTools restricts the tool whitelist.
func WithAllowedTools(tools ...string) Option {
	return func(c *Config) { c.AllowedTools = tools }
}

// WithSkipPermissions toggles --dangerously-skip-permissions (default true).
func WithSkipPermissions(skip bool) Option {
	return func(c *Config) { c.skipPermissions = skip }
}

// WithEntrypoint sets CLAUDE_CODE_ENTRYPOINT.
func WithEntrypoint(e string) Option { return func(c *Config) { c.Entrypoint = e } }

// WithPartialMessages enables token-level streaming in RunStream.
func WithPartialMessages(on bool) Option {
	return func(c *Config) { c.IncludePartialMessages = on }
}

// WithHookEvents enables hook lifecycle events in RunStream.
func WithHookEvents(on bool) Option {
	return func(c *Config) { c.IncludeHookEvents = on }
}

// WithMaxBufferSize sets the max stream-json line size (bytes) for RunStream.
// Non-positive values are ignored, keeping the 64 MiB default — this guards
// against a scanner panic from a negative/zero buffer.
func WithMaxBufferSize(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.MaxBufferSize = n
		}
	}
}

// WithStderrCallback streams the CLI's stderr lines to fn as they are written,
// in every run mode (Run, RunJSON, RunStream).
func WithStderrCallback(fn func(line string)) Option {
	return func(c *Config) { c.StderrFunc = fn }
}

// WithEnv appends extra environment variables ("KEY=value") to every invocation.
func WithEnv(env ...string) Option {
	return func(c *Config) { c.Env = append(c.Env, env...) }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option { return func(c *Config) { c.Logger = l } }

// WithObserver sets the run observer for tracing/metrics.
func WithObserver(o Observer) Option { return func(c *Config) { c.Observer = o } }

// Input describes a single Claude CLI invocation. Working-directory and
// session/MCP-file management are the caller's responsibility (see the
// workspace and mcp packages) — Input takes the resolved paths directly.
type Input struct {
	// Prompt is the user prompt. Sent via stdin to avoid argv length limits.
	Prompt string

	// Model overrides Config.DefaultModel for this invocation. Empty uses the default.
	Model string

	// SystemPrompt, when set, is passed via --system-prompt.
	SystemPrompt string

	// ContextFiles are passed via repeated --add-context flags.
	ContextFiles []string

	// MCPConfigPath, when set, is passed via --mcp-config (see mcp.WriteConfig).
	MCPConfigPath string

	// WorkDir is the subprocess working directory. Empty uses the current dir.
	WorkDir string

	// Resume, when set, resumes a prior session by ID (--resume).
	Resume string

	// Continue resumes the most recent session in WorkDir (--continue).
	Continue bool

	// ForkSession, with Resume, forks the resumed session into a new one
	// (--fork-session) instead of continuing it in place.
	ForkSession bool

	// PermissionMode sets --permission-mode (e.g. "acceptEdits", "plan",
	// "bypassPermissions"). Empty leaves it to the CLI default. Note: when
	// Config.skipPermissions is true, --dangerously-skip-permissions is also
	// passed and takes precedence.
	PermissionMode string

	// AddDirs grants the agent access to extra directories (--add-dir, repeated).
	AddDirs []string

	// SettingsPath, when set, passes a settings JSON file via --settings.
	// Use this to configure shell-command hooks and permission rules.
	SettingsPath string

	// ExtraArgs are appended verbatim to the CLI argv, after all generated
	// flags. An escape hatch for CLI flags the SDK does not model yet.
	ExtraArgs []string

	// Env is extra per-invocation environment appended after Config.Env.
	Env []string
}
