package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codingFlow: plan -> impl -> review, review loops back to impl until N >= 2.
func codingFlow() *Graph[st] {
	return New[st]("Coding").
		AddNode("plan", logNode("plan")).
		AddNode("impl", func(_ context.Context, s *st) error { s.N++; s.Log = append(s.Log, "impl"); return nil }).
		AddNode("review", logNode("review")).
		AddEdge(Start, "plan").AddEdge("plan", "impl").AddEdge("impl", "review").
		AddConditionalEdge("review", func(s *st) string {
			if s.N < 2 {
				return "impl"
			}
			return End
		}, "impl", End).
		LabelEdge("review", "impl", "changes requested")
}

func researchFlow() *Graph[st] {
	return New[st]("Research").
		AddNode("find", logNode("find")).
		AddNode("sum", logNode("sum")).
		AddEdge(Start, "find").AddEdge("find", "sum").AddEdge("sum", End)
}

func router(t *testing.T, opts ...CompileOption) *Runnable[st] {
	t.Helper()
	r, err := New[st]("inbox").
		AddNode("classify", logNode("classify")).
		AddSubgraph("coding", codingFlow()).
		AddSubgraphWith("research", GroupOpts{Collapsed: true}, researchFlow()).
		AddNode("answer", logNode("answer")).
		AddNode("reply", logNode("reply")).
		AddEdge(Start, "classify").
		AddConditionalEdge("classify", func(s *st) string { return s.Text }, "coding", "research", "answer").
		LabelEdge("classify", "coding", "coding").
		AddEdge("coding", "reply").
		AddEdge("research", "reply").
		AddEdge("answer", "reply").
		AddEdge("reply", End).
		Compile(opts...)
	require.NoError(t, err)
	return r
}

func TestSubgraphRoutes(t *testing.T) {
	r := router(t)
	for _, c := range []struct {
		kind string
		log  []string
	}{
		{"coding", []string{"classify", "plan", "impl", "review", "impl", "review", "reply"}},
		{"research", []string{"classify", "find", "sum", "reply"}},
		{"answer", []string{"classify", "answer", "reply"}},
	} {
		s, rep, err := r.Run(context.Background(), st{Text: c.kind})
		require.NoError(t, err, c.kind)
		assert.Equal(t, c.log, s.Log, c.kind)
		if c.kind == "research" {
			assert.Equal(t, []string{"classify", "research/find", "research/sum", "reply"}, rep.Path)
		}
	}
}

func TestSubgraphTopology(t *testing.T) {
	topo := router(t).Topology()
	var ids []string
	for _, n := range topo.Nodes {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{Start, "classify", "coding/plan", "coding/impl", "coding/review",
		"research/find", "research/sum", "answer", "reply", End}, ids)
	require.Len(t, topo.Groups, 2)
	assert.Equal(t, TopologyGroup{ID: "coding", Label: "Coding", Nodes: []string{"coding/plan", "coding/impl", "coding/review"}}, topo.Groups[0])
	assert.Equal(t, TopologyGroup{ID: "research", Label: "Research", Nodes: []string{"research/find", "research/sum"}, Collapsed: true}, topo.Groups[1])

	edges := map[[2]string]TopoEdge{}
	for _, e := range topo.Edges {
		edges[[2]string{e.From, e.To}] = e
	}
	assert.Equal(t, "coding", edges[[2]string{"classify", "coding/plan"}].Label, "label follows the entry")
	assert.True(t, edges[[2]string{"classify", "research/find"}].Conditional)
	assert.Equal(t, "changes requested", edges[[2]string{"coding/review", "coding/impl"}].Label)
	assert.True(t, edges[[2]string{"coding/review", "coding/impl"}].Back)
	assert.Contains(t, edges, [2]string{"coding/review", "reply"}, "End of the subgraph is the parent's exit")
	assert.Contains(t, edges, [2]string{"research/sum", "reply"})
}

// The parent's route out of a subgraph may be conditional, and a subgraph can
// be embedded twice and inside another subgraph.
func TestSubgraphNestedAndConditionalExit(t *testing.T) {
	inner := New[st]("Inner").AddNode("x", logNode("x")).AddEdge(Start, "x").AddEdge("x", End)
	outer := New[st]("Outer").AddNode("a", logNode("a")).AddSubgraph("in", inner).
		AddEdge(Start, "a").AddEdge("a", "in").AddEdge("in", End)
	r, err := New[st]("top").
		AddSubgraph("one", outer).
		AddSubgraph("two", outer).
		AddNode("done", logNode("done")).
		AddEdge(Start, "one").
		AddConditionalEdge("one", func(s *st) string {
			if len(s.Log) < 3 {
				return "two"
			}
			return "done"
		}, "two", "done").
		AddEdge("two", "done").
		AddEdge("done", End).
		Compile()
	require.NoError(t, err)
	s, rep, err := r.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "x", "a", "x", "done"}, s.Log)
	assert.Equal(t, []string{"one/a", "one/in/x", "two/a", "two/in/x", "done"}, rep.Path)
	topo := r.Topology()
	require.Len(t, topo.Groups, 2, "inner groups fold into the outer one")
	assert.Equal(t, []string{"one/a", "one/in/x"}, topo.Groups[0].Nodes)
	assert.Equal(t, "Outer", topo.Groups[0].Label)

	// Compiling again gives the same graph: the builder was not changed.
	r2, err := New[st]("again").AddSubgraph("one", outer).AddEdge(Start, "one").AddEdge("one", End).Compile()
	require.NoError(t, err)
	_, rep, err = r2.Run(context.Background(), st{})
	require.NoError(t, err)
	assert.Equal(t, []string{"one/a", "one/in/x"}, rep.Path)
}

// A subgraph ending in a conditional edge to End takes the parent's route.
func TestSubgraphConditionalEnd(t *testing.T) {
	sub := New[st]("S").AddNode("a", logNode("a")).AddNode("b", logNode("b")).
		AddEdge(Start, "a").
		AddConditionalEdge("a", func(s *st) string {
			if s.Text == "b" {
				return "b"
			}
			return End
		}, "b", End).
		AddEdge("b", End)
	for _, exitCond := range []bool{false, true} {
		g := New[st]("p").AddSubgraph("s", sub).AddNode("z", logNode("z")).
			AddEdge(Start, "s").AddEdge("z", End)
		if exitCond {
			g.AddConditionalEdge("s", func(*st) string { return "z" }, "z", End)
		} else {
			g.AddEdge("s", "z")
		}
		r, err := g.Compile()
		require.NoError(t, err)
		for _, text := range []string{"", "b"} {
			s, _, err := r.Run(context.Background(), st{Text: text})
			require.NoError(t, err)
			want := []string{"a", "z"}
			if text == "b" {
				want = []string{"a", "b", "z"}
			}
			assert.Equal(t, want, s.Log, "exitCond=%v text=%q", exitCond, text)
		}
	}
}

// A durable run fails inside a subgraph and Recover continues it there.
func TestSubgraphRecover(t *testing.T) {
	broken := true
	sub := New[st]("S").
		AddNode("a", logNode("a")).
		AddNode("b", func(_ context.Context, s *st) error {
			if broken {
				return errors.New("boom")
			}
			s.Log = append(s.Log, "b")
			return nil
		}).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End)
	store := MemStore()
	r, err := New[st]("p").AddNode("pre", logNode("pre")).AddSubgraph("s", sub).
		AddEdge(Start, "pre").AddEdge("pre", "s").AddEdge("s", End).
		Compile(WithCheckpointStore(store))
	require.NoError(t, err)
	_, rep, err := r.Run(context.Background(), st{})
	require.ErrorContains(t, err, `node "s/b": boom`)
	runs, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, runs, 1)
	_ = rep

	broken = false
	s, _, err := r.Recover(context.Background(), runs[0].RunID)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "a", "b"}, s.Log)
}

func TestSubgraphCompileErrors(t *testing.T) {
	ok := func() *Graph[st] {
		return New[st]("ok").AddNode("a", logNode("a")).AddEdge(Start, "a").AddEdge("a", End)
	}
	cases := map[string]struct {
		g    *Graph[st]
		want string
	}{
		"nil": {New[st]("p").AddSubgraph("s", nil), `subgraph "s": nil graph`},
		"no exit": {New[st]("p").AddSubgraph("s", ok()).AddEdge(Start, "s"),
			`node "s": no outgoing edge`},
		"start cond": {New[st]("p").AddSubgraph("s", New[st]("c").AddNode("a", logNode("a")).
			AddConditionalEdge(Start, func(*st) string { return "a" }, "a").AddEdge("a", End)).
			AddEdge(Start, "s").AddEdge("s", End), `its Start needs a static edge`},
		"start to end": {New[st]("p").AddSubgraph("s", New[st]("e").AddEdge(Start, End)).
			AddEdge(Start, "s").AddEdge("s", End), `goes straight to End`},
		"broken sub": {New[st]("p").AddSubgraph("s", New[st]("b").AddNode("a", logNode("a")).AddEdge(Start, "a")).
			AddEdge(Start, "s").AddEdge("s", End), `node "s/a": no outgoing edge`},
		"dup name": {New[st]("p").AddNode("s", logNode("s")).AddSubgraph("s", ok()).
			AddEdge(Start, "s").AddEdge("s", End), `node "s": duplicate name`},
		"clash": {New[st]("p").AddNode("s/a", logNode("x")).AddSubgraph("s", ok()).
			AddEdge(Start, "s").AddEdge("s", "s/a").AddEdge("s/a", End), `clashes`},
		"grouped": {New[st]("p").AddSubgraph("s", ok()).Group("g", "s").
			AddEdge(Start, "s").AddEdge("s", End), `"s" is a subgraph`},
		"branch": {New[st]("p").AddSubgraph("s", ok()).AddNode("b", logNode("b")).
			AddParallel("par", []string{"s", "b"}, func(*st, []st) error { return nil }).
			AddEdge(Start, "par").AddEdge("par", End), `branch "s" is a subgraph`},
	}
	self := New[st]("self")
	self.AddSubgraph("me", self)
	cases["self"] = struct {
		g    *Graph[st]
		want string
	}{self, "cannot embed itself"}
	a, b := New[st]("a"), New[st]("b")
	a.AddSubgraph("b", b).AddEdge(Start, "b").AddEdge("b", End)
	b.AddSubgraph("a", a).AddEdge(Start, "a").AddEdge("a", End)
	cases["cycle"] = struct {
		g    *Graph[st]
		want string
	}{a, "nested more than 32 deep"}

	for name, c := range cases {
		_, err := c.g.Compile()
		require.Error(t, err, name)
		assert.ErrorIs(t, err, ErrInvalidGraph, name)
		assert.ErrorContains(t, err, c.want, name)
	}
}
