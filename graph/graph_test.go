package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type st struct {
	Log  []string
	N    int
	Text string
}

func logNode(tag string) NodeFunc[st] {
	return func(_ context.Context, s *st) error {
		s.Log = append(s.Log, tag)
		return nil
	}
}

func linear(t *testing.T, opts ...CompileOption) *Runnable[st] {
	t.Helper()
	r, err := New[st]("lin").
		AddNode("a", logNode("a")).
		AddNode("b", logNode("b")).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End).
		Compile(opts...)
	require.NoError(t, err)
	return r
}

func TestLinear(t *testing.T) {
	out, rep, err := linear(t).Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, out.Log)
	assert.Equal(t, []string{"a", "b"}, rep.Path)
	require.Len(t, rep.Steps, 2)
	assert.Equal(t, "a", rep.Steps[0].Node)
	assert.NoError(t, rep.Steps[0].Err)
	assert.False(t, rep.Steps[0].Started.IsZero())
	assert.Positive(t, rep.Duration)
}

func TestConditionalRouting(t *testing.T) {
	build := func() *Runnable[st] {
		r, err := New[st]("c").
			AddNode("check", logNode("check")).
			AddNode("big", logNode("big")).
			AddNode("small", logNode("small")).
			AddEdge(Start, "check").
			AddConditionalEdge("check", func(s *st) string {
				if s.N > 5 {
					return "big"
				}
				return "small"
			}, "big", "small").
			AddEdge("big", End).AddEdge("small", End).
			Compile()
		require.NoError(t, err)
		return r
	}
	out, _, err := build().Run(context.Background(), st{N: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"check", "big"}, out.Log)
	out, _, err = build().Run(context.Background(), st{N: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"check", "small"}, out.Log)
}

func TestConditionalFromStartAndToEnd(t *testing.T) {
	r, err := New[st]("c").
		AddNode("x", logNode("x")).
		AddConditionalEdge(Start, func(s *st) string {
			if s.N == 0 {
				return End
			}
			return "x"
		}, "x", End).
		AddEdge("x", End).Compile()
	require.NoError(t, err)
	out, rep, err := r.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Empty(t, out.Log)
	assert.Empty(t, rep.Steps)
	out, _, err = r.Run(context.Background(), st{N: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, out.Log)
}

func reviewLoop(t *testing.T, maxSteps int) *Runnable[st] {
	r, err := New[st]("loop").
		AddNode("code", logNode("code")).
		AddNode("review", func(_ context.Context, s *st) error {
			s.N++
			s.Log = append(s.Log, "review")
			return nil
		}).
		AddNode("fix", logNode("fix")).
		AddNode("docs", logNode("docs")).
		AddEdge(Start, "code").AddEdge("code", "review").
		AddConditionalEdge("review", func(s *st) string {
			if s.N >= 3 {
				return "docs"
			}
			return "fix"
		}, "fix", "docs").
		AddEdge("fix", "review").AddEdge("docs", End).
		Compile(WithMaxSteps(maxSteps))
	require.NoError(t, err)
	return r
}

func TestReviewFixLoopExits(t *testing.T) {
	out, rep, err := reviewLoop(t, 100).Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, "code,review,fix,review,fix,review,docs", strings.Join(out.Log, ","))
	assert.Len(t, rep.Steps, 7)
}

func TestMaxSteps(t *testing.T) {
	_, rep, err := reviewLoop(t, 4).Run(context.Background(), st{})
	require.ErrorIs(t, err, ErrMaxSteps)
	assert.Contains(t, err.Error(), "code review fix review")
	assert.Contains(t, err.Error(), `before node "fix"`)
	assert.Len(t, rep.Steps, 4)
}

func TestInvalidRouteAtRuntime(t *testing.T) {
	r, err := New[st]("bad").
		AddNode("a", logNode("a")).AddNode("b", logNode("b")).
		AddEdge(Start, "a").
		AddConditionalEdge("a", func(*st) string { return "nope" }, "b").
		AddEdge("b", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.ErrorIs(t, err, ErrInvalidRoute)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), `"nope"`)
}

func TestRoutePanicRecovered(t *testing.T) {
	r, err := New[st]("bad").
		AddNode("a", logNode("a")).
		AddEdge(Start, "a").
		AddConditionalEdge("a", func(*st) string { panic("boom") }, End).
		Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "route panicked: boom")
}

func TestCompileErrors(t *testing.T) {
	ok := logNode("x")
	rt := func(*st) string { return End }
	cases := []struct {
		name  string
		build func() *Graph[st]
		want  []string
	}{
		{"no start edge", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge("a", End)
		}, []string{"Start has no outgoing edge"}},
		{"unknown target", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", "ghost")
		}, []string{`unknown node "ghost"`}},
		{"unknown from", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End).AddEdge("ghost", End)
		}, []string{`unknown node "ghost"`}},
		{"end as source", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End).AddEdge(End, "a")
		}, []string{"End has no outgoing edges"}},
		{"route to start", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", Start)
		}, []string{"cannot route to Start"}},
		{"no outgoing", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddNode("b", ok).AddEdge(Start, "a").AddEdge("a", "b")
		}, []string{`node "b": no outgoing edge`}},
		{"duplicate", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End)
		}, []string{`node "a": duplicate name`}},
		{"reserved and empty", func() *Graph[st] {
			return New[st]("g").AddNode(Start, ok).AddNode(End, ok).AddNode("", ok)
		}, []string{"reserved", "empty name"}},
		{"nil fn", func() *Graph[st] {
			return New[st]("g").AddNode("a", nil).AddEdge(Start, "a").AddEdge("a", End)
		}, []string{`node "a": nil function`}},
		{"both edge kinds", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End).
				AddConditionalEdge("a", rt, End)
		}, []string{`node "a": has both a static and a conditional edge`}},
		{"two static", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End).AddEdge("a", "a")
		}, []string{`node "a": more than one static edge`}},
		{"two conditional", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").
				AddConditionalEdge("a", rt, End).AddConditionalEdge("a", rt, End)
		}, []string{`node "a": more than one conditional edge`}},
		{"nil route", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddConditionalEdge("a", nil, End)
		}, []string{`node "a": nil route function`}},
		{"no targets", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddConditionalEdge("a", rt)
		}, []string{`node "a": conditional edge declares no targets`}},
		{"cond unknown target", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddConditionalEdge("a", rt, "zzz", End)
		}, []string{`unknown node "zzz"`}},
		{"cond unknown from", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddEdge(Start, "a").AddEdge("a", End).AddConditionalEdge("q", rt, End)
		}, []string{`unknown node "q"`}},
		{"unreachable", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddNode("island", ok).
				AddEdge(Start, "a").AddEdge("a", End).AddEdge("island", End)
		}, []string{`node "island": unreachable`}},
		{"end unreachable", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).AddNode("b", ok).
				AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", "a")
		}, []string{"End is not reachable"}},
		{"parallel nil merge and no branches", func() *Graph[st] {
			return New[st]("g").AddParallel("p", nil, nil).AddEdge(Start, "p").AddEdge("p", End)
		}, []string{`parallel node "p": nil merge`, `parallel node "p": no branches`}},
		{"parallel bad branches", func() *Graph[st] {
			m := func(*st, []st) error { return nil }
			return New[st]("g").AddNode("a", ok).
				AddParallel("p", []string{"a", "a", "nope", "q"}, m).
				AddParallel("q", []string{"a"}, m).
				AddEdge(Start, "p").AddEdge("p", "q").AddEdge("q", End)
		}, []string{`duplicate branch "a"`, `unknown branch "nope"`, `branch "q" is itself a parallel node`}},
		{"branch with edge", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).
				AddParallel("p", []string{"a"}, func(*st, []st) error { return nil }).
				AddEdge(Start, "p").AddEdge("p", End).AddEdge("a", End)
		}, []string{`node "a": is a parallel branch and must not have outgoing edges`}},
		{"branch routed to", func() *Graph[st] {
			return New[st]("g").AddNode("a", ok).
				AddParallel("p", []string{"a"}, func(*st, []st) error { return nil }).
				AddEdge(Start, "a").AddEdge("p", End)
		}, []string{`"a" is a parallel branch and cannot be routed to directly`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.build().Compile()
			require.Error(t, err)
			for _, w := range c.want {
				assert.Contains(t, err.Error(), w)
			}
		})
	}
}

func TestCompileOptionErrors(t *testing.T) {
	g := func() *Graph[st] {
		return New[st]("g").AddNode("a", logNode("a")).AddEdge(Start, "a").AddEdge("a", End)
	}
	_, err := g().Compile(WithMaxSteps(0))
	assert.ErrorContains(t, err, "WithMaxSteps")
	_, err = g().Compile(WithNodeTimeout(-1))
	assert.ErrorContains(t, err, "WithNodeTimeout")
}

func TestNodeErrorWrapped(t *testing.T) {
	sentinel := errors.New("kaput")
	r, err := New[st]("g").
		AddNode("a", func(context.Context, *st) error { return sentinel }).
		AddEdge(Start, "a").AddEdge("a", End).Compile()
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	require.ErrorIs(t, err, sentinel)
	var ne *NodeError
	require.ErrorAs(t, err, &ne)
	assert.Equal(t, "a", ne.Node)
	assert.Contains(t, err.Error(), `node "a"`)
	assert.ErrorIs(t, rep.Steps[0].Err, sentinel)
}

func TestPanicRecovered(t *testing.T) {
	r, err := New[st]("g").
		AddNode("a", func(context.Context, *st) error { panic("oh no") }).
		AddEdge(Start, "a").AddEdge("a", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	var pe *PanicError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "oh no", pe.Value)
	assert.NotEmpty(t, pe.Stack)
	assert.Contains(t, err.Error(), `node "a"`)
}

func TestContextCancelBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r, err := New[st]("g").
		AddNode("a", func(context.Context, *st) error { cancel(); return nil }).
		AddNode("b", logNode("b")).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End).Compile()
	require.NoError(t, err)
	out, _, err := r.Run(ctx, st{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), `before node "b"`)
	assert.Empty(t, out.Log)
}

func TestContextCancelDuringStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r, err := New[st]("g").
		AddNode("slow", func(ctx context.Context, _ *st) error { <-ctx.Done(); return ctx.Err() }).
		AddEdge(Start, "slow").AddEdge("slow", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(ctx, st{})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestNodeTimeout(t *testing.T) {
	r, err := New[st]("g").
		AddNode("slow", func(ctx context.Context, _ *st) error { <-ctx.Done(); return ctx.Err() }).
		AddEdge(Start, "slow").AddEdge("slow", End).Compile(WithNodeTimeout(20 * time.Millisecond))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestFuncAdapter(t *testing.T) {
	r, err := New[st]("g").
		AddNode("a", Func(func(s *st) error { s.N = 7; return nil })).
		AddEdge(Start, "a").AddEdge("a", End).Compile()
	require.NoError(t, err)
	out, _, err := r.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, 7, out.N)
}

// ---- parallel ----

func parallelGraph(t *testing.T, opts ...CompileOption) *Runnable[st] {
	t.Helper()
	branch := func(tag string, cost float64) NodeFunc[st] {
		return func(ctx context.Context, s *st) error {
			time.Sleep(5 * time.Millisecond)
			AddCost(ctx, cost)
			s.Log = append(s.Log, tag)
			return nil
		}
	}
	r, err := New[st]("par").
		AddNode("x", branch("x", 0.1)).
		AddNode("y", branch("y", 0.2)).
		AddNode("z", branch("z", 0.3)).
		AddParallel("fan", []string{"x", "y", "z"}, func(dst *st, rs []st) error {
			for _, r := range rs {
				dst.Log = append(dst.Log, r.Log...)
			}
			return nil
		}).
		AddNode("after", logNode("after")).
		AddEdge(Start, "fan").AddEdge("fan", "after").AddEdge("after", End).
		Compile(opts...)
	require.NoError(t, err)
	return r
}

func TestParallelFanOutMerge(t *testing.T) {
	out, rep, err := parallelGraph(t).Run(context.Background(), st{})
	require.NoError(t, err)
	// merge sees results in declared order
	assert.Equal(t, []string{"x", "y", "z", "after"}, out.Log)
	assert.Equal(t, []string{"fan", "after"}, rep.Path)
	assert.Len(t, rep.Steps, 5) // 3 branches + fan + after

	byNode := map[string]Step{}
	for _, s := range rep.Steps {
		byNode[s.Node] = s
	}
	assert.InDelta(t, 0.1, byNode["x"].CostUSD, 1e-9)
	assert.InDelta(t, 0.3, byNode["z"].CostUSD, 1e-9)
	assert.Equal(t, "fan", byNode["x"].Parent)
	assert.Zero(t, byNode["fan"].CostUSD)
	assert.InDelta(t, 0.6, rep.TotalCostUSD, 1e-9)
}

func TestParallelCloneIsolation(t *testing.T) {
	type S struct{ M map[string]int }
	deep := func(s S) S {
		m := map[string]int{}
		for k, v := range s.M {
			m[k] = v
		}
		return S{M: m}
	}
	var seenInMerge int
	r, err := New[S]("iso").
		AddNode("a", func(_ context.Context, s *S) error { s.M["a"] = 1; return nil }).
		AddNode("b", func(_ context.Context, s *S) error { s.M["b"] = 1; return nil }).
		AddParallel("p", []string{"a", "b"}, func(dst *S, rs []S) error {
			seenInMerge = len(dst.M)
			for _, r := range rs {
				for k, v := range r.M {
					dst.M[k] = v
				}
			}
			return nil
		}).
		AddEdge(Start, "p").AddEdge("p", End).WithClone(deep).Compile()
	require.NoError(t, err)
	out, _, err := r.Run(context.Background(), S{M: map[string]int{}})
	require.NoError(t, err)
	assert.Zero(t, seenInMerge, "branch write leaked into parent despite WithClone")
	assert.Equal(t, map[string]int{"a": 1, "b": 1}, out.M)
}

func TestParallelValueCopyIsolatesScalars(t *testing.T) {
	var seen []int
	r, err := New[st]("p").
		AddNode("a", func(_ context.Context, s *st) error { s.N += 1; return nil }).
		AddNode("b", func(_ context.Context, s *st) error { s.N += 2; return nil }).
		AddParallel("p", []string{"a", "b"}, func(dst *st, rs []st) error {
			for _, r := range rs {
				seen = append(seen, r.N)
			}
			return nil
		}).
		AddEdge(Start, "p").AddEdge("p", End).Compile()
	require.NoError(t, err)
	out, _, err := r.Run(context.Background(), st{N: 100})
	require.NoError(t, err)
	assert.Equal(t, []int{101, 102}, seen)
	assert.Equal(t, 100, out.N)
}

func TestParallelBranchErrorCancelsOthers(t *testing.T) {
	var cancelled atomic.Bool
	merged := false
	r, err := New[st]("p").
		AddNode("bad", func(context.Context, *st) error { return errors.New("branch failed") }).
		AddNode("slow", func(ctx context.Context, _ *st) error {
			select {
			case <-ctx.Done():
				cancelled.Store(true)
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		}).
		AddParallel("p", []string{"bad", "slow"}, func(*st, []st) error { merged = true; return nil }).
		AddEdge(Start, "p").AddEdge("p", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch failed")
	assert.Contains(t, err.Error(), `node "bad"`)
	assert.True(t, cancelled.Load())
	assert.False(t, merged)
}

func TestParallelMergeError(t *testing.T) {
	r, err := New[st]("p").
		AddNode("a", logNode("a")).
		AddParallel("p", []string{"a"}, func(*st, []st) error { return errors.New("merge broke") }).
		AddEdge(Start, "p").AddEdge("p", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	assert.ErrorContains(t, err, "merge broke")
	assert.ErrorContains(t, err, `node "p"`)
}

func TestInterruptInParallelRejected(t *testing.T) {
	mkG := func(branchAsks, mergeAsks bool) *Runnable[st] {
		r, err := New[st]("p").
			AddNode("a", func(context.Context, *st) error {
				if branchAsks {
					return Interrupt("branch q")
				}
				return nil
			}).
			AddParallel("p", []string{"a"}, func(*st, []st) error {
				if mergeAsks {
					return Interrupt("merge q")
				}
				return nil
			}).
			AddEdge(Start, "p").AddEdge("p", End).Compile()
		require.NoError(t, err)
		return r
	}
	for _, c := range [][2]bool{{true, false}, {false, true}} {
		_, _, err := mkG(c[0], c[1]).Run(context.Background(), st{})
		require.ErrorIs(t, err, ErrInterruptInParallel)
		var in *Interrupted[st]
		assert.False(t, errors.As(err, &in))
	}
}

// ---- interrupt / resume ----

func askGraph(t *testing.T) *Runnable[st] {
	r, err := New[st]("hitl").
		AddNode("plan", logNode("plan")).
		AddNode("ask", func(ctx context.Context, s *st) error {
			if a, ok := Answer(ctx); ok {
				s.Text = a
				s.Log = append(s.Log, "answered")
				return nil
			}
			s.Log = append(s.Log, "asking")
			return Interrupt("which db?")
		}).
		AddNode("code", func(_ context.Context, s *st) error {
			s.Log = append(s.Log, "code:"+s.Text)
			return nil
		}).
		AddEdge(Start, "plan").AddEdge("plan", "ask").AddEdge("ask", "code").AddEdge("code", End).
		Compile()
	require.NoError(t, err)
	return r
}

func TestInterruptResumeJSONRoundTrip(t *testing.T) {
	r := askGraph(t)
	_, rep, err := r.Run(context.Background(), st{})
	var in *Interrupted[st]
	require.ErrorAs(t, err, &in)
	assert.Equal(t, "which db?", in.Question)
	assert.Equal(t, "ask", in.Node)
	assert.Equal(t, []string{"plan", "ask"}, rep.Path)
	assert.Contains(t, in.Error(), "which db?")

	raw, err := json.Marshal(in.Checkpoint)
	require.NoError(t, err)
	var cp Checkpoint[st]
	require.NoError(t, json.Unmarshal(raw, &cp))
	raw2, err := json.Marshal(cp)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(raw2))
	assert.Equal(t, in.Checkpoint.Node, cp.Node)
	assert.Equal(t, in.Checkpoint.Path, cp.Path)
	require.Len(t, cp.Prior.Steps, 2)
	assert.True(t, cp.Prior.Steps[1].Paused)

	out, rep2, err := r.Resume(context.Background(), cp, "postgres")
	require.NoError(t, err)
	assert.Equal(t, []string{"plan", "asking", "answered", "code:postgres"}, out.Log)
	assert.Equal(t, []string{"plan", "ask", "ask", "code"}, rep2.Path)
	// Cumulative: the 2 steps before the pause plus the 2 after.
	assert.Len(t, rep2.Steps, 4)
	assert.GreaterOrEqual(t, rep2.Duration, rep.Duration)
	assert.True(t, errors.Is(in, ErrInterrupt))
	got, ok := AsInterrupt[st](fmt.Errorf("wrapped: %w", in))
	require.True(t, ok)
	assert.Equal(t, "ask", got.Node)
	_, ok = AsInterrupt[st](errors.New("other"))
	assert.False(t, ok)
	assert.True(t, errors.Is(Interrupt("q"), ErrInterrupt))
}

func TestResumeReportAccumulatesCost(t *testing.T) {
	r, err := New[st]("cost").
		AddNode("a", func(ctx context.Context, s *st) error { AddCost(ctx, 1.5); return nil }).
		AddNode("ask", func(ctx context.Context, s *st) error {
			if _, ok := Answer(ctx); ok {
				AddCost(ctx, 0.25)
				return nil
			}
			AddCost(ctx, 0.5)
			return Interrupt("q?")
		}).
		AddEdge(Start, "a").AddEdge("a", "ask").AddEdge("ask", End).Compile()
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	in, ok := AsInterrupt[st](err)
	require.True(t, ok)
	assert.InDelta(t, 2.0, rep.TotalCostUSD, 1e-9)
	_, rep2, err := r.Resume(context.Background(), in.Checkpoint, "x")
	require.NoError(t, err)
	assert.InDelta(t, 2.25, rep2.TotalCostUSD, 1e-9)
	assert.Equal(t, []string{"a", "ask", "ask"}, rep2.Path)
}

func TestCompileErrorIsErrInvalidGraph(t *testing.T) {
	_, err := New[st]("bad").AddEdge(Start, "missing").Compile()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidGraph)
}

func TestAnswerOnlyForResumedNodeOnce(t *testing.T) {
	var answers []string
	r, err := New[st]("loop").
		AddNode("ask", func(ctx context.Context, s *st) error {
			a, ok := Answer(ctx)
			if ok {
				answers = append(answers, a)
			} else if s.N == 0 {
				return Interrupt("first")
			} else {
				answers = append(answers, "<none>")
			}
			s.N++
			return nil
		}).
		AddEdge(Start, "ask").
		AddConditionalEdge("ask", func(s *st) string {
			if s.N < 3 {
				return "ask"
			}
			return End
		}, "ask", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	var in *Interrupted[st]
	require.ErrorAs(t, err, &in)
	_, _, err = r.Resume(context.Background(), in.Checkpoint, "yes")
	require.NoError(t, err)
	assert.Equal(t, []string{"yes", "<none>", "<none>"}, answers)
}

func TestResumeCanInterruptAgain(t *testing.T) {
	r, err := New[st]("q").
		AddNode("ask", func(ctx context.Context, s *st) error {
			if _, ok := Answer(ctx); ok {
				s.N++
			}
			if s.N < 2 {
				return Interrupt(fmt.Sprintf("q%d", s.N))
			}
			return nil
		}).
		AddEdge(Start, "ask").AddEdge("ask", End).Compile()
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	var in *Interrupted[st]
	require.ErrorAs(t, err, &in)
	assert.Equal(t, "q0", in.Question)
	_, _, err = r.Resume(context.Background(), in.Checkpoint, "a")
	require.ErrorAs(t, err, &in)
	assert.Equal(t, "q1", in.Question)
	out, _, err := r.Resume(context.Background(), in.Checkpoint, "b")
	require.NoError(t, err)
	assert.Equal(t, 2, out.N)
}

func TestResumeBudgetContinues(t *testing.T) {
	r, err := New[st]("q").
		AddNode("a", logNode("a")).
		AddNode("ask", func(ctx context.Context, s *st) error {
			if _, ok := Answer(ctx); ok {
				return nil
			}
			return Interrupt("?")
		}).
		AddEdge(Start, "a").AddEdge("a", "ask").AddEdge("ask", End).Compile(WithMaxSteps(2))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	var in *Interrupted[st]
	require.ErrorAs(t, err, &in)
	assert.Equal(t, 1, in.Checkpoint.Step)
	_, _, err = r.Resume(context.Background(), in.Checkpoint, "ok")
	require.NoError(t, err) // re-taking the same step number stays within budget
}

func TestResumeValidation(t *testing.T) {
	r := parallelGraph(t)
	_, _, err := r.Resume(context.Background(), Checkpoint[st]{Node: "ghost"}, "")
	assert.ErrorContains(t, err, `"ghost"`)
	_, _, err = r.Resume(context.Background(), Checkpoint[st]{Node: "x"}, "")
	assert.ErrorContains(t, err, `"x"`)
	_, _, err = r.Resume(context.Background(), Checkpoint[st]{Node: "after", Step: -1}, "")
	assert.ErrorContains(t, err, "negative")
}

// ---- observer / cost ----

type obsRec struct {
	mu     sync.Mutex
	events []string
}

func (o *obsRec) NodeStart(ctx context.Context, i StepInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, fmt.Sprintf("start:%s#%d:%s", i.Node, i.Step, NodeName(ctx)))
}

func (o *obsRec) NodeEnd(_ context.Context, i StepInfo, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := "ok"
	if err != nil {
		e = "err"
	}
	o.events = append(o.events, fmt.Sprintf("end:%s:%s:%.1f", i.Node, e, i.CostUSD))
}

func TestObserverOrder(t *testing.T) {
	o := &obsRec{}
	r, err := New[st]("o").
		AddNode("a", func(ctx context.Context, _ *st) error { AddCost(ctx, 0.5); return nil }).
		AddNode("b", func(context.Context, *st) error { return errors.New("x") }).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.Error(t, err)
	assert.Equal(t, []string{"start:a#1:a", "end:a:ok:0.5", "start:b#2:b", "end:b:err:0.0"}, o.events)
}

func TestObserverParallelBranches(t *testing.T) {
	o := &obsRec{}
	_, _, err := parallelGraph(t, WithObserver(o)).Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, "start:fan#1:fan", o.events[0])
	assert.Equal(t, "end:fan:ok:0.0", o.events[7])
	assert.Len(t, o.events, 10)
}

func TestCostOutsideNodeIsNoop(t *testing.T) {
	AddCost(context.Background(), 1)
	assert.Equal(t, "", NodeName(context.Background()))
	_, ok := Answer(context.Background())
	assert.False(t, ok)
}

func TestCostGoroutineSafe(t *testing.T) {
	r, err := New[st]("c").
		AddNode("a", func(ctx context.Context, _ *st) error {
			var wg sync.WaitGroup
			for i := 0; i < 50; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); AddCost(ctx, 0.01) }()
			}
			wg.Wait()
			return nil
		}).AddEdge(Start, "a").AddEdge("a", End).Compile()
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, rep.TotalCostUSD, 1e-9)
}

func TestConcurrentRuns(t *testing.T) {
	r := parallelGraph(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, rep, err := r.Run(context.Background(), st{})
			assert.NoError(t, err)
			assert.Len(t, out.Log, 4)
			assert.InDelta(t, 0.6, rep.TotalCostUSD, 1e-9)
		}()
	}
	wg.Wait()
}

func TestBuilderMutationAfterCompileIgnored(t *testing.T) {
	g := New[st]("g").AddNode("a", logNode("a")).AddEdge(Start, "a").AddEdge("a", End)
	r, err := g.Compile()
	require.NoError(t, err)
	g.AddNode("b", logNode("b"))
	out, _, err := r.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, out.Log)
}

// ---- mermaid ----

func TestMermaidGolden(t *testing.T) {
	m := func(*st, []st) error { return nil }
	r, err := New[st]("m").
		AddNode("plan", logNode("p")).
		AddNode("lint", logNode("l")).
		AddNode("test", logNode("t")).
		AddParallel("checks", []string{"lint", "test"}, m).
		AddNode("review", logNode("r")).
		AddNode("end", logNode("e")).
		AddEdge(Start, "plan").AddEdge("plan", "checks").AddEdge("checks", "review").
		AddConditionalEdge("review", func(*st) string { return End }, "plan", "end", End).
		AddEdge("end", End).
		Compile()
	require.NoError(t, err)
	want := `flowchart TD
    __start__(["start"])
    __end__(["end"])
    plan["plan"]
    subgraph checks["checks (parallel)"]
        direction LR
        checks__lint["lint"]
        checks__test["test"]
    end
    review["review"]
    n_end["end"]
    __start__ --> plan
    plan --> checks
    checks --> review
    review -.->|"plan"| plan
    review -.->|"end"| n_end
    review -.->|"end"| __end__
    n_end --> __end__
`
	assert.Equal(t, want, r.Mermaid())
}

func TestMermaidIDs(t *testing.T) {
	_, err := New[st]("m").AddNode("", logNode("x")).AddEdge(Start, "").Compile()
	require.Error(t, err)

	r, err := New[st]("m").
		AddNode("a-b", logNode("x")).AddNode("a b", logNode("x")).AddNode(`q"t`, logNode("x")).
		AddEdge(Start, "a-b").AddEdge("a-b", "a b").AddEdge("a b", `q"t`).AddEdge(`q"t`, End).
		Compile()
	require.NoError(t, err)
	out := r.Mermaid()
	assert.Contains(t, out, `a_b["a-b"]`)
	assert.Contains(t, out, `a_b_2["a b"]`)
	assert.Contains(t, out, `q_t["q#quot;t"]`)
}

func TestInterruptIsPauseNotFailure(t *testing.T) {
	o := &pauseObs{}
	r, err := New[st]("hitl").
		AddNode("ask", func(ctx context.Context, s *st) error {
			if _, ok := Answer(ctx); ok {
				return nil
			}
			return Interrupt("sure?")
		}).
		AddEdge(Start, "ask").AddEdge("ask", End).Compile(WithObserver(o))
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	var in *Interrupted[st]
	require.ErrorAs(t, err, &in)
	require.Len(t, rep.Steps, 1)
	assert.True(t, rep.Steps[0].Paused)
	assert.Equal(t, "sure?", rep.Steps[0].Question)
	assert.NoError(t, rep.Steps[0].Err)
	assert.Equal(t, []string{"ask"}, rep.Path)
	require.Len(t, o.ends, 1)
	assert.True(t, o.ends[0].Paused)
	assert.Equal(t, "sure?", o.ends[0].Question)
	assert.NoError(t, o.errs[0])

	_, rep2, err := r.Resume(context.Background(), in.Checkpoint, "yes")
	require.NoError(t, err)
	require.Len(t, rep2.Steps, 2)
	assert.True(t, rep2.Steps[0].Paused) // carried over from before the pause
	assert.False(t, rep2.Steps[1].Paused)
	assert.False(t, o.ends[1].Paused)
}

type pauseObs struct {
	ends []StepInfo
	errs []error
}

func (o *pauseObs) NodeStart(context.Context, StepInfo) {}
func (o *pauseObs) NodeEnd(_ context.Context, i StepInfo, err error) {
	o.ends = append(o.ends, i)
	o.errs = append(o.errs, err)
}

func TestInterruptInBranchStaysFailedStep(t *testing.T) {
	r, err := New[st]("p").
		AddNode("a", func(context.Context, *st) error { return Interrupt("q") }).
		AddParallel("p", []string{"a"}, func(*st, []st) error { return nil }).
		AddEdge(Start, "p").AddEdge("p", End).Compile()
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	require.ErrorIs(t, err, ErrInterruptInParallel)
	for _, s := range rep.Steps {
		assert.False(t, s.Paused)
		assert.Error(t, s.Err)
	}
}

// ADK #7286: a checkpoint's JSON is stable across a round trip and keeps the
// order of a long path with repeated nodes.
func TestCheckpointJSONStable(t *testing.T) {
	cp := Checkpoint[st]{State: st{Log: []string{"b", "a", "b"}, N: 3, Text: "x"}, Node: "ask", Step: 100}
	for i := 0; i < 100; i++ {
		cp.Path = append(cp.Path, []string{"a", "b", "a"}[i%3])
	}
	first, err := json.Marshal(cp)
	require.NoError(t, err)
	var back Checkpoint[st]
	require.NoError(t, json.Unmarshal(first, &back))
	assert.Equal(t, cp, back)
	second, err := json.Marshal(back)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}
