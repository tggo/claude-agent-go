package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

// rec implements Observer and every optional extension, logging calls.
type rec struct {
	mu      sync.Mutex
	log     []string
	infos   []StepInfo
	states  map[string][2]string
	runIDs  []string
	agents  []StepInfo
	gateErr func(StepInfo) error
	endErr  error
	runInfo RunInfo
}

func (o *rec) add(f string, a ...any) {
	o.mu.Lock()
	o.log = append(o.log, fmt.Sprintf(f, a...))
	o.mu.Unlock()
}
func (o *rec) NodeStart(_ context.Context, i StepInfo) {
	o.add("start %s#%d.%d", i.Node, i.Step, i.Attempt)
	o.mu.Lock()
	o.infos = append(o.infos, i)
	o.mu.Unlock()
}
func (o *rec) NodeEnd(_ context.Context, i StepInfo, err error) {
	o.add("end %s#%d paused=%v err=%v", i.Node, i.Step, i.Paused, err != nil)
}
func (o *rec) RunStart(_ context.Context, i RunInfo) {
	o.add("run_start resumed=%v", i.Resumed)
	o.mu.Lock()
	o.runIDs = append(o.runIDs, i.RunID)
	o.runInfo = i
	o.mu.Unlock()
}
func (o *rec) RunEnd(_ context.Context, i RunInfo, rep *Report, err error) {
	o.add("run_end interrupt=%v", errors.Is(err, ErrInterrupt))
	o.mu.Lock()
	o.endErr = err
	o.mu.Unlock()
}
func (o *rec) EdgeTaken(_ context.Context, from, to string, step int) {
	o.add("edge %s>%s@%d", from, to, step)
}
func (o *rec) NodeState(_ context.Context, i StepInfo, b, a json.RawMessage) {
	o.mu.Lock()
	if o.states == nil {
		o.states = map[string][2]string{}
	}
	o.states[fmt.Sprintf("%s#%d", i.Node, i.Step)] = [2]string{string(b), string(a)}
	o.mu.Unlock()
}
func (o *rec) Resumed(_ context.Context, i StepInfo, answer string) {
	o.add("resumed %s#%d %q", i.Node, i.Step, answer)
}
func (o *rec) Before(_ context.Context, i StepInfo) error {
	o.add("before %s#%d", i.Node, i.Step)
	if o.gateErr != nil {
		return o.gateErr(i)
	}
	return nil
}
func (o *rec) AgentEvent(_ context.Context, i StepInfo, ev claudecli.StreamEvent) {
	o.mu.Lock()
	o.agents = append(o.agents, i)
	o.mu.Unlock()
	o.add("agent %s#%d %s", i.Node, i.Step, ev.Type)
}

type cnt struct{ N int }

func loopGraph(t *testing.T, o Observer) *Runnable[cnt] {
	t.Helper()
	g := New[cnt]("loop").
		AddNode("inc", Func(func(s *cnt) error { s.N++; return nil })).
		AddNode("done", Func(func(s *cnt) error { return nil })).
		AddEdge(Start, "inc").
		AddConditionalEdge("inc", func(s *cnt) string {
			if s.N < 3 {
				return "inc"
			}
			return "done"
		}, "inc", "done").
		LabelEdge("inc", "inc", "again").
		AddEdge("done", End)
	r, err := g.Compile(WithObserver(o))
	require.NoError(t, err)
	return r
}

func TestTopology(t *testing.T) {
	g := New[cnt]("topo").
		AddAgentNode("a", Func(func(*cnt) error { return nil })).
		AddNode("b1", Func(func(*cnt) error { return nil })).
		AddNode("b2", Func(func(*cnt) error { return nil })).
		AddNode("c", Func(func(*cnt) error { return nil })).
		MarkAgent("c", "nope").
		AddParallel("par", []string{"b1", "b2"}, func(*cnt, []cnt) error { return nil }).
		AddEdge(Start, "a").
		AddEdge("a", "par").
		AddConditionalEdge("par", func(*cnt) string { return End }, "c", "a", End).
		AddEdge("c", End).
		LabelEdge("par", "a", "retry")
	r, err := g.Compile()
	require.NoError(t, err)
	top := r.Topology()
	assert.Equal(t, "topo", top.Name)
	assert.Equal(t, Start, top.Entry)
	kinds := map[string]string{}
	for _, n := range top.Nodes {
		kinds[n.ID] = n.Kind
	}
	assert.Equal(t, map[string]string{Start: "start", "a": "agent", "b1": "func", "b2": "func", "c": "agent", "par": "parallel", End: "end"}, kinds)
	for _, n := range top.Nodes {
		switch n.ID {
		case "par":
			assert.Equal(t, []string{"b1", "b2"}, n.Branches)
		case "b1":
			assert.Equal(t, "par", n.Parent)
		}
	}
	find := func(from, to string) TopoEdge {
		for _, e := range top.Edges {
			if e.From == from && e.To == to {
				return e
			}
		}
		t.Fatalf("no edge %s>%s", from, to)
		return TopoEdge{}
	}
	assert.False(t, find(Start, "a").Conditional)
	assert.True(t, find("par", "c").Conditional, "all declared conditional targets are listed")
	assert.True(t, find("par", End).Conditional)
	e := find("par", "a")
	assert.True(t, e.Back && e.Conditional)
	assert.Equal(t, "retry", e.Label)
	assert.Equal(t, "branch", find("par", "b1").Label)
	assert.False(t, find("a", "par").Back)
	_, err = json.Marshal(top)
	assert.NoError(t, err)
}

func TestObserverEventsLoop(t *testing.T) {
	o := &rec{}
	r := loopGraph(t, o)
	st, rep, err := r.Run(context.Background(), cnt{})
	require.NoError(t, err)
	assert.Equal(t, 3, st.N)
	assert.Equal(t, []string{
		"run_start resumed=false",
		"edge __start__>inc@0",
		"before inc#1", "start inc#1.1", "end inc#1 paused=false err=false", "edge inc>inc@1",
		"before inc#2", "start inc#2.2", "end inc#2 paused=false err=false", "edge inc>inc@2",
		"before inc#3", "start inc#3.3", "end inc#3 paused=false err=false", "edge inc>done@3",
		"before done#4", "start done#4.1", "end done#4 paused=false err=false", "edge done>__end__@4",
		"run_end interrupt=false",
	}, o.log)
	assert.Equal(t, [2]string{`{"N":1}`, `{"N":2}`}, o.states["inc#2"])
	assert.Equal(t, 4, len(rep.Steps))
	assert.Equal(t, `{"N":0}`, string(o.runInfo.State))
	assert.Equal(t, "loop", o.runInfo.Topology.Name)
	assert.NotEmpty(t, o.runIDs[0])
	for _, i := range o.infos {
		assert.Equal(t, o.runIDs[0], i.RunID)
	}
}

type unmarshalable struct{ C chan int }

func TestStateSkippedWhenNotMarshalable(t *testing.T) {
	o := &rec{}
	r, err := New[unmarshalable]("u").
		AddNode("n", Func(func(*unmarshalable) error { return nil })).
		AddEdge(Start, "n").AddEdge("n", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), unmarshalable{C: make(chan int)})
	require.NoError(t, err)
	assert.Empty(t, o.states)
	assert.Empty(t, o.runInfo.State)
}

func TestRunIDAcrossResume(t *testing.T) {
	o := &rec{}
	g := New[cnt]("ask").
		AddNode("q", func(ctx context.Context, s *cnt) error {
			if _, ok := Answer(ctx); ok {
				s.N = 42
				return nil
			}
			return Interrupt("sure?")
		}).
		AddEdge(Start, "q").AddEdge("q", End)
	r, err := g.Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), cnt{})
	intr, ok := AsInterrupt[cnt](err)
	require.True(t, ok)
	assert.NotEmpty(t, intr.Checkpoint.RunID)
	assert.True(t, o.endErr != nil && errors.Is(o.endErr, ErrInterrupt))

	// Round trip through JSON keeps the id.
	b, err := json.Marshal(intr.Checkpoint)
	require.NoError(t, err)
	var cp Checkpoint[cnt]
	require.NoError(t, json.Unmarshal(b, &cp))

	st, _, err := r.Resume(context.Background(), cp, "yes")
	require.NoError(t, err)
	assert.Equal(t, 42, st.N)
	require.Len(t, o.runIDs, 2)
	assert.Equal(t, o.runIDs[0], o.runIDs[1])
	assert.Equal(t, cp.RunID, o.runIDs[1])
	assert.Contains(t, o.log, `resumed q#1 "yes"`)
	assert.Equal(t, 2, o.infos[1].Attempt, "re-run after interrupt is attempt 2")
	// Gate is not consulted for the resumed re-run.
	n := 0
	for _, l := range o.log {
		if l == "before q#1" {
			n++
		}
	}
	assert.Equal(t, 1, n)

	// A checkpoint without an id gets a fresh one.
	cp.RunID = ""
	_, _, err = r.Resume(context.Background(), cp, "yes")
	require.NoError(t, err)
	assert.NotEqual(t, o.runIDs[0], o.runIDs[2])
}

func TestGateBlocksAndFails(t *testing.T) {
	o := &rec{}
	release := make(chan struct{})
	entered := make(chan struct{})
	o.gateErr = func(i StepInfo) error {
		if i.Node == "done" {
			close(entered)
			<-release
		}
		return nil
	}
	r := loopGraph(t, o)
	type out struct {
		st  cnt
		err error
	}
	ch := make(chan out)
	go func() {
		st, _, err := r.Run(context.Background(), cnt{})
		ch <- out{st, err}
	}()
	<-entered
	select {
	case <-ch:
		t.Fatal("run finished while gate was blocking")
	default:
	}
	close(release)
	res := <-ch
	require.NoError(t, res.err)

	boom := errors.New("stop here")
	o2 := &rec{gateErr: func(i StepInfo) error {
		if i.Step == 2 {
			return boom
		}
		return nil
	}}
	_, rep, err := loopGraph(t, o2).Run(context.Background(), cnt{})
	require.ErrorIs(t, err, boom)
	assert.Len(t, rep.Steps, 1, "node 2 never ran")
	assert.Equal(t, "run_end interrupt=false", o2.log[len(o2.log)-1])
}

func TestStartRouteError(t *testing.T) {
	o := &rec{}
	r, err := New[cnt]("bad").
		AddNode("n", Func(func(*cnt) error { return nil })).
		AddConditionalEdge(Start, func(*cnt) string { return "zzz" }, "n").
		AddEdge("n", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), cnt{})
	require.ErrorIs(t, err, ErrInvalidRoute)
	assert.Equal(t, "run_end interrupt=false", o.log[len(o.log)-1])
}

func TestParallelEdgesAndBranchState(t *testing.T) {
	o := &rec{}
	r, err := New[cnt]("p").
		AddNode("x", Func(func(s *cnt) error { s.N += 1; return nil })).
		AddNode("y", Func(func(s *cnt) error { s.N += 10; return nil })).
		AddParallel("fan", []string{"x", "y"}, func(d *cnt, rs []cnt) error {
			for _, r := range rs {
				d.N += r.N
			}
			return nil
		}).
		AddEdge(Start, "fan").AddEdge("fan", End).Compile(WithObserver(o))
	require.NoError(t, err)
	st, _, err := r.Run(context.Background(), cnt{})
	require.NoError(t, err)
	assert.Equal(t, 11, st.N)
	assert.Contains(t, o.log, "edge fan>x@1")
	assert.Contains(t, o.log, "edge fan>y@1")
	assert.Equal(t, [2]string{`{"N":0}`, `{"N":10}`}, o.states["y#1"])
	assert.Equal(t, [2]string{`{"N":0}`, `{"N":11}`}, o.states["fan#1"])
}

func TestAgentEventsTaggedWithStep(t *testing.T) {
	o := &rec{}
	var user int
	r := runner.New(quiet, runner.WithBinary(fakeClaude(t, streamScript("hi", false, "0.01"))))
	g, err := New[astate]("ag").
		AddAgentNode("coder", Agent(r, func(*astate) runner.Input { return runner.Input{Prompt: "p"} }, nil,
			WithProgress(func(claudecli.StreamEvent, int) { user++ }))).
		AddEdge(Start, "coder").AddEdge("coder", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = g.Run(context.Background(), astate{})
	require.NoError(t, err)
	assert.Equal(t, 3, user, "WithProgress still called")
	require.Len(t, o.agents, 3)
	for _, i := range o.agents {
		assert.Equal(t, "coder", i.Node)
		assert.Equal(t, 1, i.Step)
		assert.NotEmpty(t, i.RunID)
	}
	assert.Equal(t, KindAgent, g.Topology().Nodes[1].Kind)
}

// An observer with only the two base methods keeps working.
type baseOnly struct{ n int }

func (b *baseOnly) NodeStart(context.Context, StepInfo)      { b.n++ }
func (b *baseOnly) NodeEnd(context.Context, StepInfo, error) { b.n++ }

func TestBaseObserverStillWorks(t *testing.T) {
	b := &baseOnly{}
	r := loopGraph(t, b)
	_, _, err := r.Run(context.Background(), cnt{})
	require.NoError(t, err)
	assert.Equal(t, 8, b.n)
}

func TestTopologyFuncLocationAndRunner(t *testing.T) {
	r := runner.New(quiet, runner.WithModel("haiku"), runner.WithMaxTurns(4), runner.WithTimeout(time.Minute))
	g := New[astate]("loc").
		AddNode("plain", namedStep).
		AddNode("b1", Func(func(*astate) error { return nil })).
		AddParallel("par", []string{"b1"}, mergeStep).
		AddNode("ag", Agent(r, func(*astate) runner.Input { return runner.Input{} }, nil)).
		AgentRunner("ag", r).
		AddEdge(Start, "plain").AddEdge("plain", "par").AddEdge("par", "ag").AddEdge("ag", End)
	c, err := g.Compile(WithMaxSteps(9), WithNodeTimeout(2*time.Second))
	require.NoError(t, err)
	top := c.Topology()
	assert.Equal(t, 9, top.MaxSteps)
	assert.Equal(t, int64(2000), top.NodeTimeoutMS)
	by := map[string]TopoNode{}
	for _, n := range top.Nodes {
		by[n.ID] = n
	}
	assert.Contains(t, by["plain"].Func, "namedStep")
	assert.Contains(t, by["plain"].File, "events_test.go")
	assert.Positive(t, by["plain"].Line)
	assert.Contains(t, by["par"].Func, "mergeStep", "parallel nodes report their merge function")
	assert.Empty(t, by["ag"].Func, "the Agent adapter is not interesting")
	require.NotNil(t, by["ag"].Runner)
	assert.Equal(t, "haiku", by["ag"].Runner.Model)
	assert.Equal(t, 4, by["ag"].Runner.MaxTurns)
	assert.Equal(t, KindAgent, by["ag"].Kind)
	assert.Nil(t, by["plain"].Runner)
}

func namedStep(context.Context, *astate) error { return nil }
func mergeStep(*astate, []astate) error        { return nil }

type inObs struct {
	rec
	ins []AgentInfo
}

func (o *inObs) AgentInput(_ context.Context, i StepInfo, in AgentInfo) {
	o.mu.Lock()
	o.ins = append(o.ins, in)
	o.mu.Unlock()
	o.add("input %s#%d", i.Node, i.Step)
}

func TestAgentInputReported(t *testing.T) {
	o := &inObs{}
	r := runner.New(quiet, runner.WithBinary(fakeClaude(t, streamScript("ok", false, "0.01"))), runner.WithMaxTurns(5))
	g, err := New[astate]("in").
		AddAgentNode("coder", Agent(r, func(s *astate) runner.Input { return runner.Input{Prompt: "PROMPT"} }, nil,
			WithRetry(runner.RetryPolicy{Resume: true}))).
		AddEdge(Start, "coder").AddEdge("coder", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = g.Run(context.Background(), astate{})
	require.NoError(t, err)
	require.Len(t, o.ins, 1)
	assert.Equal(t, "PROMPT", o.ins[0].Input.Prompt)
	assert.Equal(t, "coder", o.ins[0].Input.Label, "label defaults to the node name")
	assert.Equal(t, 5, o.ins[0].Input.MaxTurns)
	assert.Equal(t, &RetryInfo{MaxAttempts: 3, Resume: true}, o.ins[0].Retry)
}
