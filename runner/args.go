package runner

import "strconv"

// outputMode selects the Claude CLI output format.
type outputMode int

const (
	// modePlain uses --print: plain text on stdout.
	modePlain outputMode = iota
	// modeJSON uses --output-format json: a single JSON result object/array.
	modeJSON
	// modeStream uses --output-format stream-json: newline-delimited events.
	modeStream
)

const flagAllowedTools = "--allowedTools"

// buildArgs assembles the CLI argv for a given input and output mode.
// The prompt itself is NOT included — it is streamed via stdin.
func (r *Runner) buildArgs(in Input, mode outputMode) []string {
	var args []string

	switch mode {
	case modePlain:
		args = append(args, "--print")
	case modeJSON:
		args = append(args, "--output-format", "json")
	case modeStream:
		args = append(args, "--output-format", "stream-json")
	}

	args = append(args, "--verbose")

	if r.cfg.skipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	model := in.Model
	if model == "" {
		model = r.cfg.DefaultModel
	}
	args = append(args, "--model", model)
	args = append(args, "--max-turns", strconv.Itoa(r.cfg.MaxTurns))

	// Budget is only meaningful in JSON/stream modes where we parse cost back.
	if budget := r.attemptBudget(in); budget != "" && mode != modePlain {
		args = append(args, "--max-budget-usd", budget)
	}

	// Partial-message and hook-event streaming only apply to stream mode.
	if mode == modeStream {
		if r.cfg.IncludePartialMessages {
			args = append(args, "--include-partial-messages")
		}
		if r.cfg.IncludeHookEvents {
			args = append(args, "--include-hook-events")
		}
	}

	if in.PermissionMode != "" {
		args = append(args, "--permission-mode", in.PermissionMode)
	}
	if in.Resume != "" {
		args = append(args, "--resume", in.Resume)
	}
	if in.Continue {
		args = append(args, "--continue")
	}
	if in.ForkSession {
		args = append(args, "--fork-session")
	}
	if in.SettingsPath != "" {
		args = append(args, "--settings", in.SettingsPath)
	}
	if in.MCPConfigPath != "" {
		args = append(args, "--mcp-config", in.MCPConfigPath)
	}
	for _, tool := range r.cfg.AllowedTools {
		args = append(args, flagAllowedTools, tool)
	}
	for _, d := range in.AddDirs {
		args = append(args, "--add-dir", d)
	}
	for _, f := range in.ContextFiles {
		args = append(args, "--add-context", f)
	}
	if in.SystemPrompt != "" {
		args = append(args, "--system-prompt", in.SystemPrompt)
	}

	args = append(args, in.ExtraArgs...)

	return args
}

// attemptBudget is the --max-budget-usd value for one invocation: the Runner's
// per-invocation budget, lowered to what a retry's MaxSpendUSD has left.
func (r *Runner) attemptBudget(in Input) string {
	if in.spendLeft <= 0 {
		return r.cfg.MaxBudgetUSD
	}
	if v, err := strconv.ParseFloat(r.cfg.MaxBudgetUSD, 64); err == nil && v > 0 && v <= in.spendLeft {
		return r.cfg.MaxBudgetUSD
	}
	return strconv.FormatFloat(in.spendLeft, 'f', -1, 64)
}
