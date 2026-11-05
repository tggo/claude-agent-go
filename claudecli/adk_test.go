package claudecli

import (
	"strings"
	"testing"
)

// ADK #7630, #3983: with --include-partial-messages the same text arrives as
// deltas and again in the assistant message; it must be counted once.
func TestADK7630NoDuplicatedStreamingText(t *testing.T) {
	delta := func(s string) string {
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + s + `"}}}`
	}
	asst := func(s string) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + s + `"}]}}`
	}
	result := `{"type":"result","subtype":"success","result":"Hello world"}`
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"deltas then assistant then result", []string{delta("Hello"), delta(" world"), asst("Hello world"), result}, "Hello world"},
		{"deltas then assistant, no result", []string{delta("Hello"), delta(" world"), asst("Hello world")}, "Hello world"},
		{"deltas only (cut mid-stream)", []string{delta("Hello"), delta(" wor")}, ""},
		{"two messages", []string{delta("a"), asst("a"), delta("b"), asst("b")}, "a\nb"},
		{"deltas after final assistant", []string{asst("Hello world"), delta("Hello"), delta(" world")}, "Hello world"},
		{"sub-agent text is not the answer", []string{asst("main"), `{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"sub"}]}}`}, "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines [][]byte
			for _, l := range tc.lines {
				lines = append(lines, []byte(l))
			}
			got, _ := ExtractTextFromStream(lines)
			if got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
			if strings.Count(got, "Hello") > 1 {
				t.Errorf("duplicated: %q", got)
			}
		})
	}
}

// ADK #3984: sub-agent events carry parent_tool_use_id.
func TestADK3984ParentToolUseID(t *testing.T) {
	sub := ParseStreamLine([]byte(`{"type":"assistant","parent_tool_use_id":"toolu_9","message":{"content":[{"type":"tool_use","id":"t","name":"Read","input":{}}]}}`))
	main := ParseStreamLine([]byte(`{"type":"assistant","parent_tool_use_id":null,"message":{"content":[]}}`))
	if sub.ParentToolUseID != "toolu_9" || !sub.IsSubagent() {
		t.Errorf("sub = %+v", sub)
	}
	if main.ParentToolUseID != "" || main.IsSubagent() {
		t.Errorf("main = %+v", main)
	}
	if m := sub.AssistantMessage(); m == nil || len(m.Content) != 1 || m.Content[0].Name != "Read" {
		t.Error("sub-agent tool use dropped")
	}
	if !strings.Contains(string(sub.Raw), "toolu_9") {
		t.Error("Raw lost the field")
	}
}
