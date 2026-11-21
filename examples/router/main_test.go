package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graphweb"
)

// Every task reaches its workflow; the unsure one is routed by the person;
// the flaky one fails inside the Coding subgraph.
func TestRouterAuto(t *testing.T) {
	srv, err := graphweb.New()
	require.NoError(t, err)
	out := runAll(context.Background(), srv, time.Millisecond, true)
	require.Len(t, out, len(tasks))
	want := []string{"coding", "research", "marketing", "answer", "coding", "research", "marketing", "marketing"}
	for i, line := range out {
		assert.Contains(t, line, " "+want[i]+" ", tasks[i])
	}
	assert.Contains(t, out[4], `failed: graph "support-router": node "coding/implement": test run timed out`)
	assert.Contains(t, out[0], "planned the change, implemented, review round 1, implemented, review round 2, replied")
	assert.True(t, strings.HasSuffix(out[3], "answered from the FAQ, replied"))
}

func TestRouterTopology(t *testing.T) {
	r, err := build(time.Millisecond)
	require.NoError(t, err)
	topo := r.Topology()
	var groups []string
	for _, g := range topo.Groups {
		groups = append(groups, g.Label)
	}
	assert.Equal(t, []string{"Coding", "Research", "Marketing"}, groups)
}
