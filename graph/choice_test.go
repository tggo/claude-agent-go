package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type choiceObs struct{ opts []string }

func (o *choiceObs) NodeStart(context.Context, StepInfo) {}

func (o *choiceObs) NodeEnd(_ context.Context, i StepInfo, _ error) {
	if i.Paused {
		o.opts = i.Options
	}
}

// InterruptChoice carries its options to Interrupted, the observer, Step and
// a durable run's log (so Recover offers them again).
func TestInterruptChoice(t *testing.T) {
	build := func(opts ...CompileOption) *Runnable[st] {
		r, err := New[st]("pick").
			AddNode("ask", func(ctx context.Context, s *st) error {
				if a, ok := Answer(ctx); ok {
					s.Text = a
					return nil
				}
				return InterruptChoice("which?", "red", "blue")
			}).AddEdge(Start, "ask").AddEdge("ask", End).Compile(opts...)
		require.NoError(t, err)
		return r
	}
	obs := &choiceObs{}
	store := MemStore()
	r := build(WithObserver(obs), WithCheckpointStore(store))
	_, _, err := r.Run(context.Background(), st{})
	ie, ok := AsInterrupt[st](err)
	require.True(t, ok)
	assert.Equal(t, []string{"red", "blue"}, ie.Options)
	assert.Equal(t, []string{"red", "blue"}, obs.opts)

	runs, err := store.List(context.Background())
	require.NoError(t, err)
	_, _, err = r.Recover(context.Background(), runs[0].RunID)
	ie2, ok := AsInterrupt[st](err)
	require.True(t, ok)
	assert.Equal(t, []string{"red", "blue"}, ie2.Options, "recovered from the log")

	s, _, err := r.Resume(context.Background(), ie2.Checkpoint, "blue")
	require.NoError(t, err)
	assert.Equal(t, "blue", s.Text)

	res, err := build().Step(context.Background(), "ask", st{}, StepOptions{})
	require.NoError(t, err)
	assert.True(t, res.Paused)
	assert.Equal(t, []string{"red", "blue"}, res.Options)
}
