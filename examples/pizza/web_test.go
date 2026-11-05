package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

// The graph the web UI draws: the order node is an agent, the confirm loop is
// a labelled back-edge.
func TestTopologyForWebUI(t *testing.T) {
	srv, err := graphweb.New(graphweb.WithJournal(filepath.Join(t.TempDir(), graphweb.DefaultJournal)))
	require.NoError(t, err)
	defer srv.Close()
	a, err := Build(Options{Model: "haiku", RunsDir: t.TempDir(), Observer: srv})
	require.NoError(t, err)
	defer a.Close()
	top := a.Graph.Topology()
	kinds := map[string]string{}
	for _, n := range top.Nodes {
		kinds[n.ID] = n.Kind
	}
	assert.Equal(t, graph.KindAgent, kinds["order"])
	assert.Equal(t, graph.KindFunc, kinds["confirm"])
	var back, labelled int
	for _, e := range top.Edges {
		if e.Back {
			back++
			assert.Equal(t, "confirm", e.From)
			assert.Equal(t, "order", e.To)
		}
		if e.From == "confirm" && e.Label != "" {
			labelled++
		}
	}
	assert.Equal(t, 1, back)
	assert.Equal(t, 3, labelled)
}
