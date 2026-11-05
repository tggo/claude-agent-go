package graphweb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

func storeRun(t *testing.T, store graph.CheckpointStore, ask, fail bool) (string, *graph.Runnable[st]) {
	t.Helper()
	r, err := graph.New[st]("durable").
		AddNode("inc", graph.Func(func(x *st) error { x.N++; return nil })).
		AddNode("q", func(ctx context.Context, x *st) error {
			if a, ok := graph.Answer(ctx); ok {
				x.Asked = a
				return nil
			}
			if ask {
				x.Note = "partial"
				return graph.Interrupt("which?")
			}
			if fail {
				return errors.New("boom")
			}
			return nil
		}).
		AddConditionalEdge("inc", func(x *st) string {
			if x.N < 2 {
				return "inc"
			}
			return "q"
		}, "inc", "q").
		AddEdge(graph.Start, "inc").AddEdge("q", graph.End).
		Compile(graph.WithCheckpointStore(store))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	l, lerr := store.List(context.Background())
	require.NoError(t, lerr)
	if !ask && !fail {
		require.NoError(t, err)
	}
	return l[0].RunID, r
}

func TestReplayStoreDone(t *testing.T) {
	store := graph.MemStore()
	id, _ := storeRun(t, store, false, false)
	srv, err := NewReplayStore(context.Background(), store, "")
	require.NoError(t, err)
	evs := srv.Events()
	var types []string
	for i, e := range evs {
		assert.Equal(t, int64(i+1), e.Seq)
		assert.Equal(t, id, e.Run)
		assert.Equal(t, SchemaVersion, e.V)
		types = append(types, e.Type)
	}
	assert.Equal(t, []string{TypeGraphStart, TypeEdge, // start -> inc
		TypeNodeStart, TypeNodeEnd, TypeEdge, // inc 1 -> inc
		TypeNodeStart, TypeNodeEnd, TypeEdge, // inc 2 -> q
		TypeNodeStart, TypeNodeEnd, TypeEdge, // q -> end
		TypeGraphEnd}, types)
	assert.NotNil(t, evs[0].Topology)
	// the second edge inc -> inc is the loop back-edge, taken once
	assert.True(t, evs[4].Back)
	assert.Equal(t, 1, evs[4].Iter)
	assert.Equal(t, 1, evs[3].Attempt)
	assert.Contains(t, evs[3].Diff, "n")
	assert.Equal(t, "ok", evs[len(evs)-1].Status)
	assert.Equal(t, []string{"inc", "inc", "q"}, evs[len(evs)-1].Path)
	assert.Equal(t, id, srv.latest)
}

func TestReplayStorePausedFailedCrashed(t *testing.T) {
	ctx := context.Background()
	store := graph.MemStore()
	id, r := storeRun(t, store, true, false)
	srv, err := NewReplayStore(ctx, store, id)
	require.NoError(t, err)
	evs := srv.Events()
	last := evs[len(evs)-1]
	assert.Equal(t, TypeInterrupt, last.Type)
	assert.Equal(t, "which?", last.Question)
	assert.True(t, evs[len(evs)-2].Paused)
	assert.Contains(t, evs[len(evs)-2].Diff, "note")

	// answer it: resume + the rest
	intr, err := func() (*graph.Interrupted[st], error) {
		_, _, err := r.Recover(ctx, id)
		i, _ := graph.AsInterrupt[st](err)
		return i, err
	}()
	require.ErrorIs(t, err, graph.ErrInterrupt)
	_, _, err = r.Resume(ctx, intr.Checkpoint, "left")
	require.NoError(t, err)
	srv, err = NewReplayStore(ctx, store, id)
	require.NoError(t, err)
	var kinds []string
	for _, e := range srv.Events() {
		kinds = append(kinds, e.Type)
	}
	assert.Contains(t, kinds, TypeResume)
	assert.Equal(t, TypeGraphEnd, kinds[len(kinds)-1])

	// failed
	store2 := graph.MemStore()
	id2, _ := storeRun(t, store2, false, true)
	srv, err = NewReplayStore(ctx, store2, id2)
	require.NoError(t, err)
	evs = srv.Events()
	assert.Equal(t, "error", evs[len(evs)-1].Status)
	assert.Contains(t, evs[len(evs)-1].Err, "boom")

	// crashed: log cut after a node_start; fork markers are skipped
	recs, _ := store2.Load(ctx, id2)
	cut := 0
	for i, rc := range recs {
		if rc.Kind == graph.KindNodeStart {
			cut = i + 1
		}
	}
	evs, err = EventsFromRecords("x", append(append([]graph.Record(nil), recs[:cut]...), graph.Record{Kind: graph.KindFork, State: recs[0].State}, graph.Record{Kind: graph.KindRecover}))
	require.NoError(t, err)
	assert.Equal(t, TypeNodeStart, evs[len(evs)-1].Type, "a crashed run ends inside its last node")
}

func TestReplayStoreErrors(t *testing.T) {
	ctx := context.Background()
	_, err := NewReplayStore(ctx, graph.MemStore(), "")
	assert.ErrorContains(t, err, "no runs")
	_, err = NewReplayStore(ctx, graph.MemStore(), "nope")
	assert.ErrorContains(t, err, "not found")
	_, err = NewReplayStore(ctx, errStore{}, "")
	assert.Error(t, err)
	_, err = NewReplayStore(ctx, errStore{}, "x")
	assert.Error(t, err)
	_, err = EventsFromRecords("x", nil)
	assert.Error(t, err)
	_, err = EventsFromRecords("x", []graph.Record{{Kind: graph.KindNodeCommit}})
	assert.Error(t, err)
	// a run_start without topology still replays
	evs, err := EventsFromRecords("x", []graph.Record{{Kind: graph.KindRunStart, Next: "a"}})
	require.NoError(t, err)
	assert.Nil(t, evs[0].Topology)
	// a store whose run fails the id check
	store := graph.MemStore()
	require.NoError(t, store.Append(ctx, "bad", graph.Record{Kind: graph.KindNodeCommit}))
	_, err = NewReplayStore(ctx, store, "bad")
	assert.Error(t, err)
}

type errStore struct{}

func (errStore) Append(context.Context, string, graph.Record) error   { return errors.New("x") }
func (errStore) Load(context.Context, string) ([]graph.Record, error) { return nil, errors.New("x") }
func (errStore) List(context.Context) ([]graph.RunSummary, error)     { return nil, errors.New("x") }
