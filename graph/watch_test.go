package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func watchGraph(watch bool) *Graph[cnt] {
	g := New[cnt]("watch").
		AddNode("work", Func(func(c *cnt) error { c.N++; return nil })).
		AddNode("guard", Func(func(*cnt) error { return nil })).
		AddEdge(Start, "work").
		AddEdge("work", "guard").
		AddEdge("guard", End)
	if watch {
		g.Watch("guard", "work", "nope", "guard")
	}
	return g
}

func TestWatchTopology(t *testing.T) {
	r, err := watchGraph(true).Compile()
	require.NoError(t, err)
	var watches []TopoEdge
	for _, e := range r.Topology().Edges {
		if e.Watch {
			watches = append(watches, e)
		}
		assert.False(t, e.Back, "%s→%s", e.From, e.To)
	}
	// unknown nodes and self-links are dropped
	assert.Equal(t, []TopoEdge{{From: "guard", To: "work", Watch: true}}, watches)

	plain, err := watchGraph(false).Compile()
	require.NoError(t, err)
	assert.Equal(t, plain.topoHash(), r.topoHash(), "watch links are presentation only")

	// the run never takes a watch link
	s, rep, err := r.Run(context.Background(), cnt{})
	require.NoError(t, err)
	assert.Equal(t, 1, s.N)
	assert.Equal(t, []string{"work", "guard"}, rep.Path)
}

func TestWatchMermaid(t *testing.T) {
	r, err := watchGraph(true).Compile()
	require.NoError(t, err)
	assert.Contains(t, r.Mermaid(), " -. watches .- ")
	plain, _ := watchGraph(false).Compile()
	assert.NotContains(t, plain.Mermaid(), "watches")
}
