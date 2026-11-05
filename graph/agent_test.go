package graph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	return path
}

func streamScript(result string, isError bool, cost string) string {
	e := "false"
	if isError {
		e = "true"
	}
	return `cat >/dev/null
cat <<'STREAM'
{"type":"system","subtype":"init","session_id":"sess-g"}
{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"` + result + `"}]}}
{"type":"result","subtype":"success","is_error":` + e + `,"session_id":"sess-g","result":"` + result + `","total_cost_usd":` + cost + `,"num_turns":1}
STREAM`
}

var quiet = runner.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

type labels struct {
	mu sync.Mutex
	ls []string
}

func (l *labels) ObserveRun(r runner.RunRecord) {
	l.mu.Lock()
	l.ls = append(l.ls, r.Label)
	l.mu.Unlock()
}

type astate struct {
	In, Out string
	Cost    float64
}

func agentGraph(t *testing.T, bin string, opts ...AgentOption) (*Runnable[astate], *labels) {
	t.Helper()
	lb := &labels{}
	r := runner.New(quiet, runner.WithBinary(bin), runner.WithObserver(lb))
	node := Agent(r,
		func(s *astate) runner.Input { return runner.Input{Prompt: "go: " + s.In} },
		func(s *astate, res *runner.Result) error {
			s.Out = res.ResultText
			s.Cost = res.TotalCostUSD
			return nil
		},
		opts...)
	g, err := New[astate]("ag").AddNode("coder", node).AddEdge(Start, "coder").AddEdge("coder", End).Compile()
	require.NoError(t, err)
	return g, lb
}

func TestAgentHappyPath(t *testing.T) {
	var events int
	var sawResult bool
	g, lb := agentGraph(t, fakeClaude(t, streamScript("done", false, "0.04")),
		WithProgress(func(ev claudecli.StreamEvent, n int) {
			events = n
			if ev.IsResult() {
				sawResult = true
			}
		}))
	out, rep, err := g.Run(context.Background(), astate{In: "x"})
	require.NoError(t, err)
	assert.Equal(t, "done", out.Out)
	assert.Equal(t, []string{"coder"}, lb.ls, "Input.Label should be the node name")
	assert.Equal(t, 3, events)
	assert.True(t, sawResult)
	assert.InDelta(t, 0.04, rep.Steps[0].CostUSD, 1e-9)
	assert.InDelta(t, 0.04, rep.TotalCostUSD, 1e-9)
}

func TestAgentExplicitLabelKept(t *testing.T) {
	lb := &labels{}
	r := runner.New(quiet, runner.WithBinary(fakeClaude(t, streamScript("ok", false, "0.01"))), runner.WithObserver(lb))
	g, err := New[astate]("ag").
		AddNode("n", Agent(r, func(*astate) runner.Input { return runner.Input{Prompt: "p", Label: "custom"} }, nil)).
		AddEdge(Start, "n").AddEdge("n", End).Compile()
	require.NoError(t, err)
	_, rep, err := g.Run(context.Background(), astate{})
	require.NoError(t, err)
	assert.Equal(t, []string{"custom"}, lb.ls)
	assert.InDelta(t, 0.01, rep.TotalCostUSD, 1e-9)
}

func TestAgentErrorResultFailsByDefault(t *testing.T) {
	g, _ := agentGraph(t, fakeClaude(t, streamScript("rate limited", true, "0.02")))
	out, rep, err := g.Run(context.Background(), astate{})
	require.ErrorIs(t, err, ErrAgentResult)
	assert.Contains(t, err.Error(), `agent "coder"`)
	assert.Contains(t, err.Error(), "rate limited")
	assert.Empty(t, out.Out, "apply must not run")
	assert.InDelta(t, 0.02, rep.TotalCostUSD, 1e-9, "cost of a failed run still counts")
}

func TestAgentAllowErrorResult(t *testing.T) {
	g, _ := agentGraph(t, fakeClaude(t, streamScript("rate limited", true, "0.02")), AllowErrorResult())
	out, _, err := g.Run(context.Background(), astate{})
	require.NoError(t, err)
	assert.Equal(t, "rate limited", out.Out)
}

func TestAgentProcessFailureWrapped(t *testing.T) {
	g, _ := agentGraph(t, fakeClaude(t, "cat >/dev/null; echo oops >&2; exit 3"))
	_, _, err := g.Run(context.Background(), astate{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `agent "coder"`)
}

func TestAgentMissingBinary(t *testing.T) {
	g, _ := agentGraph(t, "/nonexistent/claude")
	_, _, err := g.Run(context.Background(), astate{})
	assert.ErrorContains(t, err, `agent "coder"`)
}

func TestAgentApplyError(t *testing.T) {
	r := runner.New(quiet, runner.WithBinary(fakeClaude(t, streamScript("ok", false, "0.01"))))
	boom := errors.New("bad apply")
	g, err := New[astate]("ag").
		AddNode("n", Agent(r, func(*astate) runner.Input { return runner.Input{Prompt: "p"} },
			func(*astate, *runner.Result) error { return boom })).
		AddEdge(Start, "n").AddEdge("n", End).Compile()
	require.NoError(t, err)
	_, _, err = g.Run(context.Background(), astate{})
	assert.ErrorIs(t, err, boom)
	assert.ErrorContains(t, err, "apply")
}

func TestAgentWithRetry(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "n")
	script := `cat >/dev/null
n=$(cat ` + counter + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + counter + `
if [ "$n" -lt 2 ]; then
cat <<'STREAM'
{"type":"result","subtype":"error","is_error":true,"session_id":"s","result":"Overloaded","total_cost_usd":0.01}
STREAM
else
cat <<'STREAM'
{"type":"result","subtype":"success","session_id":"s","result":"second time","total_cost_usd":0.02}
STREAM
fi`
	g, _ := agentGraph(t, fakeClaude(t, script),
		WithRetry(runner.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	out, rep, err := g.Run(context.Background(), astate{})
	require.NoError(t, err)
	assert.Equal(t, "second time", out.Out)
	assert.InDelta(t, 0.03, rep.TotalCostUSD, 1e-9, "retry spend is summed")
}

func TestAgentInParallel(t *testing.T) {
	bin := fakeClaude(t, streamScript("pong", false, "0.05"))
	r := runner.New(quiet, runner.WithBinary(bin))
	mk := func() NodeFunc[astate] {
		return Agent(r, func(*astate) runner.Input { return runner.Input{Prompt: "p"} },
			func(s *astate, res *runner.Result) error { s.Out = res.ResultText; return nil })
	}
	g, err := New[astate]("p").
		AddNode("r1", mk()).AddNode("r2", mk()).
		AddParallel("reviews", []string{"r1", "r2"}, func(dst *astate, rs []astate) error {
			dst.Out = rs[0].Out + "+" + rs[1].Out
			return nil
		}).AddEdge(Start, "reviews").AddEdge("reviews", End).Compile()
	require.NoError(t, err)
	out, rep, err := g.Run(context.Background(), astate{})
	require.NoError(t, err)
	assert.Equal(t, "pong+pong", out.Out)
	assert.InDelta(t, 0.10, rep.TotalCostUSD, 1e-9)
}

func TestRunAgentAddsCostInCustomNode(t *testing.T) {
	lb := &labels{}
	r := runner.New(quiet, runner.WithBinary(fakeClaude(t, streamScript("ok", false, "0.07"))), runner.WithObserver(lb))
	g, err := New[astate]("ra").
		AddNode("custom", func(ctx context.Context, s *astate) error {
			res, err := RunAgent(ctx, r, runner.Input{Prompt: "p"})
			if err != nil {
				return err
			}
			s.Out = res.ResultText
			return Interrupt("confirm?")
		}).
		AddEdge(Start, "custom").AddEdge("custom", End).Compile()
	require.NoError(t, err)
	out, rep, err := g.Run(context.Background(), astate{})
	assert.ErrorIs(t, err, ErrInterrupt)
	assert.Equal(t, "ok", out.Out)
	assert.Equal(t, []string{"custom"}, lb.ls)
	assert.InDelta(t, 0.07, rep.TotalCostUSD, 1e-9)
}
