package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
)

func TestFromStream(t *testing.T) {
	run := FromStream(append(lines(fixtureStream), []byte("not json"), nil))
	assert.Equal(t, "sess-1", run.SessionID)
	assert.Equal(t, "The output is hi.", run.Response)

	require.Len(t, run.ToolCalls, 3)
	assert.Equal(t, "Bash", run.ToolCalls[0].Name)
	assert.JSONEq(t, `{"command":"echo hi"}`, string(run.ToolCalls[0].Args))
	assert.Equal(t, "hi", run.ToolCalls[0].Result)
	assert.False(t, run.ToolCalls[0].IsError)
	assert.Equal(t, "A\nB", run.ToolCalls[1].Result)
	assert.Equal(t, "boom", run.ToolCalls[2].Result)
	assert.True(t, run.ToolCalls[2].IsError)

	m := run.Metrics
	assert.Equal(t, 3, m.ToolCalls)
	assert.Equal(t, 3, m.Inferences, "msg_1 repeated per block counts once")
	assert.Equal(t, 3, m.Turns)
	assert.Equal(t, 18, m.InputTokens, "usage counted once per message id")
	assert.Equal(t, 31, m.OutputTokens, "latest usage per message id: 20+7+4")
	assert.Equal(t, 100, m.CacheReadTokens)
	assert.InDelta(t, 0.0123, m.CostUSD, 1e-9)
	assert.Equal(t, 4*time.Second, m.Duration)
	assert.Zero(t, m.ModelTime)
	assert.Zero(t, m.ToolTime)
}

func TestFromStreamEdgeCases(t *testing.T) {
	// No result event, no message ids, old cost key: falls back sensibly.
	run := FromStream(lines(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":1,"output_tokens":2}}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"b"},{"type":"text","text":"c"},{"type":"tool_use","name":"X","input":{}}],"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5}}}
{"type":"assistant","message":"legacy string message"}
{"type":"result","result":"","cost_usd":0.5}`))
	assert.Equal(t, "b\nc", run.Response)
	assert.Equal(t, 2, run.Metrics.Inferences)
	assert.Equal(t, 2, run.Metrics.Turns)
	assert.Equal(t, 4, run.Metrics.InputTokens)
	assert.Equal(t, 6, run.Metrics.OutputTokens)
	assert.Equal(t, 5, run.Metrics.CacheReadTokens)
	assert.InDelta(t, 0.5, run.Metrics.CostUSD, 1e-9)
	require.Len(t, run.ToolCalls, 1)

	assert.Equal(t, Run{}, FromStream(nil))
}

func buildRecording(t *testing.T, offsets []int) *recording.Recording {
	t.Helper()
	rec := &recording.Recording{Header: recording.Header{Prompt: "say hi using bash"}}
	for i, l := range lines(fixtureStream) {
		off := 0
		if i < len(offsets) {
			off = offsets[i]
		}
		rec.Lines = append(rec.Lines, recording.Line{Offset: time.Duration(off) * time.Millisecond, Raw: l})
	}
	return rec
}

func TestFromRecordingTimingSplit(t *testing.T) {
	// Lines: 0 init, 1 text, 2 tool_use tu_1, 3 result tu_1, 4 tool_use tu_2+tu_3,
	// 5 result tu_2, 6 result tu_3, 7..8 noise, 9 text, 10 result.
	rec := buildRecording(t, []int{0, 100, 200, 700, 800, 1000, 1200, 1200, 1200, 1500, 1600})
	run := FromRecording(rec)
	m := run.Metrics
	assert.Equal(t, 1600*time.Millisecond, m.Duration)
	// tu_1: 200..700 = 500ms. tu_2: 800..1000, tu_3: 800..1200 overlap -> 400ms.
	assert.Equal(t, 900*time.Millisecond, m.ToolTime)
	assert.Equal(t, 700*time.Millisecond, m.ModelTime)
	assert.Equal(t, "hi", run.ToolCalls[0].Result)
}

func TestFromRecordingWithoutOffsets(t *testing.T) {
	run := FromRecording(buildRecording(t, nil))
	assert.Equal(t, 4*time.Second, run.Metrics.Duration, "falls back to the result event")
	assert.Zero(t, run.Metrics.ToolTime)
	assert.Zero(t, run.Metrics.ModelTime)
}

func TestFromRecordingFile(t *testing.T) {
	_, err := FromRecordingFile(filepath.Join(t.TempDir(), "missing.jsonl"))
	assert.Error(t, err)

	path := filepath.Join(t.TempDir(), "r.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	w, err := recording.NewWriter(f, recording.Header{Prompt: "p"})
	require.NoError(t, err)
	for _, l := range lines(fixtureStream) {
		require.NoError(t, w.WriteLine(l))
	}
	require.NoError(t, w.Close())

	run, err := FromRecordingFile(path)
	require.NoError(t, err)
	assert.Equal(t, "The output is hi.", run.Response)
	assert.Len(t, run.ToolCalls, 3)
}

func TestCollectorManual(t *testing.T) {
	var zero Collector
	assert.Equal(t, Run{}, zero.Run())

	now := time.Unix(1000, 0)
	c := NewCollector()
	c.start = now
	c.now = func() time.Time { return now }
	step := func(ms int, line string) {
		now = time.Unix(1000, 0).Add(time.Duration(ms) * time.Millisecond)
		c.Progress(*claudecli.ParseStreamLine([]byte(line)), 0)
	}
	ls := lines(fixtureStream)
	step(10, string(ls[2]))
	step(110, string(ls[3]))
	run := c.Run()
	assert.Equal(t, 100*time.Millisecond, run.Metrics.ToolTime)
	assert.Equal(t, 110*time.Millisecond, run.Metrics.Duration)
	step(200, string(ls[9]))
	assert.Equal(t, "The output is hi.", c.Run().Response, "Run is repeatable mid-stream")
	assert.Equal(t, "", run.Response, "earlier snapshot is not mutated")

	// A zero-value Collector lazily starts its clock.
	zero.Progress(*claudecli.ParseStreamLine(ls[2]), 1)
	assert.Len(t, zero.Run().ToolCalls, 1)
}

func TestCollectorWithRunner(t *testing.T) {
	// Real subprocess, real clock: the tool takes a measurable 150ms.
	script := `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s9"}'
printf '%s\n' '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep"}}],"usage":{"input_tokens":2,"output_tokens":3}}}'
sleep 0.15
printf '%s\n' '{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}'
printf '%s\n' '{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"s9","result":"done","total_cost_usd":0.01,"num_turns":2}'`
	r := runner.New(runner.WithBinary(fakeClaude(t, script)))
	col := NewCollector()
	res, err := r.RunStream(context.Background(), runner.Input{Prompt: "go"}, col.Progress)
	require.NoError(t, err)
	assert.Equal(t, "done", res.ResultText)

	run := col.Run()
	assert.Equal(t, "s9", run.SessionID)
	assert.Equal(t, "done", run.Response)
	require.Len(t, run.ToolCalls, 1)
	assert.Equal(t, "ok", run.ToolCalls[0].Result)
	assert.GreaterOrEqual(t, run.Metrics.ToolTime, 140*time.Millisecond)
	assert.Greater(t, run.Metrics.Duration, run.Metrics.ToolTime)
	assert.Equal(t, run.Metrics.Duration, run.Metrics.ToolTime+run.Metrics.ModelTime)
	assert.Equal(t, 2, run.Metrics.Inferences)
}

func TestUnionLen(t *testing.T) {
	assert.Zero(t, unionLen(nil))
	ms := time.Millisecond
	assert.Equal(t, 10*ms, unionLen([]interval{{0, 10 * ms}}))
	assert.Equal(t, 25*ms, unionLen([]interval{{20 * ms, 30 * ms}, {0, 10 * ms}, {5 * ms, 15 * ms}, {25 * ms, 28 * ms}}))
}

func TestFlattenContent(t *testing.T) {
	assert.Equal(t, "", flattenContent(nil))
	assert.Equal(t, "x", flattenContent([]byte(`"x"`)))
	assert.Equal(t, "a\nb", flattenContent([]byte(`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`)))
	assert.Equal(t, `{"k":1}`, flattenContent([]byte(`{"k":1}`)))
}
