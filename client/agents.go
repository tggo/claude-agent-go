package client

// AgentDefinition declares a custom subagent inline (from Go), without a
// .claude/agents/*.md file. Declared to the CLI via the initialize handshake;
// the main agent can then delegate to it through the Task tool. The JSON field
// names match the CLI wire format (camelCase); zero/empty optional fields are
// omitted. Description and Prompt are required.
type AgentDefinition struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`

	Tools           []string `json:"tools,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	// Model alias ("sonnet", "opus", "haiku", "inherit") or a full model ID.
	Model         string   `json:"model,omitempty"`
	Skills        []string `json:"skills,omitempty"`
	Memory        string   `json:"memory,omitempty"` // "user" | "project" | "local"
	InitialPrompt string   `json:"initialPrompt,omitempty"`
	MaxTurns      int      `json:"maxTurns,omitempty"`
	Background    *bool    `json:"background,omitempty"`
	// Effort is a level ("low"|"medium"|"high") or, for advanced use, a number;
	// kept as any so either form passes through unchanged. nil omits it.
	Effort         any    `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	// MCPServers lists server names (string) or inline {name: config} objects.
	MCPServers []any `json:"mcpServers,omitempty"`
}
