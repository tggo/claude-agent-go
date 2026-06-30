package claudecli

import "testing"

func assistantMsg(blocks ...ContentBlock) ClaudeOutputMessage {
	return ClaudeOutputMessage{
		Type:    msgTypeAssistant,
		Message: &AssistantMessage{Role: "assistant", Content: blocks},
	}
}

func TestAllBlocks(t *testing.T) {
	msgs := []ClaudeOutputMessage{
		{Type: "system", Subtype: "init"},
		assistantMsg(
			ContentBlock{Type: BlockText, Text: "hi"},
			ContentBlock{Type: BlockToolUse, ID: "t1", Name: "Read", Input: []byte(`{"path":"x"}`)},
		),
		{Type: "result", Result: "done"},
		assistantMsg(ContentBlock{Type: BlockThinking, Thinking: "hmm"}),
		// assistant with nil message must be skipped without panic.
		{Type: msgTypeAssistant, Message: nil},
	}
	blocks := AllBlocks(msgs)
	if len(blocks) != 3 {
		t.Fatalf("AllBlocks len = %d, want 3", len(blocks))
	}
	if blocks[0].Type != BlockText || blocks[1].Type != BlockToolUse || blocks[2].Type != BlockThinking {
		t.Errorf("unexpected block order: %+v", blocks)
	}
}

func TestToolUses(t *testing.T) {
	msgs := []ClaudeOutputMessage{
		assistantMsg(
			ContentBlock{Type: BlockText, Text: "calling"},
			ContentBlock{Type: BlockToolUse, ID: "t1", Name: "Bash", Input: []byte(`{"cmd":"ls"}`)},
			ContentBlock{Type: BlockToolUse, ID: "t2", Name: "Read", Input: []byte(`{"path":"a"}`)},
		),
	}
	uses := ToolUses(msgs)
	if len(uses) != 2 {
		t.Fatalf("ToolUses len = %d, want 2", len(uses))
	}
	if uses[0].Name != "Bash" || uses[0].ID != "t1" || string(uses[0].Input) != `{"cmd":"ls"}` {
		t.Errorf("use[0] = %+v", uses[0])
	}
	if uses[1].Name != "Read" {
		t.Errorf("use[1].Name = %q", uses[1].Name)
	}
	if got := ToolUses(nil); got != nil {
		t.Errorf("ToolUses(nil) = %v, want nil", got)
	}
}

func TestThinkingText(t *testing.T) {
	msgs := []ClaudeOutputMessage{
		assistantMsg(
			ContentBlock{Type: BlockThinking, Thinking: "first"},
			ContentBlock{Type: BlockText, Text: "ignore"},
		),
		assistantMsg(ContentBlock{Type: BlockThinking, Thinking: "second"}),
	}
	if got := ThinkingText(msgs); got != "first\nsecond" {
		t.Errorf("ThinkingText = %q", got)
	}
	if got := ThinkingText(nil); got != "" {
		t.Errorf("ThinkingText(nil) = %q, want empty", got)
	}
}
