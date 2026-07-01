// Package claudecli provides types and utilities for parsing Claude CLI JSON output.
// It consolidates output parsing logic used across multiple domains (code review, PRD sessions).
package claudecli

import "encoding/json"

// ClaudeOutputMessage represents a single message in Claude CLI JSON output.
// The output is a JSON array containing multiple message types:
// - "system" with subtype "init" - initialization info
// - "assistant" - Claude's response messages
// - "result" - final result with execution metadata
type ClaudeOutputMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`

	// Init message fields (type="system", subtype="init")
	SessionID      string            `json:"session_id,omitempty"`
	CWD            string            `json:"cwd,omitempty"`
	Model          string            `json:"model,omitempty"`
	Tools          []string          `json:"tools,omitempty"`
	MCPServers     []MCPServerStatus `json:"mcp_servers,omitempty"`
	PermissionMode string            `json:"permissionMode,omitempty"`
	ClaudeVersion  string            `json:"claude_code_version,omitempty"`

	// Assistant message fields (type="assistant")
	Message *AssistantMessage `json:"message,omitempty"`

	// Result message fields (type="result")
	IsError        bool            `json:"is_error,omitempty"`
	DurationMs     int64           `json:"duration_ms,omitempty"`
	DurationAPI    int64           `json:"duration_api_ms,omitempty"`
	NumTurns       int             `json:"num_turns,omitempty"`
	Result         string          `json:"result,omitempty"`
	TotalCostUSD   float64         `json:"total_cost_usd,omitempty"`
	APIErrorStatus json.RawMessage `json:"api_error_status,omitempty"`

	// Usage fields (in result message)
	Usage      *TokenUsage            `json:"usage,omitempty"`
	ModelUsage map[string]*ModelUsage `json:"modelUsage,omitempty"`

	// StructuredOutput carries a structured (object) result when the CLI emits
	// one, kept raw so a string Result field never fails to parse.
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

// MCPServerStatus represents the status of an MCP server connection.
type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "connected", "disconnected", "disabled", "error"
}

// AssistantMessage represents Claude's response message.
type AssistantMessage struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason,omitempty"`
	Usage      *MessageUsage  `json:"usage,omitempty"`
}

// ContentBlock represents a content block in Claude's response. A block's Type
// selects which fields are populated:
//   - "text"        → Text
//   - "thinking"    → Thinking (extended-thinking reasoning)
//   - "tool_use"    → ID, Name, Input (the tool call the agent made)
//   - "tool_result" → ToolUseID, Content, IsError (the result fed back)
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// thinking blocks
	Thinking string `json:"thinking,omitempty"`

	// tool_use blocks
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result blocks
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Block type constants for ContentBlock.Type.
const (
	BlockText       = "text"
	BlockThinking   = "thinking"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// MessageUsage represents token usage for a single message.
type MessageUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// TokenUsage represents aggregate token usage from Claude CLI.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ModelUsage represents per-model usage statistics.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	WebSearchRequests        int     `json:"webSearchRequests,omitempty"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow,omitempty"`
	MaxOutputTokens          int     `json:"maxOutputTokens,omitempty"`
}

// ExecutionMetadata contains extracted execution data from Claude CLI output.
// This is stored alongside the review for analytics and debugging.
type ExecutionMetadata struct {
	SessionID      string                 `json:"session_id" bson:"session_id"`
	TotalCostUSD   float64                `json:"total_cost_usd" bson:"total_cost_usd"`
	IsError        bool                   `json:"is_error,omitempty" bson:"is_error,omitempty"`
	Subtype        string                 `json:"subtype,omitempty" bson:"subtype,omitempty"`
	APIErrorStatus json.RawMessage        `json:"api_error_status,omitempty" bson:"api_error_status,omitempty"`
	DurationMs     int64                  `json:"duration_ms" bson:"duration_ms"`
	DurationAPI    int64                  `json:"duration_api_ms" bson:"duration_api_ms"`
	NumTurns       int                    `json:"num_turns" bson:"num_turns"`
	Model          string                 `json:"model" bson:"model"`
	TokenUsage     *TokenUsage            `json:"token_usage,omitempty" bson:"token_usage,omitempty"`
	ModelUsage     map[string]*ModelUsage `json:"model_usage,omitempty" bson:"model_usage,omitempty"`
	MCPServers     []MCPServerStatus      `json:"mcp_servers,omitempty" bson:"mcp_servers,omitempty"`
	ClaudeVersion  string                 `json:"claude_version,omitempty" bson:"claude_version,omitempty"`

	// StructuredOutput is the raw structured result, when the CLI emits one.
	StructuredOutput json.RawMessage `json:"structured_output,omitempty" bson:"structured_output,omitempty"`
}
