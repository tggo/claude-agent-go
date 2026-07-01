package claudecli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// msgTypeAssistant is the Claude CLI message/event type for assistant turns.
const msgTypeAssistant = "assistant"

// ParseOutput parses Claude CLI JSON output and extracts execution metadata.
// The output can be either:
//   - A JSON array containing init, assistant messages, and a result message (streaming format)
//   - A single JSON object with type="result" (non-streaming format)
//
// Returns the result text (Claude's response) and execution metadata.
func ParseOutput(output []byte) (resultText string, metadata *ExecutionMetadata, err error) {
	if len(output) == 0 {
		return "", nil, fmt.Errorf("empty output")
	}

	// Try JSON array first (streaming format: [{init}, {assistant}, {result}])
	var messages []ClaudeOutputMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		// Try single JSON object (non-streaming format: {"type":"result",...})
		var singleMsg ClaudeOutputMessage
		if singleErr := json.Unmarshal(output, &singleMsg); singleErr != nil || singleMsg.Type == "" {
			// Detect common escaping issues for better error messages
			escapingIssue := DetectEscapingIssues(output)
			if escapingIssue != "" {
				return "", nil, fmt.Errorf("failed to parse JSON output (possible escaping issue: %s): %w", escapingIssue, err)
			}
			return "", nil, fmt.Errorf("failed to parse JSON output: %w", err)
		}
		messages = []ClaudeOutputMessage{singleMsg}
	}

	if len(messages) == 0 {
		return "", nil, fmt.Errorf("no messages in output")
	}

	metadata = &ExecutionMetadata{}

	// Process each message type
	for i := range messages {
		msg := &messages[i]

		switch msg.Type {
		case "system":
			if msg.Subtype == "init" {
				// Extract init data
				if msg.SessionID != "" {
					metadata.SessionID = msg.SessionID
				}
				if msg.Model != "" {
					metadata.Model = msg.Model
				}
				if len(msg.MCPServers) > 0 {
					metadata.MCPServers = msg.MCPServers
				}
				if msg.ClaudeVersion != "" {
					metadata.ClaudeVersion = msg.ClaudeVersion
				}
			}

		case msgTypeAssistant:
			// Extract text from assistant messages (useful for debugging)
			// The actual review content will be in the result message
			continue

		case "result":
			// Extract result data
			resultText = msg.Result

			// Override session ID from result if present (more reliable)
			if msg.SessionID != "" {
				metadata.SessionID = msg.SessionID
			}

			metadata.TotalCostUSD = msg.TotalCostUSD
			metadata.IsError = msg.IsError
			metadata.Subtype = msg.Subtype
			metadata.APIErrorStatus = msg.APIErrorStatus
			metadata.DurationMs = msg.DurationMs
			metadata.DurationAPI = msg.DurationAPI
			metadata.NumTurns = msg.NumTurns

			if msg.Usage != nil {
				metadata.TokenUsage = msg.Usage
			}

			if len(msg.ModelUsage) > 0 {
				metadata.ModelUsage = msg.ModelUsage
			}
			if len(msg.StructuredOutput) > 0 {
				metadata.StructuredOutput = msg.StructuredOutput
			}
		}
	}

	// Validate we got required data
	if resultText == "" {
		// Try to extract text from assistant messages as fallback
		resultText = ExtractTextFromAssistantMessages(messages)
	}

	if metadata.SessionID == "" {
		return resultText, metadata, fmt.Errorf("no session_id found in output")
	}

	return resultText, metadata, nil
}

// ExtractTextFromAssistantMessages extracts text content from assistant messages.
// Used as fallback when result message doesn't contain the text.
func ExtractTextFromAssistantMessages(messages []ClaudeOutputMessage) string {
	var texts []string

	for i := range messages {
		msg := &messages[i]
		if msg.Type != msgTypeAssistant || msg.Message == nil {
			continue
		}

		for _, block := range msg.Message.Content {
			if block.Type == "text" && block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
	}

	return strings.Join(texts, "\n")
}

// ExtractResultMessage finds and returns the result message from Claude output.
// Returns nil if no result message found.
func ExtractResultMessage(output []byte) (*ClaudeOutputMessage, error) {
	if len(output) == 0 {
		return nil, fmt.Errorf("empty output")
	}

	var messages []ClaudeOutputMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		return nil, fmt.Errorf("failed to parse JSON array: %w", err)
	}

	for i := range messages {
		if messages[i].Type == "result" {
			return &messages[i], nil
		}
	}

	return nil, fmt.Errorf("no result message found")
}

// ExtractInitMessage finds and returns the init message from Claude output.
// Returns nil if no init message found.
func ExtractInitMessage(output []byte) (*ClaudeOutputMessage, error) {
	if len(output) == 0 {
		return nil, fmt.Errorf("empty output")
	}

	var messages []ClaudeOutputMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		return nil, fmt.Errorf("failed to parse JSON array: %w", err)
	}

	for i := range messages {
		if messages[i].Type == "system" && messages[i].Subtype == "init" {
			return &messages[i], nil
		}
	}

	return nil, fmt.Errorf("no init message found")
}

// IsErrorResult checks if the Claude output indicates an error.
func IsErrorResult(output []byte) bool {
	result, err := ExtractResultMessage(output)
	if err != nil {
		return true // Unable to parse = error
	}
	return result.IsError
}

// GetSessionID extracts just the session ID from Claude output.
// Useful when you only need the session ID without full parsing.
func GetSessionID(output []byte) (string, error) {
	// Try result message first (most reliable)
	result, err := ExtractResultMessage(output)
	if err == nil && result.SessionID != "" {
		return result.SessionID, nil
	}

	// Fall back to init message
	init, err := ExtractInitMessage(output)
	if err == nil && init.SessionID != "" {
		return init.SessionID, nil
	}

	return "", fmt.Errorf("no session_id found in output")
}

// DetectEscapingIssues checks for common JSON escaping problems.
// Returns a description of the issue if detected, empty string otherwise.
func DetectEscapingIssues(output []byte) string {
	s := string(output)

	// Check for unescaped newlines in strings (common with code snippets)
	// Look for patterns like "result": "...\n..." without proper \n escaping
	if strings.Contains(s, "\n") && !strings.Contains(s, "\\n") {
		// Check if it's inside a string value
		inString := false
		for i := 0; i < len(s); i++ {
			if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
				inString = !inString
			}
			if inString && s[i] == '\n' {
				return "unescaped newline in string value"
			}
		}
	}

	// Check for unescaped quotes in string values
	quoteCount := strings.Count(s, "\"")
	escapedQuoteCount := strings.Count(s, "\\\"")
	// If quote count is odd (excluding escaped quotes), there's likely an unescaped quote issue
	if (quoteCount-escapedQuoteCount*2)%2 != 0 {
		return "mismatched or unescaped quotes"
	}

	// Check for truncated JSON (doesn't end with ] or })
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 0 {
		lastChar := trimmed[len(trimmed)-1]
		if lastChar != ']' && lastChar != '}' {
			return "truncated JSON output"
		}
	}

	// Check for control characters that should be escaped
	for i, c := range s {
		if c < 32 && c != '\n' && c != '\r' && c != '\t' {
			return fmt.Sprintf("unescaped control character at position %d", i)
		}
	}

	return ""
}
