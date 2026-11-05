package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/runner"
)

type st struct {
	N    int
	Out  string
	Key  string
	Ans  string
	Seen []string
}

func compile(t *testing.T, g *graph.Graph[st], opts ...graph.CompileOption) *graph.Runnable[st] {
	t.Helper()
	r, err := g.Compile(opts...)
	require.NoError(t, err)
	return r
}

func newEnv(t *testing.T, r *graph.Runnable[st], opts ...Option) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	ts.SetLogger(slogTemporal{})
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "wf-test"})
	Register(env, r, opts...)
	return env
}

type slogTemporal struct{}

func (slogTemporal) Debug(string, ...any) {}
func (slogTemporal) Info(string, ...any)  {}
func (slogTemporal) Warn(string, ...any)  {}
func (slogTemporal) Error(string, ...any) {}

func fast(o ...Option) []Option {
	return append([]Option{WithRetryPolicy(sdktemporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: time.Millisecond, BackoffCoefficient: 1})}, o...)
}

func execute(t *testing.T, env *testsuite.TestWorkflowEnvironment, r *graph.Runnable[st], init st) (st, *Result, error) {
	t.Helper()
	b, _ := jsonMarshal(init)
	env.ExecuteWorkflow(WorkflowName(r), Input{State: b})
	require.True(t, env.IsWorkflowCompleted())
	if err := env.GetWorkflowError(); err != nil {
		return st{}, nil, err
	}
	var res Result
	require.NoError(t, env.GetWorkflowResult(&res))
	var out st
	require.NoError(t, res.Decode(&out))
	return out, &res, nil
}

func linear(t *testing.T) *graph.Runnable[st] {
	return compile(t, graph.New[st]("lin").
		AddNode("a", func(ctx context.Context, s *st) error {
			s.N = 1
			s.Key = graph.IdempotencyKey(ctx)
			graph.AddCost(ctx, 0.5)
			return nil
		}).
		AddNode("b", func(ctx context.Context, s *st) error { s.N *= 10; graph.AddCost(ctx, 0.25); return nil }).
		AddEdge(graph.Start, "a").AddEdge("a", "b").AddEdge("b", graph.End))
}

func TestNames(t *testing.T) {
	r := linear(t)
	assert.Equal(t, "graph.lin", WorkflowName(r))
	assert.Equal(t, "graph.lin.node", ActivityName(r))
	assert.Equal(t, "graph-lin", TaskQueue(r))
}

func TestLinearGraphReportAndIdempotencyKey(t *testing.T) {
	r := linear(t)
	env := newEnv(t, r, fast()...)
	out, res, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.Equal(t, 10, out.N)
	assert.Equal(t, "wf-test:a:1", out.Key, "IdempotencyKey is workflowID:node:step")
	assert.Equal(t, "wf-test", res.RunID)
	assert.Equal(t, "lin", res.Graph)
	assert.Equal(t, []string{"a", "b"}, res.Path)
	require.Len(t, res.Steps, 2)
	assert.InDelta(t, 0.75, res.TotalCostUSD, 1e-9)
	rep := res.Report()
	assert.Equal(t, res.Path, rep.Path)
	assert.InDelta(t, 0.75, rep.TotalCostUSD, 1e-9)
	assert.Equal(t, "b", rep.Steps[1].Node)

	// queries on the finished workflow
	v, err := env.QueryWorkflow(QueryState)
	require.NoError(t, err)
	var raw json.RawMessage
	require.NoError(t, v.Get(&raw))
	assert.Contains(t, string(raw), `"N":10`)
	v, err = env.QueryWorkflow(QueryReport)
	require.NoError(t, err)
	var rr Result
	require.NoError(t, v.Get(&rr))
	assert.InDelta(t, 0.75, rr.TotalCostUSD, 1e-9)
	p := queryPending(t, env)
	assert.False(t, p.Waiting)
}

func queryPending(t *testing.T, env *testsuite.TestWorkflowEnvironment) Pending {
	t.Helper()
	v, err := env.QueryWorkflow(QueryPending)
	require.NoError(t, err)
	var p Pending
	require.NoError(t, v.Get(&p))
	return p
}

func TestConditionalLoop(t *testing.T) {
	r := compile(t, graph.New[st]("loop").
		AddNode("inc", func(ctx context.Context, s *st) error {
			s.N++
			s.Seen = append(s.Seen, graph.IdempotencyKey(ctx))
			return nil
		}).
		AddNode("done", func(_ context.Context, s *st) error { s.Out = "ok"; return nil }).
		AddEdge(graph.Start, "inc").
		AddConditionalEdge("inc", func(s *st) string {
			if s.N < 3 {
				return "inc"
			}
			return "done"
		}, "inc", "done").
		AddEdge("done", graph.End))
	env := newEnv(t, r)
	out, res, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.Equal(t, 3, out.N)
	assert.Equal(t, "ok", out.Out)
	assert.Equal(t, []string{"inc", "inc", "inc", "done"}, res.Path)
	assert.Equal(t, []string{"wf-test:inc:1", "wf-test:inc:2", "wf-test:inc:3"}, out.Seen, "every loop iteration has its own key")
}

func TestMaxStepsNonRetryable(t *testing.T) {
	r := compile(t, graph.New[st]("spin").
		AddNode("n", func(_ context.Context, s *st) error { s.N++; return nil }).
		AddEdge(graph.Start, "n").
		AddConditionalEdge("n", func(*st) string { return "n" }, "n", graph.End), graph.WithMaxSteps(4))
	env := newEnv(t, r)
	_, _, err := execute(t, env, r, st{})
	require.Error(t, err)
	var ae *sdktemporal.ApplicationError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, ErrTypeMaxSteps, ae.Type())
	assert.True(t, ae.NonRetryable())
	assert.Contains(t, err.Error(), "max steps (4)")
}

func interruptGraph(t *testing.T) *graph.Runnable[st] {
	return compile(t, graph.New[st]("ask").
		AddNode("ask", func(ctx context.Context, s *st) error {
			if a, ok := graph.Answer(ctx); ok {
				s.Ans = a
				return nil
			}
			s.Seen = append(s.Seen, "asked")
			return graph.Interrupt("which db?")
		}).
		AddNode("after", func(_ context.Context, s *st) error { s.Out = "db=" + s.Ans; return nil }).
		AddEdge(graph.Start, "ask").AddEdge("ask", "after").AddEdge("after", graph.End))
}

func TestInterruptWaitsForAnswerSignal(t *testing.T) {
	r := interruptGraph(t)
	env := newEnv(t, r)
	var pend Pending
	env.RegisterDelayedCallback(func() {
		pend = queryPending(t, env)
		env.SignalWorkflow(SignalAnswer, "postgres")
	}, time.Minute)
	out, res, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.True(t, pend.Waiting)
	assert.Equal(t, "ask", pend.Node)
	assert.Equal(t, "which db?", pend.Question)
	assert.Equal(t, 1, pend.Step)
	assert.Equal(t, "postgres", out.Ans)
	assert.Equal(t, "db=postgres", out.Out)
	assert.Equal(t, []string{"asked"}, out.Seen, "the state of the interrupted attempt is carried into the re-run")
	assert.Equal(t, []string{"ask", "ask", "after"}, res.Path)
	require.Len(t, res.Steps, 3)
	assert.True(t, res.Steps[0].Paused)
	assert.Equal(t, "which db?", res.Steps[0].Question)
}

func TestInterruptReviewItemsInPending(t *testing.T) {
	r := compile(t, graph.New[st]("rev").
		AddNode("review", func(ctx context.Context, s *st) error {
			if rv, ok := graph.ReviewAnswer(ctx); ok {
				s.Ans = rv.General
				return nil
			}
			return graph.InterruptReview("look", []graph.ReviewItem{{Name: "p1", Path: "/p1.png"}})
		}).
		AddEdge(graph.Start, "review").AddEdge("review", graph.End))
	env := newEnv(t, r)
	var pend Pending
	env.RegisterDelayedCallback(func() {
		pend = queryPending(t, env)
		env.SignalWorkflow(SignalAnswer, `{"approved":true,"general":"lgtm"}`)
	}, time.Minute)
	out, _, err := execute(t, env, r, st{})
	require.NoError(t, err)
	require.Len(t, pend.Items, 1)
	assert.Equal(t, "p1", pend.Items[0].Name)
	assert.Equal(t, "lgtm", out.Ans)
}

func TestEarlySignalIsBuffered(t *testing.T) {
	r := interruptGraph(t)
	env := newEnv(t, r)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalAnswer, "early") }, 0)
	out, _, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.Equal(t, "early", out.Ans)
}

func TestNodeFailureIsRetriedThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	r := compile(t, graph.New[st]("flaky").
		AddNode("n", func(_ context.Context, s *st) error {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			if c < 3 {
				return errors.New("transient")
			}
			s.Out = "ok"
			return nil
		}).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End))
	env := newEnv(t, r, fast()...)
	out, _, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Out)
	assert.Equal(t, 3, calls)
}

func TestNodeFailureExhaustsRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	r := compile(t, graph.New[st]("dead").
		AddNode("n", func(context.Context, *st) error { mu.Lock(); calls++; mu.Unlock(); return errors.New("always") }).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End))
	env := newEnv(t, r, fast()...)
	_, _, err := execute(t, env, r, st{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "always")
	assert.Equal(t, 3, calls)
}

func TestNonRetryableClasses(t *testing.T) {
	cases := map[string]struct {
		g   *graph.Graph[st]
		typ string
	}{
		"route": {graph.New[st]("route").
			AddNode("n", func(context.Context, *st) error { return nil }).
			AddNode("m", func(context.Context, *st) error { return nil }).
			AddEdge(graph.Start, "n").
			AddConditionalEdge("n", func(*st) string { return "ghost" }, "m", graph.End).
			AddEdge("m", graph.End), ErrTypeRoute},
		"panic": {graph.New[st]("pan").
			AddNode("n", func(context.Context, *st) error { panic("bad") }).
			AddEdge(graph.Start, "n").AddEdge("n", graph.End), ErrTypeNode},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := compile(t, c.g)
			env := newEnv(t, r, fast()...)
			var attempts int
			env.SetOnActivityStartedListener(func(info *activityInfo, _ context.Context, _ encodedValues) {
				if info.ActivityType.Name == ActivityName(r) {
					attempts++
				}
			})
			_, _, err := execute(t, env, r, st{})
			require.Error(t, err)
			var ae *sdktemporal.ApplicationError
			require.ErrorAs(t, err, &ae)
			assert.Equal(t, c.typ, ae.Type())
			assert.LessOrEqual(t, attempts, 2, "first Start activity + one failing node, no retries")
		})
	}
}

func TestStateDecodeErrorNonRetryable(t *testing.T) {
	r := linear(t)
	env := newEnv(t, r, fast()...)
	env.ExecuteWorkflow(WorkflowName(r), Input{State: []byte(`{"N":"not a number"}`)})
	var ae *sdktemporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &ae)
	assert.Equal(t, ErrTypeState, ae.Type())
}

func TestEmptyStateStartsFromZero(t *testing.T) {
	r := linear(t)
	env := newEnv(t, r)
	env.ExecuteWorkflow(WorkflowName(r), Input{})
	require.NoError(t, env.GetWorkflowError())
}

func TestUnmarshalableStateNonRetryable(t *testing.T) {
	type bad struct{ F func() }
	r, err := graph.New[bad]("bad").
		AddNode("n", func(context.Context, *bad) error { return nil }).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End).Compile()
	require.NoError(t, err)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	Register(env, r)
	env.ExecuteWorkflow(WorkflowName(r), Input{State: []byte(`{}`)})
	var ae *sdktemporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &ae)
	assert.Equal(t, ErrTypeState, ae.Type())
}

func TestPerNodeTimeoutOption(t *testing.T) {
	r := compile(t, graph.New[st]("slow").
		AddNode("n", func(ctx context.Context, s *st) error {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			return ctx.Err()
		}).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End))
	env := newEnv(t, r, WithRetryPolicy(sdktemporal.RetryPolicy{MaximumAttempts: 1}), WithActivityTimeout(time.Hour), WithNodeTimeout("n", 100*time.Millisecond))
	_, _, err := execute(t, env, r, st{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
	c := newConfig([]Option{WithActivityTimeout(time.Minute), WithNodeTimeout("x", time.Second)})
	assert.Equal(t, time.Second, c.nodeTimeout("x"))
	assert.Equal(t, time.Minute, c.nodeTimeout("y"))
}

func TestConfigDefaults(t *testing.T) {
	c := newConfig(nil)
	assert.Equal(t, 30*time.Minute, c.timeout)
	assert.Equal(t, 10*time.Second, c.hbInterval)
	assert.Equal(t, int32(3), c.retry.MaximumAttempts)
	assert.Contains(t, c.retry.NonRetryableErrorTypes, ErrTypeRoute)
	c = newConfig([]Option{WithHeartbeat(3*time.Second, 0)})
	assert.Equal(t, time.Second, c.hbInterval)
	c = newConfig([]Option{WithHeartbeat(3*time.Second, time.Hour)})
	assert.Equal(t, time.Hour, c.hbInterval)
}

// ---- agent nodes: heartbeat and session resume --------------------------------

type agentRig struct {
	r        *graph.Runnable[st]
	argsFile string
}

// agentGraph is one agent node on a fake CLI. The first invocation starts
// session sess-1 and dies; the second finishes. Every argv is logged.
func agentGraph(t *testing.T, script string) agentRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat >/dev/null\necho \"$@\" >> " + argsFile + "\nn=$(cat " + filepath.Join(dir, "count") + " 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > " + filepath.Join(dir, "count") + "\n" + script
	require.NoError(t, os.WriteFile(bin, []byte(body), 0o755))
	rn := runner.New(runner.WithBinary(bin), runner.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	node := graph.Agent(rn,
		func(s *st) runner.Input { return runner.Input{Prompt: "do it"} },
		func(s *st, res *runner.Result) error { s.Out = res.ResultText; return nil })
	return agentRig{argsFile: argsFile, r: compile(t, graph.New[st]("agent").
		AddAgentNode("coder", node).AddEdge(graph.Start, "coder").AddEdge("coder", graph.End))}
}

const crashThenFinish = `if [ $n -eq 1 ]; then
  echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
  echo '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"working"}]},"session_id":"sess-1"}'
  sleep 0.4
  exit 1
fi
cat <<'S'
{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]},"session_id":"sess-1"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"done","total_cost_usd":0.04,"num_turns":1}
S
`

func TestRetriedAgentAttemptResumesSession(t *testing.T) {
	rig := agentGraph(t, crashThenFinish)
	env := newEnv(t, rig.r, fast(WithHeartbeat(100*time.Millisecond, 10*time.Millisecond))...)
	var (
		mu    sync.Mutex
		beats []Heartbeat
	)
	env.SetOnActivityHeartbeatListener(func(_ *activityInfo, details encodedValues) {
		var h Heartbeat
		if details.Get(&h) == nil {
			mu.Lock()
			beats = append(beats, h)
			mu.Unlock()
		}
	})
	out, res, err := execute(t, env, rig.r, st{})
	require.NoError(t, err)
	assert.Equal(t, "done", out.Out)
	assert.InDelta(t, 0.04, res.TotalCostUSD, 1e-9)

	b, err := os.ReadFile(rig.argsFile)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	require.Len(t, lines, 2, "two CLI invocations: the failed attempt and the retry")
	assert.NotContains(t, lines[0], "--resume")
	assert.Contains(t, lines[1], "--resume sess-1", "attempt 2 continues the session heartbeated by attempt 1")

	mu.Lock()
	defer mu.Unlock()
	var sawSession, sawEvents bool
	for _, h := range beats {
		if h.Node == graph.Start {
			continue
		}
		assert.Equal(t, "coder", h.Node)
		sawSession = sawSession || h.Session == "sess-1"
		sawEvents = sawEvents || h.Events > 0
	}
	assert.True(t, sawSession, "the session id is heartbeated as soon as it is known: %v", beats)
	assert.True(t, sawEvents, "the heartbeat carries the agent event count: %v", beats)
}

func TestHeartbeatTicksWhileNodeRuns(t *testing.T) {
	var mu sync.Mutex
	n := 0
	r := compile(t, graph.New[st]("tick").
		AddNode("n", func(ctx context.Context, s *st) error { time.Sleep(400 * time.Millisecond); return nil }).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End))
	env := newEnv(t, r, WithHeartbeat(100*time.Millisecond, 10*time.Millisecond))
	env.SetOnActivityHeartbeatListener(func(info *activityInfo, _ encodedValues) {
		if info.ActivityType.Name == ActivityName(r) {
			mu.Lock()
			n++
			mu.Unlock()
		}
	})
	_, _, err := execute(t, env, r, st{})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, n, 3, "a goroutine heartbeats while the node works")
}

func TestRetryOfPlainNodeIgnoresUnrelatedHeartbeat(t *testing.T) {
	// A retry whose previous heartbeat belonged to another node must not
	// pass its session on.
	var mu sync.Mutex
	calls := 0
	r := compile(t, graph.New[st]("hb").
		AddNode("n", func(ctx context.Context, s *st) error {
			mu.Lock()
			calls++
			c := calls
			mu.Unlock()
			if _, ok := graph.RecoveredSession(ctx); ok {
				s.Out = "recovered"
			}
			if c == 1 {
				return errors.New("once")
			}
			return nil
		}).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End))
	env := newEnv(t, r, fast()...)
	out, _, err := execute(t, env, r, st{})
	require.NoError(t, err)
	assert.Empty(t, out.Out, "no session was heartbeated, nothing to recover")
}

// ---- client helpers -------------------------------------------------------------

func TestStartWaitAnswerQuery(t *testing.T) {
	r := linear(t)
	c := &mocks.Client{}
	run := &mocks.WorkflowRun{}
	c.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
		return o.ID == "job-1" && o.TaskQueue == "graph-lin"
	}), "graph.lin", mock.MatchedBy(func(in Input) bool { return strings.Contains(string(in.State), `"N":4`) })).Return(run, nil)
	run.On("Get", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		*(a.Get(1).(*Result)) = Result{RunID: "job-1", State: []byte(`{"N":9}`), TotalCostUSD: 1}
	}).Return(nil)
	got, err := Start(context.Background(), c, r, st{N: 4}, StartOptions{ID: "job-1"})
	require.NoError(t, err)
	s, res, err := Wait[st](context.Background(), got)
	require.NoError(t, err)
	assert.Equal(t, 9, s.N)
	assert.InDelta(t, 1, res.TotalCostUSD, 1e-9)

	c.On("SignalWorkflow", mock.Anything, "job-1", "", SignalAnswer, "yes").Return(nil)
	require.NoError(t, Answer(context.Background(), c, "job-1", "yes"))

	ev := &mocks.Value{}
	ev.On("Get", mock.Anything).Run(func(a mock.Arguments) {
		*(a.Get(0).(*Pending)) = Pending{Waiting: true, Question: "q?"}
	}).Return(nil)
	c.On("QueryWorkflow", mock.Anything, "job-1", "", QueryPending).Return(ev, nil)
	p, err := PendingQuestion(context.Background(), c, "job-1")
	require.NoError(t, err)
	assert.Equal(t, "q?", p.Question)

	c.On("QueryWorkflow", mock.Anything, "gone", "", QueryPending).Return(nil, errors.New("not found"))
	_, err = PendingQuestion(context.Background(), c, "gone")
	assert.Error(t, err)
}

func TestStartAndWaitErrors(t *testing.T) {
	type bad struct{ F func() }
	rb, err := graph.New[bad]("bad").AddNode("n", func(context.Context, *bad) error { return nil }).
		AddEdge(graph.Start, "n").AddEdge("n", graph.End).Compile()
	require.NoError(t, err)
	_, err = Start(context.Background(), &mocks.Client{}, rb, bad{}, StartOptions{})
	assert.ErrorContains(t, err, "encode initial state")

	run := &mocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Return(errors.New("workflow failed")).Once()
	_, _, err = Wait[st](context.Background(), run)
	assert.EqualError(t, err, "workflow failed")

	run2 := &mocks.WorkflowRun{}
	run2.On("Get", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		*(a.Get(1).(*Result)) = Result{State: []byte(`{"N":"x"}`)}
	}).Return(nil)
	_, res, err := Wait[st](context.Background(), run2)
	assert.ErrorContains(t, err, "decode final state")
	assert.NotNil(t, res)
}
