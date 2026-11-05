package graph

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type artObs struct {
	mu   sync.Mutex
	got  []Artifact
	info []StepInfo
}

func (*artObs) NodeStart(context.Context, StepInfo)      {}
func (*artObs) NodeEnd(context.Context, StepInfo, error) {}
func (o *artObs) Artifact(_ context.Context, i StepInfo, a Artifact) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got, o.info = append(o.got, a), append(o.info, i)
}

type plainObs struct{}

func (plainObs) NodeStart(context.Context, StepInfo)      {}
func (plainObs) NodeEnd(context.Context, StepInfo, error) {}

func TestEmitArtifact(t *testing.T) {
	g := New[int]("art").
		AddNode("a", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Name: "one", Path: "/tmp/one.png", Caption: "c"})
			EmitArtifact(ctx, Artifact{Path: "rel/two.txt"})
			return nil
		}).
		AddNode("b1", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Name: "br", Path: "/tmp/br.png"})
			return nil
		}).
		AddNode("b2", func(context.Context, *int) error { return nil }).
		AddParallel("par", []string{"b1", "b2"}, func(*int, []int) error { return nil }).
		AddEdge(Start, "a").AddEdge("a", "par").AddEdge("par", End)
	obs := &artObs{}
	r, err := g.Compile(WithObserver(obs))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), 0)
	require.NoError(t, err)

	require.Len(t, obs.got, 3)
	assert.Equal(t, Artifact{Name: "one", Path: "/tmp/one.png", Caption: "c"}, obs.got[0])
	assert.Equal(t, "a", obs.info[0].Node)
	assert.NotEmpty(t, obs.info[0].RunID)
	assert.Equal(t, "two.txt", obs.got[1].Name)
	assert.True(t, filepath.IsAbs(obs.got[1].Path))
	assert.Equal(t, "b1", obs.info[2].Node)
	assert.Equal(t, "par", obs.info[2].Parent)
}

func TestEmitArtifactNoop(t *testing.T) {
	EmitArtifact(context.Background(), Artifact{Path: "/x"}) // outside a run

	g := New[int]("plain").
		AddNode("a", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Path: "/x"})
			return nil
		}).AddEdge(Start, "a").AddEdge("a", End)
	for _, opts := range [][]CompileOption{nil, {WithObserver(plainObs{})}} {
		r, err := g.Compile(opts...)
		require.NoError(t, err)
		_, _, err = r.Run(context.Background(), 0)
		require.NoError(t, err)
	}
}
