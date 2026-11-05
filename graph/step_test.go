package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

type sstate struct {
	N    int
	Log  []string
	Ans  string
	Key  string
	Run  string
	Sess string
	To   string
}

func stepGraph(t *testing.T, opts ...CompileOption) *Runnable[sstate] {
	t.Helper()
	g := New[sstate]("stp").
		AddNode("a", func(ctx context.Context, s *sstate) error {
			s.N++
			s.Key = IdempotencyKey(ctx)
			s.Run = RunID(ctx)
			AddCost(ctx, 0.25)
			return nil
		}).
		AddNode("ask", func(ctx context.Context, s *sstate) error {
			if a, ok := Answer(ctx); ok {
				s.Ans = a
				return nil
			}
			s.Log = append(s.Log, "asked")
			return Interrupt("which?")
		}).
		AddNode("boom", func(context.Context, *sstate) error { return errors.New("kaput") }).
		AddNode("panic", func(context.Context, *sstate) error { panic("bad") }).
		AddNode("review", func(context.Context, *sstate) error {
			return InterruptReview("check", []ReviewItem{{Name: "1", Path: "/x"}})
		}).
		AddEdge(Start, "a").
		AddConditionalEdge("a", func(s *sstate) string {
			if s.To != "" {
				return s.To
			}
			if s.N > 1 {
				return "ask"
			}
			return End
		}, "ask", End, "boom", "panic", "review").
		AddEdge("ask", End).AddEdge("boom", End).AddEdge("panic", End).AddEdge("review", End)
	r, err := g.Compile(opts...)
	require.NoError(t, err)
	return r
}

func TestStepStartRoutesWithoutRunning(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), Start, sstate{}, StepOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a", res.Next)
	assert.Zero(t, res.State.N)
}

func TestStepRunsOneNodeAndRoutes(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), "a", sstate{}, StepOptions{RunID: "wf-1", Step: 3})
	require.NoError(t, err)
	assert.Equal(t, End, res.Next)
	assert.Equal(t, 1, res.State.N)
	assert.Equal(t, "wf-1:a:3", res.State.Key)
	assert.Equal(t, "wf-1", res.State.Run)
	assert.InDelta(t, 0.25, res.Step.CostUSD, 1e-9)
	assert.Equal(t, "a", res.Step.Node)
	res, err = r.Step(context.Background(), "a", res.State, StepOptions{RunID: "wf-1", Step: 4})
	require.NoError(t, err)
	assert.Equal(t, "ask", res.Next)
	assert.Equal(t, "wf-1:a:4", res.State.Key)
}

func TestStepDefaults(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), "a", sstate{}, StepOptions{})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.State.Run, "run-"))
	assert.Equal(t, res.State.Run+":a:1", res.State.Key)
}

func TestStepInterruptThenAnswer(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), "ask", sstate{}, StepOptions{Step: 2})
	require.NoError(t, err)
	assert.True(t, res.Paused)
	assert.Equal(t, "which?", res.Question)
	assert.Empty(t, res.Next)
	assert.Equal(t, []string{"asked"}, res.State.Log)
	assert.True(t, res.Step.Paused)

	ans := "pg"
	res, err = r.Step(context.Background(), "ask", res.State, StepOptions{Step: 2, Answer: &ans})
	require.NoError(t, err)
	assert.False(t, res.Paused)
	assert.Equal(t, "pg", res.State.Ans)
	assert.Equal(t, End, res.Next)
}

func TestStepInterruptReviewItems(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), "review", sstate{}, StepOptions{})
	require.NoError(t, err)
	assert.True(t, res.Paused)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "1", res.Items[0].Name)
}

func TestStepNodeErrorAndPanic(t *testing.T) {
	r := stepGraph(t)
	res, err := r.Step(context.Background(), "boom", sstate{N: 7}, StepOptions{})
	var ne *NodeError
	require.ErrorAs(t, err, &ne)
	assert.Equal(t, "boom", ne.Node)
	assert.Equal(t, 7, res.State.N)
	assert.Contains(t, res.Step.ErrText, "kaput")

	_, err = r.Step(context.Background(), "panic", sstate{}, StepOptions{})
	var pe *PanicError
	require.ErrorAs(t, err, &pe)
}

func TestStepUnknownAndBranchNodes(t *testing.T) {
	g := New[sstate]("p").
		AddNode("b1", func(context.Context, *sstate) error { return nil }).
		AddParallel("fan", []string{"b1"}, func(dst *sstate, rs []sstate) error { dst.N = len(rs); return nil }).
		AddEdge(Start, "fan").AddEdge("fan", End)
	r, err := g.Compile()
	require.NoError(t, err)
	_, err = r.Step(context.Background(), "nope", sstate{}, StepOptions{})
	assert.ErrorContains(t, err, "not a node")
	_, err = r.Step(context.Background(), "b1", sstate{}, StepOptions{})
	assert.ErrorContains(t, err, "not a node")
	res, err := r.Step(context.Background(), "fan", sstate{}, StepOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.State.N, "parallel node runs its branches and merge")
	assert.Equal(t, End, res.Next)
}

func TestStepInvalidRoute(t *testing.T) {
	g := New[sstate]("bad").
		AddNode("n", func(context.Context, *sstate) error { return nil }).
		AddNode("m", func(context.Context, *sstate) error { return nil }).
		AddEdge(Start, "n").
		AddConditionalEdge("n", func(*sstate) string { return "ghost" }, "m", End).
		AddEdge("m", End)
	r, err := g.Compile()
	require.NoError(t, err)
	_, err = r.Step(context.Background(), "n", sstate{}, StepOptions{})
	assert.ErrorIs(t, err, ErrInvalidRoute)
}

type stepObs struct {
	mu       sync.Mutex
	attempts []int
	resumed  []string
	gateErr  error
	gated    int
}

func (o *stepObs) NodeStart(_ context.Context, i StepInfo) {
	o.mu.Lock()
	o.attempts = append(o.attempts, i.Attempt)
	o.mu.Unlock()
}
func (o *stepObs) NodeEnd(context.Context, StepInfo, error) {}
func (o *stepObs) Resumed(_ context.Context, i StepInfo, a string) {
	o.resumed = append(o.resumed, i.Node+"="+a)
}
func (o *stepObs) Before(context.Context, StepInfo) error { o.gated++; return o.gateErr }

func TestStepObserverGateAndAttempt(t *testing.T) {
	o := &stepObs{}
	r := stepGraph(t, WithObserver(o))
	_, err := r.Step(context.Background(), "a", sstate{}, StepOptions{Attempt: 3})
	require.NoError(t, err)
	assert.Equal(t, []int{3}, o.attempts)
	assert.Equal(t, 1, o.gated)

	ans := "x"
	_, err = r.Step(context.Background(), "ask", sstate{}, StepOptions{Answer: &ans})
	require.NoError(t, err)
	assert.Equal(t, []string{"ask=x"}, o.resumed)
	assert.Equal(t, 1, o.gated, "an answered re-run is not gated, as in Resume")

	o.gateErr = errors.New("denied")
	res, err := r.Step(context.Background(), "a", sstate{}, StepOptions{})
	assert.EqualError(t, err, "denied")
	assert.Zero(t, res.State.N)
}

// stepAgentGraph has one agent node on a fake CLI that logs its argv.
func stepAgentGraph(t *testing.T) (*Runnable[astate], string) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakeClaude(t, `echo "$@" >> `+argsFile+"\n"+streamScript("done", false, "0.04"))
	r := runner.New(quiet, runner.WithBinary(bin))
	node := Agent(r,
		func(s *astate) runner.Input { return runner.Input{Prompt: "go"} },
		func(s *astate, res *runner.Result) error { s.Out = res.ResultText; return nil })
	g, err := New[astate]("ag").AddAgentNode("coder", node).AddEdge(Start, "coder").AddEdge("coder", End).Compile()
	require.NoError(t, err)
	return g, argsFile
}

func TestStepAgentHooksAndSessionResume(t *testing.T) {
	g, argsFile := stepAgentGraph(t)
	var (
		mu       sync.Mutex
		sessions []string
		events   int
	)
	hooks := StepHooks{
		OnSession: func(id string) { mu.Lock(); sessions = append(sessions, id); mu.Unlock() },
		OnEvent:   func(claudecli.StreamEvent, int) { mu.Lock(); events++; mu.Unlock() },
	}
	res, err := g.Step(context.Background(), "coder", astate{}, StepOptions{Hooks: hooks})
	require.NoError(t, err)
	assert.Equal(t, "done", res.State.Out)
	assert.Equal(t, []string{"sess-g"}, sessions)
	assert.Equal(t, 3, events)
	assert.InDelta(t, 0.04, res.Step.CostUSD, 1e-9)
	b, _ := os.ReadFile(argsFile)
	assert.NotContains(t, string(b), "--resume")

	// A retried attempt hands the logged session back: the CLI runs --resume.
	_, err = g.Step(context.Background(), "coder", astate{}, StepOptions{Session: "sess-old"})
	require.NoError(t, err)
	b, _ = os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], "--resume sess-old")
}

func TestStepHooksNilCallbacks(t *testing.T) {
	g, _ := stepAgentGraph(t)
	_, err := g.Step(context.Background(), "coder", astate{}, StepOptions{Hooks: StepHooks{}})
	require.NoError(t, err)
}
