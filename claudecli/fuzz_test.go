package claudecli

import "testing"

// Fuzz targets assert the parsers never panic on arbitrary/ malformed input and
// that their accessors are safe to call on whatever they return — the CLI's JSON
// shape drifts between versions, so robustness matters more than strictness.
//
// Run the seed corpus with `go test`; fuzz with:
//   go test -run xxx -fuzz FuzzParseStreamLine ./claudecli

var streamSeeds = []string{
	``,
	`not json`,
	`{`,
	`{"type":"result","total_cost_usd":0.1,"is_error":true}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	`{"type":"rate_limit_event","status":"allowed"}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}`,
	`{"type":"system","subtype":"init","session_id":"s"}`,
	"{\"type\":\"result\",\"result\":\"line\nbreak\"}",
}

func FuzzParseStreamLine(f *testing.F) {
	for _, s := range streamSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		ev := ParseStreamLine(line)
		if ev == nil {
			return
		}
		// All accessors must be panic-safe on anything that parsed.
		_ = ev.IsResult()
		_ = ev.IsAssistant()
		_ = ev.IsRateLimit()
		_ = ev.IsPartial()
		_ = ev.Cost()
		_ = ev.AssistantText()
		_ = ev.ThinkingDelta()
		_ = ev.TextDelta()
		_ = ev.Partial()
		_ = ev.AssistantMessage()
	})
}

func FuzzParseOutput(f *testing.F) {
	seeds := []string{
		``,
		`[]`,
		`{}`,
		`[{"type":"system","subtype":"init","session_id":"s"},{"type":"result","session_id":"s","result":"ok","total_cost_usd":0.02}]`,
		`{"type":"result","session_id":"s","result":"ok"}`,
		`[{"type":"result"}]`,
		`[{"type":"result","structured_output":{"a":1}}]`,
		`truncated [{"type":`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		text, meta, err := ParseOutput(data)
		if err == nil && meta == nil {
			t.Fatal("nil metadata on success")
		}
		_ = text
	})
}

func FuzzExtractJSON(f *testing.F) {
	for _, s := range []string{``, `[1,2]`, `noise [1,2] noise`, `{"a":1}`, `[[[`, `text {"k":"v"} tail`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ExtractJSONFromText(s)
		_, _ = ExtractJSONObjectFromText(s)
		_ = DetectEscapingIssues([]byte(s))
	})
}
