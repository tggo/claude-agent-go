package claudecli

// AllBlocks returns every content block from all assistant messages in order.
func AllBlocks(messages []ClaudeOutputMessage) []ContentBlock {
	var out []ContentBlock
	for i := range messages {
		m := &messages[i]
		if m.Type != msgTypeAssistant || m.Message == nil {
			continue
		}
		out = append(out, m.Message.Content...)
	}
	return out
}

// ToolUse is a flattened view of a tool_use block.
type ToolUse struct {
	ID    string
	Name  string
	Input []byte // raw JSON arguments
}

// ToolUses extracts all tool_use blocks across assistant messages.
func ToolUses(messages []ClaudeOutputMessage) []ToolUse {
	var out []ToolUse
	for _, b := range AllBlocks(messages) {
		if b.Type == BlockToolUse {
			out = append(out, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return out
}

// ThinkingText concatenates all thinking-block text across assistant messages.
func ThinkingText(messages []ClaudeOutputMessage) string {
	var s string
	for _, b := range AllBlocks(messages) {
		if b.Type == BlockThinking && b.Thinking != "" {
			if s != "" {
				s += "\n"
			}
			s += b.Thinking
		}
	}
	return s
}
