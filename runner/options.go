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
)

// Config holds defaults for a Runner. Zero values are replaced with sane
// defaults in New, so a bare Config{} is usable.
type Config struct {
	// Binary is the path to the Claude CLI binary (default: "claude").
	Binary string

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

	// Entrypoint sets CLAUDE_CODE_ENTRYPOINT for telemetry/attribution.
	// Empty leaves it unset.
	Entrypoint string

	// Env is extra environment appended to os.Environ() for every invocation.
	Env []string

	// Logger receives structured logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Option mutates a Config. Use with New for ergonomic construction.
type Option func(*Config)

// WithBinary sets the CLI binary path.
func WithBinary(path string) Option { return func(c *Config) { c.Binary = path } }

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

// WithEnv appends extra environment variables ("KEY=value") to every invocation.
func WithEnv(env ...string) Option {
	return func(c *Config) { c.Env = append(c.Env, env...) }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option { return func(c *Config) { c.Logger = l } }

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
