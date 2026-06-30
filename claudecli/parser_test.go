package claudecli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sample Claude CLI JSON output for testing.
var sampleClaudeOutput = []byte(`[
	{
		"type": "system",
		"subtype": "init",
		"cwd": "/tmp/test-repo",
		"session_id": "abc123-def456-ghi789",
		"model": "claude-sonnet-4-20250514",
		"tools": ["Bash", "Read", "Edit"],
		"mcp_servers": [
			{"name": "kb", "status": "connected"},
			{"name": "playwright", "status": "disabled"}
		],
		"permissionMode": "default",
		"claude_code_version": "2.1.3"
	},
	{
		"type": "assistant",
		"message": {
			"id": "msg_123",
			"model": "claude-sonnet-4-20250514",
			"role": "assistant",
			"content": [
				{"type": "text", "text": "I found some issues in the code."}
			],
			"usage": {
				"input_tokens": 1000,
				"output_tokens": 200
			}
		},
		"session_id": "abc123-def456-ghi789"
	},
	{
		"type": "result",
		"subtype": "success",
		"is_error": false,
		"duration_ms": 5432,
		"duration_api_ms": 8765,
		"num_turns": 1,
		"result": "[{\"file_path\":\"main.go\",\"line\":42,\"category\":\"bug\",\"severity\":\"high\",\"title\":\"Nil pointer\",\"body\":\"Potential nil pointer dereference\"}]",
		"session_id": "abc123-def456-ghi789",
		"total_cost_usd": 0.0234,
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 200,
			"cache_creation_input_tokens": 500,
			"cache_read_input_tokens": 300
		},
		"modelUsage": {
			"claude-sonnet-4-20250514": {
				"inputTokens": 800,
				"outputTokens": 180,
				"cacheReadInputTokens": 300,
				"cacheCreationInputTokens": 500,
				"costUSD": 0.0200,
				"contextWindow": 200000,
				"maxOutputTokens": 64000
			},
			"claude-haiku-4-5-20251001": {
				"inputTokens": 200,
				"outputTokens": 20,
				"costUSD": 0.0034
			}
		}
	}
]`)

func TestParseOutput_ValidOutput(t *testing.T) {
	resultText, metadata, err := ParseOutput(sampleClaudeOutput)

	require.NoError(t, err)
	assert.NotEmpty(t, resultText)
	assert.Contains(t, resultText, "main.go")

	// Check metadata
	require.NotNil(t, metadata)
	assert.Equal(t, "abc123-def456-ghi789", metadata.SessionID)
	assert.Equal(t, "claude-sonnet-4-20250514", metadata.Model)
	assert.Equal(t, "2.1.3", metadata.ClaudeVersion)
	assert.InDelta(t, 0.0234, metadata.TotalCostUSD, 0.0001)
	assert.Equal(t, int64(5432), metadata.DurationMs)
	assert.Equal(t, int64(8765), metadata.DurationAPI)
	assert.Equal(t, 1, metadata.NumTurns)

	// Check token usage
	require.NotNil(t, metadata.TokenUsage)
	assert.Equal(t, 1000, metadata.TokenUsage.InputTokens)
	assert.Equal(t, 200, metadata.TokenUsage.OutputTokens)
	assert.Equal(t, 500, metadata.TokenUsage.CacheCreationInputTokens)
	assert.Equal(t, 300, metadata.TokenUsage.CacheReadInputTokens)

	// Check model usage
	require.Len(t, metadata.ModelUsage, 2)
	sonnet := metadata.ModelUsage["claude-sonnet-4-20250514"]
	require.NotNil(t, sonnet)
	assert.Equal(t, 800, sonnet.InputTokens)
	assert.Equal(t, 180, sonnet.OutputTokens)
	assert.InDelta(t, 0.0200, sonnet.CostUSD, 0.0001)

	// Check MCP servers
	require.Len(t, metadata.MCPServers, 2)
	assert.Equal(t, "kb", metadata.MCPServers[0].Name)
	assert.Equal(t, "connected", metadata.MCPServers[0].Status)
}

func TestParseOutput_EmptyOutput(t *testing.T) {
	_, _, err := ParseOutput([]byte{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty output")
}

func TestParseOutput_InvalidJSON(t *testing.T) {
	_, _, err := ParseOutput([]byte("not json"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse JSON")
}

func TestParseOutput_EmptyArray(t *testing.T) {
	_, _, err := ParseOutput([]byte("[]"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no messages")
}

func TestParseOutput_NoSessionID(t *testing.T) {
	output := []byte(`[
		{"type": "result", "result": "test", "total_cost_usd": 0.01}
	]`)

	_, _, err := ParseOutput(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no session_id")
}

func TestParseOutput_OnlyInitMessage(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session-123",
			"model": "claude-sonnet-4"
		}
	]`)

	resultText, metadata, err := ParseOutput(output)

	// Should succeed but with empty result
	require.NoError(t, err)
	assert.Empty(t, resultText)
	assert.Equal(t, "test-session-123", metadata.SessionID)
	assert.Equal(t, "claude-sonnet-4", metadata.Model)
}

func TestParseOutput_ResultOverridesInitSessionID(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "init-session"
		},
		{
			"type": "result",
			"session_id": "result-session",
			"result": "test output"
		}
	]`)

	_, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	// Result session ID should take precedence
	assert.Equal(t, "result-session", metadata.SessionID)
}

func TestParseOutput_ErrorResult(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "result",
			"is_error": true,
			"session_id": "test-session",
			"result": "Error: something went wrong"
		}
	]`)

	resultText, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Contains(t, resultText, "Error:")
	assert.Equal(t, "test-session", metadata.SessionID)
}

func TestParseOutput_MultipleAssistantMessages(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "assistant",
			"message": {
				"content": [{"type": "text", "text": "First message"}]
			}
		},
		{
			"type": "assistant",
			"message": {
				"content": [{"type": "text", "text": "Second message"}]
			}
		},
		{
			"type": "result",
			"session_id": "test-session",
			"result": "Final result"
		}
	]`)

	resultText, _, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Equal(t, "Final result", resultText)
}

func TestParseOutput_FallbackToAssistantMessages(t *testing.T) {
	// When result has no text, fall back to assistant messages
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "assistant",
			"message": {
				"content": [
					{"type": "text", "text": "First part"},
					{"type": "text", "text": "Second part"}
				]
			}
		},
		{
			"type": "result",
			"session_id": "test-session",
			"result": ""
		}
	]`)

	resultText, _, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Contains(t, resultText, "First part")
	assert.Contains(t, resultText, "Second part")
}

func TestExtractResultMessage_Valid(t *testing.T) {
	result, err := ExtractResultMessage(sampleClaudeOutput)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "result", result.Type)
	assert.False(t, result.IsError)
	assert.Equal(t, "abc123-def456-ghi789", result.SessionID)
}

func TestExtractResultMessage_NoResult(t *testing.T) {
	output := []byte(`[{"type": "system", "subtype": "init"}]`)

	_, err := ExtractResultMessage(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no result message")
}

func TestExtractResultMessage_Empty(t *testing.T) {
	_, err := ExtractResultMessage([]byte{})
	assert.Error(t, err)
}

func TestExtractInitMessage_Valid(t *testing.T) {
	init, err := ExtractInitMessage(sampleClaudeOutput)

	require.NoError(t, err)
	require.NotNil(t, init)
	assert.Equal(t, "system", init.Type)
	assert.Equal(t, "init", init.Subtype)
	assert.Equal(t, "abc123-def456-ghi789", init.SessionID)
}

func TestExtractInitMessage_NoInit(t *testing.T) {
	output := []byte(`[{"type": "result", "session_id": "test"}]`)

	_, err := ExtractInitMessage(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no init message")
}

func TestExtractInitMessage_InvalidJSON(t *testing.T) {
	_, err := ExtractInitMessage([]byte("invalid json"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse JSON")
}

func TestExtractInitMessage_Empty(t *testing.T) {
	_, err := ExtractInitMessage([]byte{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty output")
}

func TestIsErrorResult_Success(t *testing.T) {
	assert.False(t, IsErrorResult(sampleClaudeOutput))
}

func TestIsErrorResult_Error(t *testing.T) {
	output := []byte(`[{"type": "result", "is_error": true}]`)
	assert.True(t, IsErrorResult(output))
}

func TestIsErrorResult_InvalidJSON(t *testing.T) {
	assert.True(t, IsErrorResult([]byte("invalid")))
}

func TestGetSessionID_FromResult(t *testing.T) {
	sessionID, err := GetSessionID(sampleClaudeOutput)

	require.NoError(t, err)
	assert.Equal(t, "abc123-def456-ghi789", sessionID)
}

func TestGetSessionID_FromInit(t *testing.T) {
	output := []byte(`[
		{"type": "system", "subtype": "init", "session_id": "init-only-session"}
	]`)

	sessionID, err := GetSessionID(output)

	require.NoError(t, err)
	assert.Equal(t, "init-only-session", sessionID)
}

func TestGetSessionID_NotFound(t *testing.T) {
	output := []byte(`[{"type": "system", "subtype": "init"}]`)

	_, err := GetSessionID(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no session_id")
}

func TestParseOutput_ZeroCost(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "result",
			"session_id": "test-session",
			"result": "test",
			"total_cost_usd": 0,
			"duration_ms": 100
		}
	]`)

	_, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Equal(t, float64(0), metadata.TotalCostUSD)
	assert.Equal(t, int64(100), metadata.DurationMs)
}

func TestParseOutput_NoModelUsage(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "result",
			"session_id": "test-session",
			"result": "test"
		}
	]`)

	_, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Nil(t, metadata.ModelUsage)
	assert.Nil(t, metadata.TokenUsage)
}

func TestParseOutput_WithToolUseContent(t *testing.T) {
	output := []byte(`[
		{
			"type": "system",
			"subtype": "init",
			"session_id": "test-session"
		},
		{
			"type": "assistant",
			"message": {
				"content": [
					{"type": "tool_use", "name": "Read"},
					{"type": "text", "text": "Reading file..."}
				]
			}
		},
		{
			"type": "result",
			"session_id": "test-session",
			"result": ""
		}
	]`)

	resultText, _, err := ParseOutput(output)

	require.NoError(t, err)
	// Should only extract text, not tool_use
	assert.Equal(t, "Reading file...", resultText)
}

// TestParseOutput_SingleResultObject tests parsing when Claude CLI returns
// a single JSON object instead of an array. This happens with newer CLI versions
// using --output-format json in non-streaming mode.
func TestParseOutput_SingleResultObject(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","is_error":false,` +
		`"duration_ms":1845,"duration_api_ms":1818,"num_turns":1,"result":"[]",` +
		`"stop_reason":"end_turn","session_id":"ba1eff83-58f5-4d64-a081-c972bd80df8f",` +
		`"total_cost_usd":0.0395871,` +
		`"usage":{"input_tokens":2,"cache_creation_input_tokens":10034,` +
		`"cache_read_input_tokens":6312,"output_tokens":4},` +
		`"modelUsage":{"claude-sonnet-4-6":{"inputTokens":2,"outputTokens":4,` +
		`"cacheReadInputTokens":6312,"cacheCreationInputTokens":10034,` +
		`"costUSD":0.0395871,"contextWindow":200000,"maxOutputTokens":32000}}}`)

	resultText, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Equal(t, "[]", resultText)

	require.NotNil(t, metadata)
	assert.Equal(t, "ba1eff83-58f5-4d64-a081-c972bd80df8f", metadata.SessionID)
	assert.InDelta(t, 0.0395871, metadata.TotalCostUSD, 0.0001)
	assert.Equal(t, int64(1845), metadata.DurationMs)
	assert.Equal(t, int64(1818), metadata.DurationAPI)
	assert.Equal(t, 1, metadata.NumTurns)

	require.Len(t, metadata.ModelUsage, 1)
	sonnet := metadata.ModelUsage["claude-sonnet-4-6"]
	require.NotNil(t, sonnet)
	assert.Equal(t, 2, sonnet.InputTokens)
	assert.Equal(t, 4, sonnet.OutputTokens)
	assert.InDelta(t, 0.0395871, sonnet.CostUSD, 0.0001)
}

// TestParseOutput_SingleResultObjectWithComments tests single object format
// when Claude returns review comments.
func TestParseOutput_SingleResultObjectWithComments(t *testing.T) {
	output := []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":5000,"duration_api_ms":4800,"num_turns":1,"result":"[{\"file_path\":\"main.go\",\"line\":10,\"title\":\"Bug\",\"body\":\"Issue here\"}]","session_id":"test-single-session","total_cost_usd":0.05}`)

	resultText, metadata, err := ParseOutput(output)

	require.NoError(t, err)
	assert.Contains(t, resultText, "main.go")
	assert.Equal(t, "test-single-session", metadata.SessionID)
	assert.InDelta(t, 0.05, metadata.TotalCostUSD, 0.001)
}

func TestExtractTextFromAssistantMessages_Empty(t *testing.T) {
	messages := []ClaudeOutputMessage{
		{Type: "system"},
		{Type: "result"},
	}

	text := ExtractTextFromAssistantMessages(messages)
	assert.Empty(t, text)
}

func TestExtractTextFromAssistantMessages_NilMessage(t *testing.T) {
	messages := []ClaudeOutputMessage{
		{Type: msgTypeAssistant, Message: nil},
	}

	text := ExtractTextFromAssistantMessages(messages)
	assert.Empty(t, text)
}

func TestExtractTextFromAssistantMessages_EmptyContent(t *testing.T) {
	messages := []ClaudeOutputMessage{
		{
			Type: msgTypeAssistant,
			Message: &AssistantMessage{
				Content: []ContentBlock{},
			},
		},
	}

	text := ExtractTextFromAssistantMessages(messages)
	assert.Empty(t, text)
}
