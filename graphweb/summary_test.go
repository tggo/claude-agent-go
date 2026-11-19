package graphweb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

func TestSummarizeFinishedJournal(t *testing.T) {
	sum, err := Summarize(filepath.Join("testdata", "inspector.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, "launch-pricing", sum.Graph)
	assert.Equal(t, StatusDone, sum.Status)
	assert.False(t, sum.Started.IsZero())
	assert.False(t, sum.Ended.IsZero())
	assert.Positive(t, sum.DurationMS)
	assert.Positive(t, sum.CostUSD)
	assert.Positive(t, sum.Tokens)
	require.NotEmpty(t, sum.Nodes)
	assert.Equal(t, "orchestrator", sum.Nodes[0].Name)
	assert.Equal(t, StatusDone, sum.Nodes[0].Status)
	assert.InDelta(t, 0.12, sum.Nodes[0].CostUSD, 1e-9)
	assert.EqualValues(t, 3588+512, sum.Nodes[0].Tokens)
	assert.Equal(t, 1, sum.Nodes[0].Attempts)
	assert.EqualValues(t, 6500, sum.Nodes[0].DurationMS)

	var back RunSummary
	b, err := json.Marshal(sum)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, sum.Graph, back.Graph)
	assert.Contains(t, string(b), `"duration_ms"`)
	assert.Contains(t, string(b), `"nodes":[{"name":"orchestrator"`)
}

// A directory with journal.jsonl works like the file.
func TestSummarizeDirectory(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", "chain16.jsonl"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "journal.jsonl"), b, 0o600))
	sum, err := Summarize(dir)
	require.NoError(t, err)
	assert.Equal(t, "video-pipeline", sum.Graph)
	assert.Equal(t, StatusDone, sum.Status)
}

func firstLines(t *testing.T, name string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return strings.Join(strings.SplitAfter(string(b), "\n")[:n], "")
}

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "j.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// A run that is still going, or that crashed, is running; its last line may be cut.
func TestSummarizePartialJournal(t *testing.T) {
	body := firstLines(t, "chain16.jsonl", 8)
	sum, err := Summarize(writeTmp(t, body))
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, sum.Status)
	assert.True(t, sum.Ended.IsZero())
	assert.False(t, sum.LastEvent.IsZero())
	assert.NotEmpty(t, sum.Nodes)
	assert.NotEmpty(t, sum.Graph)

	cut, err := Summarize(writeTmp(t, body+`{"v":1,"run":"run-aaaa111122223333","seq":9,"ts":"2026-11-05T10:0`))
	require.NoError(t, err, "a half-written last line is ignored")
	assert.Equal(t, sum, cut)

	// the node that was started and not ended is running
	last := sum.Nodes[len(sum.Nodes)-1]
	assert.Contains(t, []string{StatusRunning, StatusDone}, last.Status)
}

func TestSummarizeUnknownAndErrors(t *testing.T) {
	sum, err := Summarize(writeTmp(t, ""))
	require.NoError(t, err)
	assert.Equal(t, StatusUnknown, sum.Status)
	assert.NotNil(t, sum.Nodes)

	// cut at the front: events without a graph_start
	tail := firstLines(t, "chain16.jsonl", 8)
	tail = tail[strings.Index(tail, "\n")+1:]
	sum, err = Summarize(writeTmp(t, tail))
	require.NoError(t, err)
	assert.Equal(t, StatusUnknown, sum.Status)

	_, err = Summarize(filepath.Join(t.TempDir(), "missing.jsonl"))
	assert.Error(t, err)
	_, err = Summarize(writeTmp(t, "{not json}\n"+firstLines(t, "chain16.jsonl", 3)))
	assert.Error(t, err, "garbage before the last line is an error")
	_, err = Summarize(writeTmp(t, `{"x":1}`+"\n"))
	assert.Error(t, err, "a complete line that is not an event is an error")
}

// Live journal written by the server: failed run, retries, interrupt.
func TestSummarizeLiveJournals(t *testing.T) {
	ctx := context.Background()
	jp := filepath.Join(t.TempDir(), "run.jsonl")
	s := newSrv(t, WithJournal(jp))
	g := graph.New[st]("flaky").
		AddNode("a", graph.Func(func(x *st) error { x.N++; return nil })).
		AddNode("b", graph.Func(func(*st) error { return errors.New("boom") })).
		AddEdge(graph.Start, "a").AddEdge("a", "b").AddEdge("b", graph.End)
	r, err := g.Compile(graph.WithObserver(s))
	require.NoError(t, err)
	_, _, err = r.Run(ctx, st{})
	require.Error(t, err)
	sum, err := Summarize(jp)
	require.NoError(t, err)
	assert.Equal(t, "flaky", sum.Graph)
	assert.Equal(t, StatusFailed, sum.Status)
	assert.Contains(t, sum.Error, "boom")
	require.Len(t, sum.Nodes, 2)
	assert.Equal(t, StatusDone, sum.Nodes[0].Status)
	assert.Equal(t, StatusFailed, sum.Nodes[1].Status)
	assert.Contains(t, sum.Nodes[1].Error, "boom")
	assert.Equal(t, 1, sum.Nodes[1].Attempts)

	// a looping node adds its visits up
	jp2 := filepath.Join(t.TempDir(), "loop.jsonl")
	s2 := newSrv(t, WithJournal(jp2))
	_, _, err = loopRun(t, s2).Run(ctx, st{})
	require.NoError(t, err)
	sum, err = Summarize(jp2)
	require.NoError(t, err)
	assert.Equal(t, StatusDone, sum.Status)
	require.Len(t, sum.Nodes, 2)
	assert.Equal(t, "inc", sum.Nodes[0].Name)
	assert.Equal(t, 3, sum.Nodes[0].Attempts)
	assert.Equal(t, 1, sum.Nodes[1].Attempts)
}

func TestSummarizeWaitingOnInterrupt(t *testing.T) {
	var b strings.Builder
	for _, l := range []string{
		`{"v":1,"run":"r1","seq":1,"ts":"2026-11-05T10:00:00Z","type":"graph_start","topology":{"name":"ask","entry":"__start__","nodes":[],"edges":[]}}`,
		`{"v":1,"run":"r1","seq":2,"ts":"2026-11-05T10:00:01Z","type":"node_start","node":"q","step":1}`,
		`{"v":1,"run":"r1","seq":3,"ts":"2026-11-05T10:00:02Z","type":"node_end","node":"q","step":1,"duration_ms":1000,"paused":true}`,
		`{"v":1,"run":"r1","seq":4,"ts":"2026-11-05T10:00:02Z","type":"interrupt","node":"q","step":1,"question":"ok?"}`,
	} {
		b.WriteString(l + "\n")
	}
	sum, err := Summarize(writeTmp(t, b.String()))
	require.NoError(t, err)
	assert.Equal(t, StatusWaiting, sum.Status)
	assert.Equal(t, StatusWaiting, sum.Nodes[0].Status)
	assert.EqualValues(t, 2000, sum.DurationMS, "an open run lasts to its last event")
	// the answer resumes it
	b.WriteString(`{"v":1,"run":"r1","seq":5,"ts":"2026-11-05T10:00:05Z","type":"resume","node":"q","step":1,"answer":"yes"}` + "\n")
	sum, err = Summarize(writeTmp(t, b.String()))
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, sum.Status)
}
