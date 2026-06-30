package claudecli

import "testing"

func TestParseStreamLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantNil  bool
		wantType string
	}{
		{name: "empty line", line: "", wantNil: true},
		{name: "invalid json", line: "not json", wantNil: true},
		{
			name:     "assistant event (object message)",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}`,
			wantType: msgTypeAssistant,
		},
		{
			name:     "result event",
			line:     `{"type":"result","result":"Done","total_cost_usd":0.05,"num_turns":3,"session_id":"abc123"}`,
			wantType: "result",
		},
		{
			name:     "system init event",
			line:     `{"type":"system","subtype":"init","session_id":"abc"}`,
			wantType: "system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := ParseStreamLine([]byte(tt.line))
			if tt.wantNil {
				if event != nil {
					t.Errorf("expected nil, got %+v", event)
				}
				return
			}
			if event == nil {
				t.Fatal("expected non-nil event")
			}
			if event.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", event.Type, tt.wantType)
			}
		})
	}
}

func TestStreamEventIsResult(t *testing.T) {
	event := &StreamEvent{Type: "result"}
	if !event.IsResult() {
		t.Error("expected IsResult() = true")
	}
	event.Type = msgTypeAssistant
	if event.IsResult() {
		t.Error("expected IsResult() = false")
	}
}

func TestStreamEventIsAssistant(t *testing.T) {
	event := &StreamEvent{Type: msgTypeAssistant}
	if !event.IsAssistant() {
		t.Error("expected IsAssistant() = true")
	}
	event.Type = "system"
	if event.IsAssistant() {
		t.Error("expected IsAssistant() = false")
	}
}

func TestStreamEventCost(t *testing.T) {
	// Prefers total_cost_usd (current CLI).
	e := &StreamEvent{TotalCostUSD: 0.20, CostUSD: 0.05}
	if got := e.Cost(); got != 0.20 {
		t.Errorf("Cost() = %v, want 0.20", got)
	}
	// Falls back to legacy cost_usd.
	e = &StreamEvent{CostUSD: 0.07}
	if got := e.Cost(); got != 0.07 {
		t.Errorf("Cost() = %v, want 0.07", got)
	}
}

func TestStreamEventAssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]}}`
	e := ParseStreamLine([]byte(line))
	if e == nil {
		t.Fatal("nil event")
	}
	if got := e.AssistantText(); got != "part one\npart two" {
		t.Errorf("AssistantText() = %q", got)
	}
	// Non-assistant event yields "".
	r := &StreamEvent{Type: "result"}
	if got := r.AssistantText(); got != "" {
		t.Errorf("AssistantText() on result = %q, want empty", got)
	}
}

func TestExtractTextFromStream(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"init","session_id":"s"}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Step 1"}]}}`),
		[]byte(`{"type":"result","result":"All done!","total_cost_usd":0.10,"num_turns":5}`),
	}

	text, result := ExtractTextFromStream(lines)
	if text != "All done!" { // result text wins over assistant text
		t.Errorf("text = %q, want %q", text, "All done!")
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.Cost() != 0.10 {
		t.Errorf("Cost() = %v, want 0.10", result.Cost())
	}
	if result.NumTurns != 5 {
		t.Errorf("NumTurns = %d, want 5", result.NumTurns)
	}
}

func TestExtractTextFromStreamNoResult(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"World"}]}}`),
	}

	text, result := ExtractTextFromStream(lines)
	if text != "Hello\nWorld" {
		t.Errorf("text = %q, want %q", text, "Hello\nWorld")
	}
	if result != nil {
		t.Error("expected nil result")
	}
}
