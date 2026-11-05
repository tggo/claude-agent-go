package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/recording"
)

func TestCombine(t *testing.T) {
	a := Run{ToolCalls: []ToolCall{{Name: "Write"}}, Response: "a", SessionID: "s1",
		Metrics: Metrics{ToolCalls: 1, Inferences: 2, Turns: 2, InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CostUSD: 0.01, Duration: time.Second, ModelTime: 700 * time.Millisecond, ToolTime: 300 * time.Millisecond}}
	b := Run{ToolCalls: []ToolCall{{Name: "Bash"}, {Name: "approve"}}, Metrics: Metrics{ToolCalls: 2, CostUSD: 0.02, Duration: 2 * time.Second}}
	c := Combine(a, b)
	if len(c.ToolCalls) != 3 || c.ToolCalls[2].Name != "approve" {
		t.Fatalf("tools = %+v", c.ToolCalls)
	}
	if c.Response != "a" || c.SessionID != "s1" {
		t.Errorf("response/session = %q %q", c.Response, c.SessionID)
	}
	m := c.Metrics
	if m.ToolCalls != 3 || m.Inferences != 2 || m.Turns != 2 || m.InputTokens != 10 || m.OutputTokens != 5 || m.CacheReadTokens != 3 || m.Duration != 3*time.Second || m.ModelTime != 700*time.Millisecond || m.ToolTime != 300*time.Millisecond {
		t.Errorf("metrics = %+v", m)
	}
	if m.CostUSD < 0.0299 || m.CostUSD > 0.0301 {
		t.Errorf("cost = %v", m.CostUSD)
	}
	if got := Combine(); len(got.ToolCalls) != 0 {
		t.Error("empty combine")
	}
}

func writeRec(t *testing.T, path string, start time.Time, lines ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.NewWriter(f, recording.Header{StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		w.WriteLine([]byte(l))
	}
	w.Close()
}

func TestFromRecordingDir(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	tool := func(id, name string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","role":"assistant","content":[{"type":"tool_use","id":"t` + id + `","name":"` + name + `","input":{}}]}}`
	}
	// Named so lexical order is the reverse of start order.
	writeRec(t, filepath.Join(dir, "a.jsonl"), t0.Add(time.Minute), tool("2", "approve"),
		`{"type":"result","result":"second","total_cost_usd":0.02}`)
	writeRec(t, filepath.Join(dir, "b.jsonl"), t0, tool("1", "Write"),
		`{"type":"result","result":"first","total_cost_usd":0.01}`)
	os.WriteFile(filepath.Join(dir, "junk.jsonl"), []byte("not a recording\n"), 0o644)

	run, err := FromRecordingDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.ToolCalls) != 2 || run.ToolCalls[0].Name != "Write" || run.ToolCalls[1].Name != "approve" {
		t.Fatalf("order = %+v", run.ToolCalls)
	}
	if run.Response != "second" {
		t.Errorf("response = %q", run.Response)
	}
	if _, err := FromRecordingDir("[bad"); err == nil {
		t.Error("bad glob should error")
	}
}

// Real CLI stream-json: per-message usage lacks output tokens; the result
// event's usage holds the run totals and must win.
func TestResultUsageWins(t *testing.T) {
	run := FromStream([][]byte{
		[]byte(`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":2,"cache_read_input_tokens":100}}}`),
		[]byte(`{"type":"result","result":"hi","total_cost_usd":0.01,"usage":{"input_tokens":8,"output_tokens":1697,"cache_read_input_tokens":148059}}`),
	})
	if m := run.Metrics; m.InputTokens != 8 || m.OutputTokens != 1697 || m.CacheReadTokens != 148059 {
		t.Errorf("metrics = %+v", m)
	}
}
