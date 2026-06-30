package claudecli

import "testing"

func TestPartialTextDelta(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}}`
	e := ParseStreamLine([]byte(line))
	if e == nil || !e.IsPartial() {
		t.Fatalf("expected partial event, got %+v", e)
	}
	if got := e.TextDelta(); got != "hello" {
		t.Errorf("TextDelta = %q", got)
	}
	if got := e.ThinkingDelta(); got != "" {
		t.Errorf("ThinkingDelta = %q, want empty", got)
	}
	p := e.Partial()
	if p == nil || p.Type != EventContentBlockDelta || p.Index != 1 {
		t.Errorf("Partial = %+v", p)
	}
}

func TestPartialThinkingDelta(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"pondering"}}}`
	e := ParseStreamLine([]byte(line))
	if got := e.ThinkingDelta(); got != "pondering" {
		t.Errorf("ThinkingDelta = %q", got)
	}
	if got := e.TextDelta(); got != "" {
		t.Errorf("TextDelta = %q, want empty", got)
	}
}

func TestPartialNonPartial(t *testing.T) {
	// A normal assistant event is not partial.
	e := &StreamEvent{Type: msgTypeAssistant}
	if e.IsPartial() {
		t.Error("assistant event should not be partial")
	}
	if e.Partial() != nil {
		t.Error("Partial() on non-partial should be nil")
	}
	if e.TextDelta() != "" || e.ThinkingDelta() != "" {
		t.Error("deltas on non-partial should be empty")
	}
}

func TestPartialContentBlockStart(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text"}}}`
	e := ParseStreamLine([]byte(line))
	p := e.Partial()
	if p == nil || p.Type != EventContentBlockStart || p.ContentBlock == nil || p.ContentBlock.Type != "text" {
		t.Errorf("content_block_start parse = %+v", p)
	}
	// No delta on a start event.
	if e.TextDelta() != "" {
		t.Error("start event should have no text delta")
	}
}

func TestPartialInvalidEvent(t *testing.T) {
	// stream_event with empty/invalid event payload.
	e := &StreamEvent{Type: "stream_event"}
	if e.Partial() != nil {
		t.Error("empty event payload should yield nil Partial")
	}
}
