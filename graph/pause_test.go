package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pauseGraph is pre -> work -> post. pre runs hook (if set) so a test can
// request a pause from inside the run, the way another process would while a
// node is busy. work is what a fix replaces: workFn.
func pauseGraph(t *testing.T, c *durCounter, hook func(ctx context.Context), workFn NodeFunc[durState], opts ...CompileOption) *Runnable[durState] {
	t.Helper()
	r, err := New[durState]("pausable").
		AddNode("pre", func(ctx context.Context, s *durState) error {
			c.hit("pre")
			if hook != nil {
				hook(ctx)
			}
			s.Trail = append(s.Trail, "pre")
			return nil
		}).
		AddNode("work", func(ctx context.Context, s *durState) error { c.hit("work"); return workFn(ctx, s) }).
		AddNode("post", func(_ context.Context, s *durState) error {
			c.hit("post")
			s.Trail = append(s.Trail, "post")
			return nil
		}).
		AddEdge(Start, "pre").AddEdge("pre", "work").AddEdge("work", "post").AddEdge("post", End).
		Compile(opts...)
	require.NoError(t, err)
	return r
}

func workOK(_ context.Context, s *durState) error { s.Trail = append(s.Trail, "work"); return nil }

// A pause requested while a node runs takes effect after that node commits:
// the run stops before the next node, logs a pause record and no run_end, and
// returns ErrPaused. Recover clears the request and continues there; the
// nodes before the pause do not run again.
func TestPauseBetweenNodesThenRecover(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := FileStore(dir)
	require.NoError(t, err)
	sig := FilePause{Dir: dir}
	c := &durCounter{}
	request := func(ctx context.Context) { require.NoError(t, sig.Request(RunID(ctx))) }
	r := pauseGraph(t, c, request, workOK, WithCheckpointStore(st), WithPauseSignal(sig))

	out, rep, err := r.Run(ctx, durState{})
	require.ErrorIs(t, err, ErrPaused)
	assert.NotErrorIs(t, err, ErrInterrupt)
	var pe *PausedError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "work", pe.Node)
	assert.Equal(t, 1, pe.Step)
	assert.Equal(t, []string{"pre"}, out.Trail)
	assert.Equal(t, []string{"pre"}, rep.Path)
	assert.Zero(t, c.get("work"), "the node after the pause did not start")

	id := durOnlyRun(t, st)
	assert.Equal(t, id, pe.RunID)
	recs, err := st.Load(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{KindRunStart, KindNodeStart, KindNodeCommit, KindEdge, KindPause}, durKinds(recs))
	last := recs[len(recs)-1]
	assert.Equal(t, "work", last.Node)
	assert.Equal(t, "work", last.Next)
	assert.JSONEq(t, `{"trail":["pre"],"n":0}`, string(last.State))

	l, err := st.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, l[0].Status)
	assert.Empty(t, l[0].Question)
	assert.Equal(t, "work", l[0].Next)

	// Still requested: the pause file is there until Recover.
	requested, err := sig.PauseRequested(ctx, id)
	require.NoError(t, err)
	assert.True(t, requested)

	r2 := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig))
	out, rep, err = r2.Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "post"}, out.Trail)
	assert.Equal(t, []string{"pre", "work", "post"}, rep.Path)
	assert.Equal(t, 1, c.get("pre"))
	assert.Equal(t, 1, c.get("work"))
	assert.Equal(t, 1, c.get("post"))
	requested, err = sig.PauseRequested(ctx, id)
	require.NoError(t, err)
	assert.False(t, requested, "Recover cleared the pause")
	l, _ = st.List(ctx)
	assert.Equal(t, StatusDone, l[0].Status)
}

// The debugging loop: a run is paused before a node that is known to be
// broken, someone inspects the logged state and ships a fixed node function,
// and the new build continues the same run.
func TestPauseThenRecoverWithFixedNode(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{}
	c := &durCounter{}
	buggy := func(_ context.Context, s *durState) error { return errors.New("bug: work divides by zero") }
	request := func(ctx context.Context) { sig.set(RunID(ctx), true) }

	_, _, err := pauseGraph(t, c, request, buggy, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrPaused)
	assert.Zero(t, c.get("work"), "the buggy node never ran")
	id := durOnlyRun(t, st)

	fixed := func(_ context.Context, s *durState) error { s.Trail = append(s.Trail, "work-fixed"); return nil }
	out, _, err := pauseGraph(t, c, nil, fixed, WithCheckpointStore(st), WithPauseSignal(sig)).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work-fixed", "post"}, out.Trail)
	assert.Equal(t, 1, c.get("pre"))
	assert.Equal(t, 1, sig.cleared, "cleared once")
}

// A fix that changes the graph's shape (a new node) is refused unless the new
// build opts in with AllowTopologyChange; then the new node runs.
func TestPauseThenRecoverWithChangedTopology(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{}
	c := &durCounter{}
	request := func(ctx context.Context) { sig.set(RunID(ctx), true) }
	_, _, err := pauseGraph(t, c, request, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrPaused)
	id := durOnlyRun(t, st)

	withCheck := func(opts ...CompileOption) *Runnable[durState] {
		r, err := New[durState]("pausable").
			AddNode("pre", func(_ context.Context, s *durState) error { c.hit("pre"); return nil }).
			AddNode("work", func(_ context.Context, s *durState) error { s.Trail = append(s.Trail, "work"); return nil }).
			AddNode("check", func(_ context.Context, s *durState) error { s.Trail = append(s.Trail, "check"); return nil }).
			AddNode("post", func(_ context.Context, s *durState) error { s.Trail = append(s.Trail, "post"); return nil }).
			AddEdge(Start, "pre").AddEdge("pre", "work").AddEdge("work", "check").AddEdge("check", "post").AddEdge("post", End).
			Compile(append([]CompileOption{WithCheckpointStore(st), WithPauseSignal(sig)}, opts...)...)
		require.NoError(t, err)
		return r
	}
	_, _, err = withCheck().Recover(ctx, id)
	require.ErrorIs(t, err, ErrTopologyChanged)
	assert.True(t, sig.get(id), "a refused Recover leaves the pause in place")

	out, _, err := withCheck(AllowTopologyChange()).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "check", "post"}, out.Trail)
	assert.Equal(t, 1, c.get("pre"))
}

// A pause requested before the run starts stops it before its first node;
// the run_start is logged, so it can be recovered.
func TestPauseBeforeFirstNode(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{all: true}
	c := &durCounter{}
	_, rep, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	var pe *PausedError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "pre", pe.Node)
	assert.Zero(t, pe.Step)
	assert.Empty(t, rep.Path)
	recs, _ := st.Load(ctx, durOnlyRun(t, st))
	assert.Equal(t, []string{KindRunStart, KindPause}, durKinds(recs))

	sig.all = false
	out, _, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Recover(ctx, pe.RunID)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "post"}, out.Trail)
}

// A pause and its Recover can repeat: each Recover continues from the latest
// pause, and Recover without a pause signal configured continues too.
func TestPauseTwiceAndRecoverWithoutSignal(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{}
	c := &durCounter{}
	request := func(ctx context.Context) { sig.set(RunID(ctx), true) }
	pauseInWork := func(ctx context.Context, s *durState) error {
		sig.set(RunID(ctx), true)
		s.Trail = append(s.Trail, "work")
		return nil
	}
	_, _, err := pauseGraph(t, c, request, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrPaused)
	id := durOnlyRun(t, st)

	_, _, err = pauseGraph(t, c, nil, pauseInWork, WithCheckpointStore(st), WithPauseSignal(sig)).Recover(ctx, id)
	var pe *PausedError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "post", pe.Node)
	assert.Equal(t, 2, pe.Step)

	out, rep, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st)).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "post"}, out.Trail)
	assert.Equal(t, []string{"pre", "work", "post"}, rep.Path)
	assert.Equal(t, 1, c.get("work"))
	assert.True(t, sig.get(id), "without the signal configured nothing clears it")
}

// The node re-run by Resume with an answer is not paused before (it is the
// answer's node); a pause still stops the run after it.
func TestPauseSkippedForAnsweredNode(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{}
	c := &durCounter{}
	r := durAskGraph(t, st, c)
	r.cfg.pause = sig
	_, _, err := r.Run(ctx, durState{})
	ie, ok := AsInterrupt[durState](err)
	require.True(t, ok)

	sig.all = true
	_, _, err = r.Resume(ctx, ie.Checkpoint, "Ann")
	var pe *PausedError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "post", pe.Node)
	assert.Equal(t, 2, c.get("ask"), "ask ran again with the answer")
	assert.Zero(t, c.get("post"))
}

func TestPauseSignalErrorFailsRun(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	boom := errors.New("signal store down")
	sig := &fakePause{err: boom}
	c := &durCounter{}
	_, _, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, ErrPaused)
	l, _ := st.List(ctx)
	assert.Equal(t, StatusFailed, l[0].Status)

	sig.err = nil
	out, _, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Recover(ctx, l[0].RunID)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "post"}, out.Trail)
}

func TestPauseRecordFailureFailsRun(t *testing.T) {
	ctx := context.Background()
	inner := MemStore()
	st := &durDyingStore{CheckpointStore: inner, limit: 1} // run_start only
	_, _, err := pauseGraph(t, &durCounter{}, nil, workOK, WithCheckpointStore(st), WithPauseSignal(&fakePause{all: true})).Run(ctx, durState{})
	require.ErrorIs(t, err, durErrDead)
}

func TestPauseClearFailureStopsRecover(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	sig := &fakePause{all: true}
	c := &durCounter{}
	_, _, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrPaused)
	sig.clearErr = errors.New("read-only")
	_, _, err = pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(sig)).Recover(ctx, durOnlyRun(t, st))
	require.ErrorContains(t, err, "clear pause: read-only")
	assert.Zero(t, c.get("pre"))
}

func TestPauseSignalNeedsStore(t *testing.T) {
	_, err := New[durState]("x").AddNode("a", Func(func(*durState) error { return nil })).
		AddEdge(Start, "a").AddEdge("a", End).Compile(WithPauseSignal(&fakePause{}))
	require.ErrorContains(t, err, "WithPauseSignal: needs WithCheckpointStore")
}

func TestFilePause(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sub")
	f := FilePause{Dir: dir}
	ok, err := f.PauseRequested(ctx, "r1")
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, f.ClearPause(ctx, "r1"), "clearing nothing is fine")
	require.NoError(t, f.Request("r1"))
	_, err = os.Stat(filepath.Join(dir, "r1.pause"))
	require.NoError(t, err)
	ok, _ = f.PauseRequested(ctx, "r1")
	assert.True(t, ok)
	require.NoError(t, f.ClearPause(ctx, "r1"))
	ok, _ = f.PauseRequested(ctx, "r1")
	assert.False(t, ok)

	for _, bad := range []string{"../x", "", "a/b"} {
		assert.Error(t, f.Request(bad))
		_, err := f.PauseRequested(ctx, bad)
		assert.Error(t, err)
		assert.Error(t, f.ClearPause(ctx, bad))
	}

	// A pause file next to a FileStore's logs is not mistaken for a run.
	st, err := FileStore(dir)
	require.NoError(t, err)
	require.NoError(t, f.Request("r2"))
	l, err := st.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, l)

	// Stat errors other than not-exist are reported: a file where Dir should be.
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))
	_, err = FilePause{Dir: notDir}.PauseRequested(ctx, "r1")
	assert.Error(t, err)
	assert.Error(t, FilePause{Dir: notDir}.Request("r1"))
}

// MemPause pauses a run from another goroutine of the same process.
func TestMemPause(t *testing.T) {
	ctx := context.Background()
	var sig MemPause // zero value is usable
	ok, err := sig.PauseRequested(ctx, "r")
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, sig.ClearPause(ctx, "r"))

	st := MemStore()
	c := &durCounter{}
	started, release := make(chan string), make(chan struct{})
	hook := func(ctx context.Context) { started <- RunID(ctx); <-release }
	r := pauseGraph(t, c, hook, workOK, WithCheckpointStore(st), WithPauseSignal(&sig))
	done := make(chan error, 1)
	go func() { _, _, err := r.Run(ctx, durState{}); done <- err }()
	id := <-started
	sig.Request(id) // while pre runs
	close(release)
	require.ErrorIs(t, <-done, ErrPaused)
	ok, _ = sig.PauseRequested(ctx, id)
	assert.True(t, ok)

	out, _, err := pauseGraph(t, c, nil, workOK, WithCheckpointStore(st), WithPauseSignal(&sig)).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"pre", "work", "post"}, out.Trail)
	ok, _ = sig.PauseRequested(ctx, id)
	assert.False(t, ok, "Recover cleared it")
}

func TestPausedErrorMessage(t *testing.T) {
	err := error(&PausedError{RunID: "r", Node: "n"})
	assert.Equal(t, `graph: run "r" paused before node "n"`, err.Error())
	assert.ErrorIs(t, err, ErrPaused)
}

// fakePause is a PauseSignal for the error paths: per-run flags, all runs at
// once, injected errors, and a count of clears.
type fakePause struct {
	runs     map[string]bool
	all      bool
	err      error
	clearErr error
	cleared  int
}

func (m *fakePause) set(id string, v bool) {
	if m.runs == nil {
		m.runs = map[string]bool{}
	}
	m.runs[id] = v
}

func (m *fakePause) get(id string) bool { return m.all || m.runs[id] }

func (m *fakePause) PauseRequested(_ context.Context, id string) (bool, error) {
	return m.get(id), m.err
}

func (m *fakePause) ClearPause(_ context.Context, id string) error {
	if m.clearErr != nil {
		return m.clearErr
	}
	m.cleared++
	m.set(id, false)
	return nil
}
