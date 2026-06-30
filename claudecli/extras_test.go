package claudecli

import "testing"

func TestStructuredOutputParsed(t *testing.T) {
	out := `[{"type":"system","subtype":"init","session_id":"s"},` +
		`{"type":"result","session_id":"s","result":"done","structured_output":{"score":7,"ok":true}}]`
	_, meta, err := ParseOutput([]byte(out))
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if len(meta.StructuredOutput) == 0 {
		t.Fatal("StructuredOutput not captured")
	}
	if string(meta.StructuredOutput) != `{"score":7,"ok":true}` {
		t.Errorf("StructuredOutput = %s", meta.StructuredOutput)
	}
}

func TestIsRateLimitAndRaw(t *testing.T) {
	line := `{"type":"rate_limit_event","status":"allowed","resetsAt":123}`
	e := ParseStreamLine([]byte(line))
	if e == nil || !e.IsRateLimit() {
		t.Fatalf("expected rate_limit_event, got %+v", e)
	}
	if string(e.Raw) != line {
		t.Errorf("Raw not set correctly: %s", e.Raw)
	}
	// non rate-limit
	r := &StreamEvent{Type: "result"}
	if r.IsRateLimit() {
		t.Error("result should not be rate limit")
	}
}
