package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

// The review -> fix loop shows up as a back-edge and every agent node is tagged.
func TestTopologyForWebUI(t *testing.T) {
	p, err := Build("haiku", t.TempDir())
	require.NoError(t, err)
	defer p.Close()
	top := p.Graph.Topology()
	for _, n := range top.Nodes {
		switch n.ID {
		case "plan", "code", "review", "fix", "docs":
			assert.Equal(t, graph.KindAgent, n.Kind, n.ID)
		}
	}
	var back []string
	for _, e := range top.Edges {
		if e.Back {
			back = append(back, e.From+">"+e.To)
		}
	}
	assert.Equal(t, []string{"fix>review"}, back)
}
