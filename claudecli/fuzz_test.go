package claudecli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

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

// genStream builds a structurally valid stream-json run from a seed: init,
// turns of text + parallel tool_use blocks answered by tool_result events,
// optional sub-agent chatter, and a final result event.
func genStream(seed uint64) [][]byte {
	next := func() uint64 { // xorshift64*
		seed ^= seed >> 12
		seed ^= seed << 25
		seed ^= seed >> 27
		return seed * 2685821657736338717
	}
	if seed == 0 {
		seed = 88172645463325252
	}
	n := func(k int) int { return int(next() % uint64(k)) }
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	sid := fmt.Sprintf("sess-%d", seed%1000)
	add(`{"type":"system","subtype":"init","session_id":"` + sid + `"}`)
	add(`{"type":"user","message":{"role":"user","content":"go"}}`)
	id := 0
	for turn := n(5); turn >= 0; turn-- {
		var blocks, results []string
		var ids []string
		blocks = append(blocks, `{"type":"text","text":"thinking \"hard\"\nabout it"}`)
		for k := n(4); k > 0; k-- {
			id++
			tid := fmt.Sprintf("toolu_%d", id)
			ids = append(ids, tid)
			blocks = append(blocks, `{"type":"tool_use","id":"`+tid+`","name":"mcp__x__t`+fmt.Sprint(n(3))+`","input":{"n":`+fmt.Sprint(n(100))+`}}`)
			results = append(results, `{"type":"tool_result","tool_use_id":"`+tid+`","content":"r"}`)
		}
		add(`{"type":"assistant","session_id":"` + sid + `","message":{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}}`)
		if len(ids) > 0 && n(2) == 0 {
			add(`{"type":"assistant","parent_tool_use_id":"` + ids[0] + `","message":{"content":[{"type":"text","text":"sub"}]}}`)
		}
		if len(results) > 0 {
			add(`{"type":"user","message":{"role":"user","content":[` + strings.Join(results, ",") + `]}}`)
		}
	}
	add(`{"type":"result","subtype":"success","session_id":"` + sid + `","result":"done","total_cost_usd":0.01}`)
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = []byte(l)
	}
	return out
}

func parseAll(lines [][]byte) []StreamEvent {
	var evs []StreamEvent
	for _, l := range lines {
		if ev := ParseStreamLine(l); ev != nil {
			evs = append(evs, *ev)
		}
	}
	return evs
}

// FuzzCheckInvariants: a generated valid stream has no violations, and the
// same stream cut at any byte offset never panics any parser.
func FuzzCheckInvariants(f *testing.F) {
	for s := uint64(1); s <= 8; s++ {
		f.Add(s, uint16(s*37))
	}
	f.Fuzz(func(t *testing.T, seed uint64, cut uint16) {
		lines := genStream(seed)
		if vs := CheckInvariants(parseAll(lines)); len(vs) != 0 {
			t.Fatalf("valid stream has violations: %v", vs)
		}
		joined := bytes.Join(lines, []byte("\n"))
		joined = joined[:int(cut)%(len(joined)+1)]
		parts := bytes.Split(joined, []byte("\n"))
		_, _ = ExtractTextFromStream(parts)
		for _, v := range CheckInvariants(parseAll(parts)) {
			_ = v.String()
		}
	})
}

func TestGenStreamValidAndTruncated(t *testing.T) {
	for seed := uint64(0); seed < 300; seed++ {
		lines := genStream(seed)
		if vs := CheckInvariants(parseAll(lines)); len(vs) != 0 {
			t.Fatalf("seed %d: %v", seed, vs)
		}
		joined := bytes.Join(lines, []byte("\n"))
		for cut := 0; cut <= len(joined); cut += 7 {
			parts := bytes.Split(joined[:cut], []byte("\n"))
			_, _ = ExtractTextFromStream(parts)
			_ = CheckInvariants(parseAll(parts))
		}
	}
}
