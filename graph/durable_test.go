package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/runner"
)

type durState struct {
	Trail []string `json:"trail"`
	N     int      `json:"n"`
	Note  string   `json:"note,omitempty"`
}

var durErrDead = errors.New("process died")

// durDyingStore simulates SIGKILL at an exact point of the log: the first limit
// appends persist, every later one is lost and fails.
type durDyingStore struct {
	CheckpointStore
	mu    sync.Mutex
	limit int
	n     int
}

func (d *durDyingStore) Append(ctx context.Context, id string, r Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n >= d.limit {
		return durErrDead
	}
	d.n++
	return d.CheckpointStore.Append(ctx, id, r)
}

type durCounter struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *durCounter) hit(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[name]++
}

func (c *durCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[name]
}

// chain builds n1 -> n2 -> ... -> n10; every node counts its executions, adds
// $1 of cost and appends its name to the state.
func durChain(t *testing.T, c *durCounter, size int, opts ...CompileOption) *Runnable[durState] {
	t.Helper()
	g := New[durState]("chain")
	prev := Start
	for i := 1; i <= size; i++ {
		name := fmt.Sprintf("n%d", i)
		g.AddNode(name, func(ctx context.Context, s *durState) error {
			c.hit(name)
			AddCost(ctx, 1)
			s.Trail = append(s.Trail, name)
			s.N++
			return nil
		})
		g.AddEdge(prev, name)
		prev = name
	}
	g.AddEdge(prev, End)
	r, err := g.Compile(opts...)
	require.NoError(t, err)
	return r
}

func durOnlyRun(t *testing.T, st CheckpointStore) string {
	t.Helper()
	l, err := st.List(context.Background())
	require.NoError(t, err)
	require.Len(t, l, 1)
	return l[0].RunID
}

func durKinds(recs []Record) []string {
	var k []string
	for _, r := range recs {
		k = append(k, r.Kind)
	}
	return k
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs, err := FileStore(filepath.Join(t.TempDir(), "runs"))
	require.NoError(t, err)
	for name, st := range map[string]CheckpointStore{"mem": MemStore(), "file": fs} {
		t.Run(name, func(t *testing.T) {
			recs, err := st.Load(ctx, "nope")
			require.NoError(t, err)
			assert.Empty(t, recs)
			now := time.Now().UTC().Truncate(time.Millisecond)
			in := []Record{
				{V: 1, Seq: 1, Time: now, Kind: KindRunStart, Graph: "g", State: []byte(`{"a":1}`), Next: "x"},
				{V: 1, Seq: 2, Time: now, Kind: KindNodeCommit, Node: "x", Step: 1, State: []byte(`{"a":2}`), Next: End, CostUSD: 0.5},
				{V: 1, Seq: 3, Time: now, Kind: KindRunEnd},
			}
			for _, r := range in {
				require.NoError(t, st.Append(ctx, "r1", r))
			}
			require.NoError(t, st.Append(ctx, "r2", Record{V: 1, Seq: 1, Time: now.Add(-time.Hour), Kind: KindRunStart, Next: "x"}))
			out, err := st.Load(ctx, "r1")
			require.NoError(t, err)
			require.Len(t, out, 3)
			for i := range in {
				assert.Equal(t, in[i].Kind, out[i].Kind)
				assert.Equal(t, in[i].Seq, out[i].Seq)
				assert.Equal(t, string(in[i].State), string(out[i].State))
			}
			l, err := st.List(ctx)
			require.NoError(t, err)
			require.Len(t, l, 2)
			assert.Equal(t, "r1", l[0].RunID, "most recently updated first")
			assert.Equal(t, StatusDone, l[0].Status)
			assert.Equal(t, 0.5, l[0].CostUSD)
			assert.Equal(t, StatusCrashed, l[1].Status, "old log without run_end")
		})
	}
}

func TestFileStoreFilesAndIDs(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "runs")
	st, err := FileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Append(ctx, "run-1", Record{Seq: 1, Kind: KindRunStart}))
	di, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	fi, err := os.Stat(filepath.Join(dir, "run-1.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "a b"} {
		assert.Error(t, st.Append(ctx, bad, Record{Kind: KindRunStart}), bad)
		_, err := st.Load(ctx, bad)
		assert.Error(t, err, bad)
	}
	_, err = FileStore("")
	assert.Error(t, err)
	_, err = FileStore(filepath.Join(dir, "run-1.jsonl", "sub"))
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d.jsonl"), 0o700))
	l, err := st.List(ctx)
	require.NoError(t, err)
	assert.Len(t, l, 1, "only run logs are listed")
}

func TestFileStoreTornTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := FileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Append(ctx, "r", Record{Seq: 1, Kind: KindRunStart, Next: "a"}))
	require.NoError(t, st.Append(ctx, "r", Record{Seq: 2, Kind: KindNodeCommit, Node: "a", Next: End}))
	p := filepath.Join(dir, "r.jsonl")

	// a crash mid-write: a half line without newline
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"v":1,"seq":3,"kind":"node_co`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	recs, err := st.Load(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, []string{KindRunStart, KindNodeCommit}, durKinds(recs), "torn tail ignored")

	// the next append cuts the torn tail instead of gluing onto it
	require.NoError(t, st.Append(ctx, "r", Record{Seq: 3, Kind: KindRunEnd}))
	recs, err = st.Load(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, []string{KindRunStart, KindNodeCommit, KindRunEnd}, durKinds(recs))

	// a torn tail longer than the scan buffer, and a file that is only a torn line
	f, err = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"state":"` + strings.Repeat("x", 200<<10))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, st.Append(ctx, "r", Record{Seq: 4, Kind: KindRecover}))
	recs, err = st.Load(ctx, "r")
	require.NoError(t, err)
	assert.Len(t, recs, 4)

	// a whole record that lost only its newline was already readable: kept
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nl.jsonl"),
		[]byte(`{"v":1,"seq":1,"kind":"run_start","next":"a"}`), 0o600))
	recs, err = st.Load(ctx, "nl")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NoError(t, st.Append(ctx, "nl", Record{Seq: 2, Kind: KindRunEnd}))
	recs, err = st.Load(ctx, "nl")
	require.NoError(t, err)
	assert.Equal(t, []string{KindRunStart, KindRunEnd}, durKinds(recs))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "only.jsonl"), []byte(`{"kind":`), 0o600))
	require.NoError(t, st.Append(ctx, "only", Record{Seq: 1, Kind: KindRunStart}))
	recs, err = st.Load(ctx, "only")
	require.NoError(t, err)
	assert.Len(t, recs, 1)

	// garbage in the middle is corruption, not a torn write
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.jsonl"), []byte("{\"kind\":\"run_start\"}\nnot json\n{\"kind\":\"run_end\"}\n"), 0o600))
	_, err = st.Load(ctx, "bad")
	assert.ErrorContains(t, err, "corrupt")
	l, err := st.List(ctx)
	require.NoError(t, err)
	var seen bool
	for _, s := range l {
		if s.RunID == "bad" {
			seen = true
			assert.Equal(t, "corrupt", s.Status)
		}
	}
	assert.True(t, seen)
}

func TestLogShape(t *testing.T) {
	st := MemStore()
	c := &durCounter{}
	out, rep, err := durChain(t, c, 2, WithCheckpointStore(st)).Run(context.Background(), durState{})
	require.NoError(t, err)
	id := durOnlyRun(t, st)
	recs, _ := st.Load(context.Background(), id)
	assert.Equal(t, []string{KindRunStart, KindNodeStart, KindNodeCommit, KindEdge, KindNodeStart, KindNodeCommit, KindEdge, KindRunEnd}, durKinds(recs))
	for i, r := range recs {
		assert.Equal(t, i+1, r.Seq)
		assert.Equal(t, RecordVersion, r.V)
		assert.False(t, r.Time.IsZero())
	}
	assert.Equal(t, "chain", recs[0].Graph)
	assert.NotEmpty(t, recs[0].TopoHash)
	assert.Contains(t, string(recs[0].Topology), `"n1"`)
	assert.Equal(t, "n1", recs[0].Next)
	assert.Equal(t, "n2", recs[2].Next)
	assert.Equal(t, 1.0, recs[2].CostUSD)
	assert.JSONEq(t, `{"trail":["n1","n2"],"n":2}`, string(recs[7].State))
	assert.Equal(t, []string{"n1", "n2"}, out.Trail)
	assert.Equal(t, 2.0, rep.TotalCostUSD)
}

// The commit of node k is in the log before node k+1 starts.
func TestCommitBeforeNextNode(t *testing.T) {
	st := MemStore()
	var bad []string
	g := New[durState]("order")
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("n%d", i)
		g.AddNode(name, func(ctx context.Context, s *durState) error {
			recs, _ := st.Load(ctx, RunID(ctx))
			var commits []string
			for _, r := range recs {
				if r.Kind == KindNodeCommit {
					commits = append(commits, r.Node)
				}
			}
			if len(commits) != s.N {
				bad = append(bad, fmt.Sprintf("%s saw %d commits, state says %d", name, len(commits), s.N))
			}
			s.N++
			return nil
		})
		prev := Start
		if i > 1 {
			prev = fmt.Sprintf("n%d", i-1)
		}
		g.AddEdge(prev, name)
	}
	g.AddEdge("n4", End)
	r, err := g.Compile(WithCheckpointStore(st))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), durState{})
	require.NoError(t, err)
	assert.Empty(t, bad)
}

// Kill the process at every position of the log of a 10-node run; Recover must
// finish with the right state, never re-running a committed node.
func TestRecoverFromEveryPosition(t *testing.T) {
	const size = 10
	total := 1 + 3*size + 1 // run_start, (node_start, commit, edge) x10, run_end
	for limit := 1; limit < total; limit++ {
		t.Run(fmt.Sprintf("dies-after-%d-records", limit), func(t *testing.T) {
			inner := MemStore()
			c := &durCounter{}
			_, _, err := durChain(t, c, size, WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: limit})).Run(context.Background(), durState{})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCheckpoint)
			id := durOnlyRun(t, inner)
			recs, _ := inner.Load(context.Background(), id)
			committed := 0
			for _, r := range recs {
				if r.Kind == KindNodeCommit {
					committed++
				}
			}

			out, rep, err := durChain(t, c, size, WithCheckpointStore(inner)).Recover(context.Background(), id)
			require.NoError(t, err)

			var want []string
			for i := 1; i <= size; i++ {
				want = append(want, fmt.Sprintf("n%d", i))
				n := c.get(fmt.Sprintf("n%d", i))
				if i <= committed {
					assert.Equal(t, 1, n, "n%d was committed and must not run again", i)
				} else {
					assert.Contains(t, []int{1, 2}, n, "n%d", i)
				}
			}
			assert.Equal(t, want, out.Trail)
			assert.Equal(t, size, out.N)
			// cumulative report: restored from the log plus the rest
			assert.Equal(t, want, rep.Path)
			assert.Len(t, rep.Steps, size)
			assert.InDelta(t, float64(size), rep.TotalCostUSD, 1e-9)

			after, _ := inner.Load(context.Background(), id)
			assert.Equal(t, KindRunEnd, after[len(after)-1].Kind)
			assert.Empty(t, after[len(after)-1].Err)
			for i, r := range after {
				assert.Equal(t, i+1, r.Seq, "seq stays dense across the crash")
			}
		})
	}
}

func TestRecoverFinishedDoesNoWork(t *testing.T) {
	st := MemStore()
	c := &durCounter{}
	r := durChain(t, c, 3, WithCheckpointStore(st))
	_, _, err := r.Run(context.Background(), durState{})
	require.NoError(t, err)
	id := durOnlyRun(t, st)
	before, _ := st.Load(context.Background(), id)

	out, rep, err := r.Recover(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1", "n2", "n3"}, out.Trail)
	assert.Len(t, rep.Steps, 3)
	for _, n := range []string{"n1", "n2", "n3"} {
		assert.Equal(t, 1, c.get(n))
	}
	after, _ := st.Load(context.Background(), id)
	assert.Len(t, after, len(before), "nothing is appended")
}

func TestRecoverErrors(t *testing.T) {
	ctx := context.Background()
	c := &durCounter{}
	_, _, err := durChain(t, c, 2).Recover(ctx, "x")
	assert.ErrorContains(t, err, "WithCheckpointStore")
	_, _, err = durChain(t, c, 2).Fork(ctx, "x", 1, "y", nil)
	assert.ErrorContains(t, err, "WithCheckpointStore")

	st := MemStore()
	r := durChain(t, c, 2, WithCheckpointStore(st))
	_, _, err = r.Recover(ctx, "x")
	assert.ErrorIs(t, err, ErrRunNotFound)
	_, _, err = r.Fork(ctx, "x", 1, "y", nil)
	assert.ErrorIs(t, err, ErrRunNotFound)

	// no run_start
	require.NoError(t, st.Append(ctx, "headless", Record{Seq: 1, Kind: KindNodeCommit, Node: "n1", Next: "n2"}))
	_, _, err = r.Recover(ctx, "headless")
	assert.ErrorContains(t, err, "no run_start")

	// state that is not valid for S
	require.NoError(t, st.Append(ctx, "badstate", Record{Seq: 1, Kind: KindRunStart, State: []byte(`{"n":"str"}`), Next: "n1", TopoHash: r.topoHash()}))
	_, _, err = r.Recover(ctx, "badstate")
	assert.ErrorContains(t, err, "decode logged state")
	require.NoError(t, st.Append(ctx, "nostate", Record{Seq: 1, Kind: KindRunStart, Next: "n1", TopoHash: r.topoHash()}))
	_, _, err = r.Recover(ctx, "nostate")
	assert.ErrorContains(t, err, "no state")

	// the store itself failing
	_, _, err = durChain(t, c, 2, WithCheckpointStore(durFailingStore{})).Recover(ctx, "x")
	assert.ErrorIs(t, err, ErrCheckpoint)
	_, _, err = durChain(t, c, 2, WithCheckpointStore(durFailingStore{})).Fork(ctx, "x", 1, "y", nil)
	assert.ErrorIs(t, err, ErrCheckpoint)
}

type durFailingStore struct{}

func (durFailingStore) Append(context.Context, string, Record) error { return errors.New("disk full") }
func (durFailingStore) Load(context.Context, string) ([]Record, error) {
	return nil, errors.New("disk gone")
}
func (durFailingStore) List(context.Context) ([]RunSummary, error) {
	return nil, errors.New("disk gone")
}

func TestStoreFailureStopsRun(t *testing.T) {
	c := &durCounter{}
	_, _, err := durChain(t, c, 3, WithCheckpointStore(durFailingStore{})).Run(context.Background(), durState{})
	assert.ErrorIs(t, err, ErrCheckpoint)
	assert.Zero(t, c.get("n1"), "a run that cannot log must not start")

	// a state that cannot be logged
	type bad struct{ F func() }
	g := New[bad]("b").AddNode("a", Func(func(*bad) error { return nil })).AddEdge(Start, "a").AddEdge("a", End)
	r, err := g.Compile(WithCheckpointStore(MemStore()))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), bad{})
	assert.ErrorIs(t, err, ErrCheckpoint)
	assert.ErrorContains(t, err, "marshal")
}

func TestCommitFailsAfterNode(t *testing.T) {
	// dies exactly at the commit of n2: n2 ran, but is not committed
	inner := MemStore()
	c := &durCounter{}
	_, _, err := durChain(t, c, 3, WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: 1 + 3 + 1})).Run(context.Background(), durState{})
	require.ErrorIs(t, err, ErrCheckpoint)
	assert.Equal(t, 1, c.get("n2"))
	out, _, err := durChain(t, c, 3, WithCheckpointStore(inner)).Recover(context.Background(), durOnlyRun(t, inner))
	require.NoError(t, err)
	assert.Equal(t, 2, c.get("n2"), "the in-flight node runs again")
	assert.Equal(t, []string{"n1", "n2", "n3"}, out.Trail, "state has n2 once: the uncommitted run left no trace")
}

func TestRecoverRetriesFailedRun(t *testing.T) {
	st := MemStore()
	var fail = true
	c := &durCounter{}
	mk := func() *Runnable[durState] {
		r, err := New[durState]("flaky").
			AddNode("a", func(_ context.Context, s *durState) error { c.hit("a"); s.Trail = append(s.Trail, "a"); return nil }).
			AddNode("b", func(_ context.Context, s *durState) error {
				c.hit("b")
				if fail {
					s.Trail = append(s.Trail, "b-partial")
					return errors.New("boom")
				}
				s.Trail = append(s.Trail, "b")
				return nil
			}).
			AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End).
			Compile(WithCheckpointStore(st))
		require.NoError(t, err)
		return r
	}
	_, _, err := mk().Run(context.Background(), durState{})
	require.Error(t, err)
	id := durOnlyRun(t, st)
	l, _ := st.List(context.Background())
	assert.Equal(t, StatusFailed, l[0].Status)
	assert.Contains(t, l[0].Err, "boom")

	fail = false
	out, rep, err := mk().Recover(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, out.Trail, "partial mutation of the failed attempt is discarded")
	assert.Equal(t, 1, c.get("a"))
	assert.Equal(t, 2, c.get("b"))
	assert.Equal(t, []string{"a", "b"}, rep.Path)
	l, _ = st.List(context.Background())
	assert.Equal(t, StatusDone, l[0].Status)
}

func durAskGraph(t *testing.T, st CheckpointStore, c *durCounter) *Runnable[durState] {
	t.Helper()
	r, err := New[durState]("ask").
		AddNode("pre", func(_ context.Context, s *durState) error { c.hit("pre"); s.Trail = append(s.Trail, "pre"); return nil }).
		AddNode("ask", func(ctx context.Context, s *durState) error {
			c.hit("ask")
			if a, ok := Answer(ctx); ok {
				s.Note = a
				s.Trail = append(s.Trail, "ask:"+a)
				return nil
			}
			s.Trail = append(s.Trail, "ask?")
			return Interrupt("name?")
		}).
		AddNode("post", func(_ context.Context, s *durState) error {
			c.hit("post")
			s.Trail = append(s.Trail, "post")
			return nil
		}).
		AddEdge(Start, "pre").AddEdge("pre", "ask").AddEdge("ask", "post").AddEdge("post", End).
		Compile(WithCheckpointStore(st))
	require.NoError(t, err)
	return r
}

func TestRecoverAfterInterrupt(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	c := &durCounter{}
	r := durAskGraph(t, st, c)
	_, rep0, err := r.Run(ctx, durState{})
	intr, ok := AsInterrupt[durState](err)
	require.True(t, ok)
	id := durOnlyRun(t, st)
	l, _ := st.List(ctx)
	assert.Equal(t, StatusPaused, l[0].Status)
	assert.Equal(t, "name?", l[0].Question)
	recs, _ := st.Load(ctx, id)
	assert.Equal(t, KindInterrupt, recs[len(recs)-1].Kind, "a pause is not a run_end")

	// a fresh process asks the log: same question, same checkpoint
	_, rep, err := durAskGraph(t, st, &durCounter{}).Recover(ctx, id)
	got, ok := AsInterrupt[durState](err)
	require.True(t, ok)
	assert.ErrorIs(t, err, ErrInterrupt)
	assert.Equal(t, intr.Question, got.Question)
	assert.Equal(t, intr.Node, got.Node)
	assert.Equal(t, intr.Checkpoint.Step, got.Checkpoint.Step)
	assert.Equal(t, intr.Checkpoint.State, got.Checkpoint.State)
	assert.Equal(t, intr.Checkpoint.Path, got.Checkpoint.Path)
	assert.Equal(t, intr.Checkpoint.RunID, got.Checkpoint.RunID)
	assert.Len(t, rep.Steps, len(rep0.Steps))
	assert.Equal(t, 1, c.get("ask"), "recover asks, it does not re-run")

	// answer through the recovered checkpoint
	out, _, err := durAskGraph(t, st, c).Resume(ctx, got.Checkpoint, "bob")
	require.NoError(t, err)
	assert.Equal(t, "bob", out.Note)
	recs, _ = st.Load(ctx, id)
	assert.Equal(t, []string{KindRunStart, KindNodeStart, KindNodeCommit, KindEdge, KindNodeStart, KindInterrupt,
		KindResume, KindNodeStart, KindNodeCommit, KindEdge, KindNodeStart, KindNodeCommit, KindEdge, KindRunEnd}, durKinds(recs))
	l, _ = st.List(ctx)
	assert.Equal(t, StatusDone, l[0].Status)
}

func TestRecoverAnsweredThenCrashed(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	c := &durCounter{}
	_, _, err := durAskGraph(t, st, c).Run(ctx, durState{})
	intr, _ := AsInterrupt[durState](err)
	id := durOnlyRun(t, st)
	n, _ := st.Load(ctx, id)

	// the answer is logged, then the process dies before the node commits:
	// resume record (1), node_start (2), crash on commit
	dying := &durDyingStore{CheckpointStore: st, limit: 2}
	_, _, err = durAskGraph(t, dying, c).Resume(ctx, intr.Checkpoint, "carol")
	require.ErrorIs(t, err, ErrCheckpoint)
	recs, _ := st.Load(ctx, id)
	require.Len(t, recs, len(n)+2)

	out, rep, err := durAskGraph(t, st, c).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "carol", out.Note, "the node is re-run with the logged answer")
	assert.Equal(t, []string{"pre", "ask?", "ask:carol", "post"}, out.Trail)
	assert.Equal(t, []string{"pre", "ask", "ask", "post"}, rep.Path, "an interrupted node appears once per attempt")
}

func TestResumeWithoutLogImportsCheckpoint(t *testing.T) {
	ctx := context.Background()
	// checkpoint produced by a graph without a store
	noStore := durAskGraph(t, nil, &durCounter{})
	_, _, err := noStore.Run(ctx, durState{})
	intr, ok := AsInterrupt[durState](err)
	require.True(t, ok)

	st := MemStore()
	c := &durCounter{}
	out, _, err := durAskGraph(t, st, c).Resume(ctx, intr.Checkpoint, "dave")
	require.NoError(t, err)
	assert.Equal(t, "dave", out.Note)
	id := durOnlyRun(t, st)
	recs, _ := st.Load(ctx, id)
	assert.Equal(t, []string{KindRunStart, KindInterrupt, KindResume, KindNodeStart, KindNodeCommit, KindEdge, KindNodeStart, KindNodeCommit, KindEdge, KindRunEnd}, durKinds(recs))

	// resume with a store that cannot load
	_, _, err = durAskGraph(t, durFailingStore{}, c).Resume(ctx, intr.Checkpoint, "x")
	assert.ErrorIs(t, err, ErrCheckpoint)
}

func TestIdempotencyKeyStableAcrossRecovery(t *testing.T) {
	ctx := context.Background()
	inner := MemStore()
	var mu sync.Mutex
	keys := map[string][]string{}
	mk := func(st CheckpointStore) *Runnable[durState] {
		g := New[durState]("idem")
		rec := func(name string) NodeFunc[durState] {
			return func(ctx context.Context, s *durState) error {
				mu.Lock()
				keys[name] = append(keys[name], IdempotencyKey(ctx))
				mu.Unlock()
				s.N++
				return nil
			}
		}
		g.AddNode("a", rec("a")).AddNode("b", rec("b")).
			AddEdge(Start, "a").AddEdge("a", "b").
			AddConditionalEdge("b", func(s *durState) string {
				if s.N < 4 {
					return "b"
				}
				return End
			}, "b", End)
		r, err := g.Compile(WithCheckpointStore(st))
		require.NoError(t, err)
		return r
	}
	// records: run_start, a(start,commit,edge), b#1(start, commit...) dies at the commit of the 2nd b
	_, _, err := mk(&durDyingStore{CheckpointStore: inner, limit: 1 + 3 + 3 + 1}).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrCheckpoint)
	id := durOnlyRun(t, inner)
	_, _, err = mk(inner).Recover(ctx, id)
	require.NoError(t, err)

	assert.Equal(t, []string{id + ":a:1"}, keys["a"])
	// b ran for steps 2,3 (3 crashed before commit), then 3 again after recovery, then 4
	assert.Equal(t, []string{id + ":b:2", id + ":b:3", id + ":b:3", id + ":b:4"}, keys["b"])
	assert.Equal(t, "", IdempotencyKey(ctx))
}

func TestFork(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	c := &durCounter{}
	r := durChain(t, c, 5, WithCheckpointStore(st))
	_, _, err := r.Run(ctx, durState{})
	require.NoError(t, err)
	src := durOnlyRun(t, st)
	srcRecs, _ := st.Load(ctx, src)
	var commit3 int
	for _, rc := range srcRecs {
		if rc.Kind == KindNodeCommit && rc.Node == "n3" {
			commit3 = rc.Seq
		}
	}
	require.NotZero(t, commit3)

	out, rep, err := r.Fork(ctx, src, commit3, "alt", func(s *durState) { s.Note = "edited"; s.N += 100 })
	require.NoError(t, err)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4", "n5"}, out.Trail)
	assert.Equal(t, "edited", out.Note)
	assert.Equal(t, 105, out.N)
	assert.Equal(t, 1, c.get("n1"), "history before the fork point is not re-run")
	assert.Equal(t, 1, c.get("n3"))
	assert.Equal(t, 2, c.get("n4"))
	assert.Equal(t, 2, c.get("n5"))
	assert.Len(t, rep.Steps, 5)

	// the source is untouched
	again, _ := st.Load(ctx, src)
	assert.Equal(t, srcRecs, again)
	// the fork's log is the prefix, a fork record, then the continuation
	alt, _ := st.Load(ctx, "alt")
	assert.Equal(t, srcRecs[:commit3], alt[:commit3])
	assert.Equal(t, KindFork, alt[commit3].Kind)
	assert.Equal(t, src, alt[commit3].From)
	assert.Equal(t, commit3, alt[commit3].FromSeq)
	assert.Equal(t, "n4", alt[commit3].Next)
	assert.Equal(t, KindRunEnd, alt[len(alt)-1].Kind)

	// fork of a fork, without an edit, at the run_start
	out, _, err = r.Fork(ctx, "alt", 1, "again", nil)
	require.NoError(t, err)
	assert.Equal(t, 5, out.N)
	assert.Equal(t, 2, c.get("n1"))

	// errors
	_, _, err = r.Fork(ctx, src, commit3, src, nil)
	assert.ErrorContains(t, err, "must differ")
	_, _, err = r.Fork(ctx, src, commit3, "alt", nil)
	assert.ErrorContains(t, err, "already exists")
	_, _, err = r.Fork(ctx, src, 2, "x1", nil) // node_start
	assert.ErrorContains(t, err, "not a node_commit")
	_, _, err = r.Fork(ctx, src, 999, "x2", nil)
	assert.ErrorContains(t, err, "not a node_commit")
}

func TestRollbackFromCrashedRun(t *testing.T) {
	// a run that died is forked back to an earlier commit; the dead run stays dead
	ctx := context.Background()
	inner := MemStore()
	c := &durCounter{}
	_, _, err := durChain(t, c, 4, WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: 1 + 3*3 + 1})).Run(ctx, durState{})
	require.Error(t, err)
	id := durOnlyRun(t, inner)
	recs, _ := inner.Load(ctx, id)
	var c1 int
	for _, rc := range recs {
		if rc.Kind == KindNodeCommit && rc.Node == "n1" {
			c1 = rc.Seq
		}
	}
	out, _, err := durChain(t, c, 4, WithCheckpointStore(inner)).Fork(ctx, id, c1, "rollback", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4"}, out.Trail)
	still, _ := inner.Load(ctx, id)
	assert.Equal(t, recs, still)
}

func TestTopologyChangeGuard(t *testing.T) {
	ctx := context.Background()
	inner := MemStore()
	c := &durCounter{}
	_, _, err := durChain(t, c, 4, WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: 1 + 3 + 2})).Run(ctx, durState{})
	require.Error(t, err)
	id := durOnlyRun(t, inner)

	// n5 added at the end: a different graph
	_, _, err = durChain(t, c, 5, WithCheckpointStore(inner)).Recover(ctx, id)
	assert.ErrorIs(t, err, ErrTopologyChanged)
	_, _, err = durChain(t, c, 5, WithCheckpointStore(inner)).Fork(ctx, id, 1, "f", nil)
	assert.ErrorIs(t, err, ErrTopologyChanged)

	out, _, err := durChain(t, c, 5, WithCheckpointStore(inner), AllowTopologyChange()).Recover(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4", "n5"}, out.Trail)

	// allowed, but the node the run continues at is gone
	inner2 := MemStore()
	_, _, err = durChain(t, c, 4, WithCheckpointStore(&durDyingStore{CheckpointStore: inner2, limit: 1 + 3*3 + 1})).Run(ctx, durState{})
	require.Error(t, err)
	_, _, err = durChain(t, c, 2, WithCheckpointStore(inner2), AllowTopologyChange()).Recover(ctx, durOnlyRun(t, inner2))
	assert.ErrorIs(t, err, ErrTopologyChanged)
	assert.ErrorContains(t, err, `"n4"`)

	// Editing a node body or moving code does not change the topology.
	assert.Equal(t, durChain(t, c, 4).topoHash(), durChain(t, &durCounter{}, 4).topoHash())
}

func TestParallelCommitsOnlyMergedResult(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	g := New[durState]("par").
		AddNode("fan", Func(func(s *durState) error { return nil })).
		AddNode("l", Func(func(s *durState) error { s.Trail = []string{"l"}; return nil })).
		AddNode("r", Func(func(s *durState) error { s.Trail = []string{"r"}; return nil })).
		AddParallel("both", []string{"l", "r"}, func(dst *durState, res []durState) error {
			for _, x := range res {
				dst.Trail = append(dst.Trail, x.Trail...)
			}
			return nil
		}).
		AddEdge(Start, "fan").AddEdge("fan", "both").AddEdge("both", End)
	r, err := g.Compile(WithCheckpointStore(st))
	require.NoError(t, err)
	out, _, err := r.Run(ctx, durState{})
	require.NoError(t, err)
	assert.Equal(t, []string{"l", "r"}, out.Trail)
	recs, _ := st.Load(ctx, durOnlyRun(t, st))
	var commits []string
	for _, rc := range recs {
		if rc.Kind == KindNodeCommit {
			commits = append(commits, rc.Node)
		}
	}
	assert.Equal(t, []string{"fan", "both"}, commits)
}

func TestSessionRecordedAndResumedOnRecover(t *testing.T) {
	ctx := context.Background()
	bin := clitest.FakeCLI(t, clitest.RecordStdin(), clitest.Init("sess-42"), clitest.Text("ok"), clitest.Result("done", 0.25))
	run := runner.New(quiet, runner.WithBinary(bin))
	mk := func(st CheckpointStore, opts ...AgentOption) *Runnable[durState] {
		r, err := New[durState]("agent").
			AddNode("pre", durTrail("pre")).
			AddAgentNode("work", Agent(run,
				func(s *durState) runner.Input { return runner.Input{Prompt: "do the work"} },
				func(s *durState, res *runner.Result) error { s.Note = res.ResultText; return nil }, opts...)).
			AddNode("post", durTrail("post")).
			AddEdge(Start, "pre").AddEdge("pre", "work").AddEdge("work", "post").AddEdge("post", End).
			Compile(WithCheckpointStore(st))
		require.NoError(t, err)
		return r
	}
	inner := MemStore()
	// records: run_start, pre(3), work node_start(5), session(6), then the commit is lost
	_, _, err := mk(&durDyingStore{CheckpointStore: inner, limit: 1 + 3 + 1 + 1}).Run(ctx, durState{})
	require.ErrorIs(t, err, ErrCheckpoint)
	assert.NotContains(t, clitest.Args(t, bin), "--resume", "the first run starts fresh")
	id := durOnlyRun(t, inner)
	recs, _ := inner.Load(ctx, id)
	last := recs[len(recs)-1]
	assert.Equal(t, KindSession, last.Kind)
	assert.Equal(t, "sess-42", last.SessionID)
	assert.Equal(t, "work", last.Node)
	assert.Equal(t, 2, last.Step)

	out, rep, err := mk(inner, WithRecoverPrompt("Continue the work please.")).Recover(ctx, id)
	require.NoError(t, err)
	args := clitest.Args(t, bin)
	i := durIndexOf(args, "--resume")
	require.GreaterOrEqual(t, i, 0, "argv: %v", args)
	assert.Equal(t, "sess-42", args[i+1])
	assert.Contains(t, clitest.Stdin(t, bin), "Continue the work please.")
	assert.NotContains(t, clitest.Stdin(t, bin), "do the work")
	assert.Equal(t, "done", out.Note)
	assert.Equal(t, []string{"pre", "post"}, out.Trail)
	assert.Equal(t, []string{"pre", "work", "post"}, rep.Path)
	assert.InDelta(t, 0.25, rep.TotalCostUSD, 1e-9)
	after, _ := inner.Load(ctx, id)
	var rcv *Record
	for i := range after {
		if after[i].Kind == KindRecover {
			rcv = &after[i]
		}
	}
	require.NotNil(t, rcv)
	assert.Equal(t, "sess-42", rcv.SessionID)

	// a node after the crashed one starts fresh again: no resume leaks
	assert.Equal(t, KindRunEnd, after[len(after)-1].Kind)
}

func TestRecoverPromptPrecedence(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		opts []AgentOption
		want string
	}{
		"default":      {nil, DefaultRecoverPrompt},
		"retry prompt": {[]AgentOption{WithRetry(runner.RetryPolicy{MaxAttempts: 1, ResumePrompt: "retry-continue"})}, "retry-continue"},
		"option wins":  {[]AgentOption{WithRetry(runner.RetryPolicy{MaxAttempts: 1, ResumePrompt: "retry-continue"}), WithRecoverPrompt("mine")}, "mine"},
	} {
		t.Run(name, func(t *testing.T) {
			bin := clitest.FakeCLI(t, clitest.RecordStdin(), clitest.Init("s1"), clitest.Result("r", 0))
			run := runner.New(quiet, runner.WithBinary(bin))
			r, err := New[durState]("a").
				AddAgentNode("work", Agent(run, func(*durState) runner.Input { return runner.Input{Prompt: "orig"} }, nil, tc.opts...)).
				AddEdge(Start, "work").AddEdge("work", End).
				Compile(WithCheckpointStore(MemStore()))
			require.NoError(t, err)
			// craft the crashed log by hand: run_start, node_start, session
			st := MemStore()
			r.cfg.store = st
			for _, rc := range []Record{
				{Seq: 1, Kind: KindRunStart, State: []byte(`{}`), Next: "work", TopoHash: r.topoHash()},
				{Seq: 2, Kind: KindNodeStart, Node: "work", Step: 1},
				{Seq: 3, Kind: KindSession, Node: "work", Step: 1, SessionID: "s1"},
			} {
				require.NoError(t, st.Append(ctx, "crashed", rc))
			}
			_, _, err = r.Recover(ctx, "crashed")
			require.NoError(t, err)
			assert.Contains(t, clitest.Stdin(t, bin), tc.want)
			assert.NotContains(t, clitest.Stdin(t, bin), "orig")
		})
	}
}

func TestRecoveredSessionForHandWrittenNode(t *testing.T) {
	ctx := context.Background()
	st := MemStore()
	var got []string
	r, err := New[durState]("hw").
		AddNode("a", func(ctx context.Context, s *durState) error {
			sid, ok := RecoveredSession(ctx)
			got = append(got, fmt.Sprintf("%s/%v", sid, ok))
			return nil
		}).
		AddNode("b", func(ctx context.Context, s *durState) error {
			sid, ok := RecoveredSession(ctx)
			got = append(got, fmt.Sprintf("b:%s/%v", sid, ok))
			return nil
		}).
		AddEdge(Start, "a").AddEdge("a", "b").AddEdge("b", End).
		Compile(WithCheckpointStore(st))
	require.NoError(t, err)
	_, _, err = r.Run(ctx, durState{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/false", "b:/false"}, got)

	got = nil
	for _, rc := range []Record{
		{Seq: 1, Kind: KindRunStart, State: []byte(`{}`), Next: "a", TopoHash: r.topoHash()},
		{Seq: 2, Kind: KindNodeStart, Node: "a", Step: 1},
		{Seq: 3, Kind: KindSession, Node: "a", Step: 1, SessionID: "sid-9"},
		{Seq: 4, Kind: KindSession, Node: "a", Step: 7, SessionID: "other"}, // wrong step: ignored (last wins)
	} {
		require.NoError(t, st.Append(ctx, "c", rc))
	}
	_, _, err = r.Recover(ctx, "c")
	require.NoError(t, err)
	assert.Equal(t, []string{"/false", "b:/false"}, got, "a session of another step is not resumed")

	got = nil
	require.NoError(t, st.Append(ctx, "d", Record{Seq: 1, Kind: KindRunStart, State: []byte(`{}`), Next: "a", TopoHash: r.topoHash()}))
	require.NoError(t, st.Append(ctx, "d", Record{Seq: 2, Kind: KindSession, Node: "a", Step: 1, SessionID: "sid-9"}))
	_, _, err = r.Recover(ctx, "d")
	require.NoError(t, err)
	assert.Equal(t, []string{"sid-9/true", "b:/false"}, got, "only the crashed node sees the session")
}

func TestSessionLogFailureSurfacesAtCommit(t *testing.T) {
	ctx := context.Background()
	bin := clitest.FakeCLI(t, clitest.Init("s"), clitest.Result("r", 0))
	run := runner.New(quiet, runner.WithBinary(bin))
	inner := MemStore()
	// run_start, node_start succeed; the session append fails
	r, err := New[durState]("a").
		AddAgentNode("work", Agent(run, func(*durState) runner.Input { return runner.Input{Prompt: "p"} }, nil)).
		AddEdge(Start, "work").AddEdge("work", End).
		Compile(WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: 2}))
	require.NoError(t, err)
	_, _, err = r.Run(ctx, durState{})
	assert.ErrorIs(t, err, ErrCheckpoint)
}

func TestObserverSeesRecoveredSegment(t *testing.T) {
	ctx := context.Background()
	inner := MemStore()
	c := &durCounter{}
	_, _, err := durChain(t, c, 3, WithCheckpointStore(&durDyingStore{CheckpointStore: inner, limit: 5})).Run(ctx, durState{})
	require.Error(t, err)
	obs := &durRunObs{}
	_, _, err = durChain(t, c, 3, WithCheckpointStore(inner), WithObserver(obs)).Recover(ctx, durOnlyRun(t, inner))
	require.NoError(t, err)
	assert.Equal(t, 1, obs.starts)
	assert.Equal(t, 1, obs.ends)
	assert.True(t, obs.resumed)
	assert.NoError(t, obs.err)

	// recovering a finished run still reports a segment
	obs2 := &durRunObs{}
	_, _, err = durChain(t, c, 3, WithCheckpointStore(inner), WithObserver(obs2)).Recover(ctx, durOnlyRun(t, inner))
	require.NoError(t, err)
	assert.Equal(t, 1, obs2.ends)
}

type durRunObs struct {
	starts, ends int
	resumed      bool
	err          error
}

func (o *durRunObs) NodeStart(context.Context, StepInfo)      {}
func (o *durRunObs) NodeEnd(context.Context, StepInfo, error) {}
func (o *durRunObs) RunStart(_ context.Context, i RunInfo)    { o.starts++; o.resumed = i.Resumed }
func (o *durRunObs) RunEnd(_ context.Context, _ RunInfo, _ *Report, err error) {
	o.ends++
	o.err = err
}

func TestSummarize(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) time.Time { return now.Add(d) }
	for name, tc := range map[string]struct {
		recs   []Record
		status string
	}{
		"empty":   {nil, StatusCrashed},
		"running": {[]Record{{Kind: KindRunStart, Time: at(-time.Hour)}, {Kind: KindNodeStart, Time: at(-time.Second)}}, StatusRunning},
		"crashed": {[]Record{{Kind: KindRunStart, Time: at(-time.Hour)}, {Kind: KindNodeStart, Time: at(-time.Hour)}}, StatusCrashed},
		"paused":  {[]Record{{Kind: KindRunStart, Time: at(-time.Hour)}, {Kind: KindInterrupt, Time: at(-time.Hour), Question: "q?", CostUSD: 1}}, StatusPaused},
		"done":    {[]Record{{Kind: KindRunStart}, {Kind: KindNodeCommit, Node: "a", Next: End, CostUSD: 2}, {Kind: KindRunEnd}}, StatusDone},
		"failed":  {[]Record{{Kind: KindRunStart}, {Kind: KindRunEnd, Err: "x"}}, StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			s := Summarize("r", tc.recs, now)
			assert.Equal(t, tc.status, s.Status)
			assert.Equal(t, "r", s.RunID)
		})
	}
	s := Summarize("r", []Record{{Kind: KindRunStart, Graph: "g", Next: "a"}, {Kind: KindNodeCommit, Node: "a", Next: "b", CostUSD: 2}, {Kind: KindFork, Next: "c"}}, now)
	assert.Equal(t, "g", s.Graph)
	assert.Equal(t, "a", s.LastNode)
	assert.Equal(t, "c", s.Next)
	assert.Equal(t, 1, s.Commits)
	assert.Equal(t, 2.0, s.CostUSD)
}

func durTrail(tag string) NodeFunc[durState] {
	return func(_ context.Context, s *durState) error {
		s.Trail = append(s.Trail, tag)
		return nil
	}
}

func durIndexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
