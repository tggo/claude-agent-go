package trace

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/claude-agent-go/recording"
)

// writeRec writes a realistic recording; ms values are line offsets.
func writeRec(t *testing.T, path, label string, started time.Time, withResult bool, xss string) {
	t.Helper()
	// Write the body with explicit offsets (the Writer stamps wall clock).
	lines := []struct {
		ms  int
		raw string
	}{
		{0, `{"type":"system","subtype":"init","session_id":"s1","model":"claude-haiku","tools":["Bash","Read"]}`},
		{100, `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"hmm think"}],"usage":{"input_tokens":10,"output_tokens":5}}}`},
		{110, `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Running it"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`},
		{600, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + xss + `"}]}}`},
		{650, `{"type":"rate_limit_event","info":1}`},
		{700, `{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file":"x"}}],"usage":{"input_tokens":20,"output_tokens":7,"cache_read_input_tokens":3}}}`},
		{900, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"boom"},{"type":"image"}],"is_error":true}]}}`},
		{910, `{"type":"user","message":{"content":"plain"}}`},
		{920, `{"type":"system","subtype":"hook_started"}`},
		{930, `{"type":"assistant","message":{"id":"m3","role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Edit","input":{}}]}}`},
		{940, `not json`},
	}
	if withResult {
		lines = append(lines, struct {
			ms  int
			raw string
		}{1000, `{"type":"result","subtype":"success","result":"All done","total_cost_usd":0.0123,"num_turns":3,"session_id":"s1","duration_ms":1000,"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":30,"cache_creation_input_tokens":4}}`})
	}
	var buf bytes.Buffer
	_, err := recording.NewWriter(&buf, recording.Header{Label: label, Prompt: "fix <b>the</b> bug", Model: "haiku", WorkDir: "/w", StartedAt: started})
	require.NoError(t, err)
	// stamp by hand: header already in buf
	for _, l := range lines {
		raw := l.raw
		if strings.HasPrefix(raw, "{") {
			raw = `{"_cag_t":` + strconv.Itoa(l.ms) + `,` + raw[1:]
		}
		buf.WriteString(raw + "\n")
	}
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func TestBuild(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	writeRec(t, p, "node-a", time.Now(), true, "hello")
	rec, err := recording.ReadFile(p)
	require.NoError(t, err)
	m := Build(rec)

	assert.Equal(t, "ok", m.Status)
	assert.InDelta(t, 0.0123, m.CostUSD, 1e-9)
	assert.Equal(t, 3, m.Turns)
	assert.Equal(t, time.Second, m.Wall)
	assert.Equal(t, "s1", m.SessionID)
	assert.Equal(t, 3, m.ToolCalls)
	assert.Equal(t, 3, m.Inferences) // m1 counted once
	assert.Equal(t, 1, m.Errors)
	assert.Equal(t, 100, m.InputTokens)
	assert.Equal(t, 50, m.OutputTokens)
	assert.Equal(t, 30, m.CacheRead)
	assert.Equal(t, 4, m.CacheWrite)
	assert.Equal(t, len(rec.Lines), m.Events)

	var tools []Step
	for _, s := range m.Steps {
		if s.Kind == KindToolUse {
			tools = append(tools, s)
		}
	}
	require.Len(t, tools, 3)
	assert.Equal(t, 490*time.Millisecond, tools[0].Duration)
	assert.True(t, tools[0].HasDuration())
	assert.Equal(t, 200*time.Millisecond, tools[1].Duration)
	assert.Equal(t, time.Duration(-1), tools[2].Duration)
	assert.False(t, tools[2].HasDuration())
	assert.InDelta(t, 11, tools[0].Left, 0.01)
	assert.InDelta(t, 49, tools[0].Width, 0.01)
	assert.InDelta(t, 7, tools[2].Width, 0.01) // runs to the end
	assert.Contains(t, tools[0].Text, "\"command\": \"ls\"")
}

func TestBuildNoResultFallbacks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	writeRec(t, p, "", time.Now(), false, "x")
	rec, _ := recording.ReadFile(p)
	m := Build(rec)
	assert.Equal(t, "incomplete", m.Status)
	assert.Equal(t, 30, m.InputTokens) // summed from assistant usage
	assert.Equal(t, 12, m.OutputTokens)
	assert.Equal(t, 3, m.CacheRead)
	assert.Equal(t, 930*time.Millisecond, m.Wall)
	assert.Equal(t, "run", m.Title())
	m.Source = "a.jsonl"
	assert.Equal(t, "a.jsonl", m.Title())
}

func TestBuildErrorResultAndStructured(t *testing.T) {
	var buf bytes.Buffer
	_, err := recording.NewWriter(&buf, recording.Header{})
	require.NoError(t, err)
	buf.WriteString(`{"_cag_t":0,"type":"result","subtype":"error_max_turns","is_error":true,"duration_ms":2500,"structured_output":{"a":1}}` + "\n")
	rec, err := recording.Read(&buf)
	require.NoError(t, err)
	m := Build(rec)
	assert.Equal(t, "error", m.Status)
	assert.Equal(t, 2500*time.Millisecond, m.Wall)
	assert.Contains(t, m.Steps[0].Text, `"a": 1`)
	assert.True(t, m.Steps[0].IsError)
}

func TestLongResultTruncated(t *testing.T) {
	long := strings.Repeat("é", previewLimit+50)
	p := filepath.Join(t.TempDir(), "a.jsonl")
	writeRec(t, p, "x", time.Now(), true, long)
	rec, _ := recording.ReadFile(p)
	m := Build(rec)
	var res Step
	for _, s := range m.Steps {
		if s.Kind == KindToolResult {
			res = s
			break
		}
	}
	assert.Equal(t, long, res.Full)
	assert.True(t, strings.HasSuffix(res.Text, "…"))
	assert.Equal(t, "Bash", res.Name)
}

func render(t *testing.T, ms []Model, graph string) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, ms, graph))
	return buf.String()
}

func TestRenderEscapesXSS(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	writeRec(t, p, `<img src=x onerror=alert(1)>`, time.Now(), true, `<script>alert(\"xss\")</script>`)
	ms, _, err := Load(p)
	require.NoError(t, err)
	out := render(t, ms, "")
	assert.NotContains(t, out, "<script>alert(")
	assert.NotContains(t, out, "<img src=x")
	assert.Contains(t, out, "&lt;script&gt;alert(")
	assert.Contains(t, out, "&lt;img src=x onerror")
	assert.Contains(t, out, "fix &lt;b&gt;the&lt;/b&gt; bug")
	assert.NotContains(t, out, "http://")
	assert.NotContains(t, out, "https://")
}

func TestRenderContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	writeRec(t, p, "node-a", time.Now(), true, "ok-output")
	ms, _, _ := Load(p)
	out := render(t, ms, "")
	for _, want := range []string{"node-a", "hmm think", "Running it", "All done", "$0.0123", "490ms", "rate limit event", "boom", "[image]", "claude-haiku", "no result", "step tool_result err", "left:11.00%"} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "<h2>Runs</h2>")
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeRec(t, filepath.Join(dir, "z.jsonl"), "first", now, true, "r")
	writeRec(t, filepath.Join(dir, "a.jsonl"), "second", now.Add(time.Minute), false, "r")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.jsonl"), []byte("nope\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "graph.mmd"), []byte("graph TD\n A-->B <x>"), 0o644))

	ms, graph, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, ms, 2)
	assert.Equal(t, "first", ms[0].Label)
	assert.Equal(t, "second", ms[1].Label)
	assert.Equal(t, "run-2", ms[1].ID)
	assert.Contains(t, graph, "A-->B")

	out := render(t, ms, graph)
	assert.Contains(t, out, "<h2>Runs</h2>")
	assert.Less(t, strings.Index(out, `href="#run-1"`), strings.Index(out, `href="#run-2"`))
	assert.Contains(t, out, "graph TD")
	assert.Contains(t, out, "A--&gt;B &lt;x&gt;")
	assert.Contains(t, out, "incomplete")
	assert.Contains(t, out, "Total")
}

func TestLoadErrors(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "missing.jsonl"))
	assert.Error(t, err)

	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	require.NoError(t, os.WriteFile(bad, []byte("{\"type\":\"x\"}\n"), 0o644))
	_, _, err = Load(bad)
	assert.ErrorIs(t, err, recording.ErrNotRecording)

	_, _, err = Load(t.TempDir())
	assert.ErrorContains(t, err, "no recordings")
}

func TestFormatters(t *testing.T) {
	assert.Equal(t, "—", fmtDur(-1))
	assert.Equal(t, "250ms", fmtDur(250*time.Millisecond))
	assert.Equal(t, "1.50s", fmtDur(1500*time.Millisecond))
	assert.Equal(t, "2m05s", fmtDur(125*time.Second))
	assert.Equal(t, "1,234,567", fmtNum(1234567))
	assert.Equal(t, "12", fmtNum(12))
	assert.Equal(t, "", prettyJSON(nil))
	assert.Equal(t, "{bad", prettyJSON([]byte("{bad")))
	assert.Equal(t, "", flattenContent(nil))
	assert.Equal(t, "5", flattenContent([]byte("5")))
	assert.Nil(t, userBlocks([]byte("{")))
}

func TestAttemptShown(t *testing.T) {
	dir := t.TempDir()
	for i, n := range []int{1, 2} {
		var buf bytes.Buffer
		_, err := recording.NewWriter(&buf, recording.Header{Label: "review", Attempt: n, StartedAt: time.Now().Add(time.Duration(i) * time.Second)})
		require.NoError(t, err)
		buf.WriteString(`{"_cag_t":5,"type":"result","subtype":"success","result":"r"}` + "\n")
		require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".jsonl"), buf.Bytes(), 0o644))
	}
	ms, _, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "review · attempt 2", ms[1].Title())
	out := render(t, ms, "")
	assert.Contains(t, out, "review · attempt 1")
	assert.Contains(t, out, "review · attempt 2")
	assert.Contains(t, out, "failed or truncated attempts included")
}
