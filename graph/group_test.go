package graph

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nop(_ context.Context, _ *struct{}) error { return nil }

func chain() *Graph[struct{}] {
	return New[struct{}]("g").
		AddNode("a", nop).AddNode("b", nop).AddNode("c", nop).AddNode("d", nop).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", "c").AddEdge("c", "d").AddEdge("d", End)
}

func TestGroupTopology(t *testing.T) {
	r, err := chain().Group("draft", "a", "b").GroupWith("ship", GroupOpts{Label: "Ship it", Collapsed: true}, "d").Compile()
	require.NoError(t, err)
	topo := r.Topology()
	assert.Equal(t, []TopologyGroup{
		{ID: "draft", Label: "draft", Nodes: []string{"a", "b"}},
		{ID: "ship", Label: "Ship it", Nodes: []string{"d"}, Collapsed: true},
	}, topo.Groups)
	got := map[string]string{}
	for _, n := range topo.Nodes {
		got[n.ID] = n.Group
	}
	assert.Equal(t, map[string]string{Start: "", "a": "draft", "b": "draft", "c": "", "d": "ship", End: ""}, got)

	raw, err := json.Marshal(topo)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"groups":[{"id":"draft","label":"draft","nodes":["a","b"]},{"id":"ship","label":"Ship it","nodes":["d"],"collapsed":true}]`)
	assert.Contains(t, string(raw), `"group":"draft"`)
}

func TestNoGroupsOmitted(t *testing.T) {
	r, err := chain().Compile()
	require.NoError(t, err)
	raw, _ := json.Marshal(r.Topology())
	assert.NotContains(t, string(raw), `"group`)
}

func TestGroupErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		g    *Graph[struct{}]
		want string
	}{
		"unknown":   {chain().Group("x", "a", "zzz"), `group "x": unknown node "zzz"`},
		"two":       {chain().Group("x", "a").Group("y", "a"), `node "a": in two groups ("x" and "y")`},
		"dup node":  {chain().Group("x", "a", "a"), `duplicate node "a"`},
		"dup group": {chain().Group("x", "a").Group("x", "b"), `group "x": duplicate name`},
		"empty":     {chain().Group("x"), `group "x": no nodes`},
		"noname":    {chain().Group("", "a"), `group with empty name`},
		"reserved":  {chain().Group("x", Start), `reserved`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.g.Compile()
			require.ErrorIs(t, err, ErrInvalidGraph)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestGroupDeclaredBeforeNodes(t *testing.T) {
	g := New[struct{}]("g").Group("s", "a", "b").
		AddNode("a", nop).AddNode("b", nop).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End)
	r, err := g.Compile()
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, r.Topology().Groups[0].Nodes)
}

func TestGroupParallelBranchesFollow(t *testing.T) {
	g := New[struct{}]("g").
		AddNode("x", nop).AddNode("y", nop).
		AddParallel("fan", []string{"x", "y"}, func(*struct{}, []struct{}) error { return nil }).
		AddEdge(Start, "fan").AddEdge("fan", End).
		Group("par", "fan")
	r, err := g.Compile()
	require.NoError(t, err)
	assert.Equal(t, []string{"x", "y", "fan"}, r.Topology().Groups[0].Nodes)
	for _, n := range r.Topology().Nodes {
		if n.ID == "x" || n.ID == "y" || n.ID == "fan" {
			assert.Equal(t, "par", n.Group, n.ID)
		}
	}
}

func TestGroupMermaid(t *testing.T) {
	r, err := chain().GroupWith("one", GroupOpts{Label: `A "B"`}, "a", "b").Compile()
	require.NoError(t, err)
	m := r.Mermaid()
	assert.Contains(t, m, "    c[\"c\"]\n")
	assert.Contains(t, m, "    subgraph grp_one[\"A #quot;B#quot;\"]\n        a[\"a\"]\n        b[\"b\"]\n    end\n")
	assert.Contains(t, m, "a --> b")
}
