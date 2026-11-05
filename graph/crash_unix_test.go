//go:build unix

package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	childMains["crash10"] = func() { crashChild(false) }
	childMains["recover10"] = func() { crashChild(true) }
}

// crashGraph is n1..n10. Every node appends a line to <mark>/<node>.log (a
// side effect that outlives the process), then adds itself to the state. The
// node named crashAt SIGKILLs its own process after its side effect, in the
// middle of the node: no deferred function, no run_end, nothing flushes.
func crashGraph(mark, crashAt string, store CheckpointStore) (*Runnable[durState], error) {
	g := New[durState]("crash10")
	prev := Start
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("n%d", i)
		g.AddNode(name, func(ctx context.Context, s *durState) error {
			f, err := os.OpenFile(filepath.Join(mark, name+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			fmt.Fprintf(f, "%s pid=%d key=%s\n", name, os.Getpid(), IdempotencyKey(ctx))
			f.Close()
			if name == crashAt {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {} // not reached: SIGKILL cannot be handled
			}
			AddCost(ctx, 0.5)
			s.Trail = append(s.Trail, name)
			s.N++
			return nil
		})
		g.AddEdge(prev, name)
		prev = name
	}
	g.AddEdge(prev, End)
	return g.Compile(WithCheckpointStore(store))
}

// crashChild is the re-executed test binary: it runs (or recovers) the graph
// in the stores and dirs named by the environment.
func crashChild(recoverRun bool) {
	store, err := FileStore(os.Getenv("CRASH_STORE"))
	if err != nil {
		panic(err)
	}
	r, err := crashGraph(os.Getenv("CRASH_MARK"), os.Getenv("CRASH_AT"), store)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	if !recoverRun {
		_, _, err = r.Run(ctx, durState{})
		fmt.Println("child: run returned", err) // only reached if it did not crash
		return
	}
	l, err := store.List(ctx)
	if err != nil || len(l) != 1 {
		panic(fmt.Sprint("list: ", l, err))
	}
	out, rep, err := r.Recover(ctx, l[0].RunID)
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(map[string]any{"trail": out.Trail, "steps": len(rep.Steps), "cost": rep.TotalCostUSD})
	fmt.Println(string(b))
}

func markLines(t *testing.T, mark, node string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mark, node+".log"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(b), "\n")
}

// startCrash runs the 10-node graph in a child process that kills itself in
// n7 and returns the store and mark dirs.
func startCrash(t *testing.T) (storeDir, markDir string) {
	t.Helper()
	storeDir, markDir = filepath.Join(t.TempDir(), "runs"), t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GRAPH_CHILD=crash10", "CRASH_STORE="+storeDir, "CRASH_MARK="+markDir, "CRASH_AT=n7")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the child must die, output: %s", out)
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, ws.Signaled() && ws.Signal() == syscall.SIGKILL, "child must be SIGKILLed, got %v (%s)", ws, out)

	// the world as the crash left it
	for i := 1; i <= 7; i++ {
		require.Equal(t, 1, markLines(t, markDir, fmt.Sprintf("n%d", i)))
	}
	for i := 8; i <= 10; i++ {
		require.Zero(t, markLines(t, markDir, fmt.Sprintf("n%d", i)))
	}
	return storeDir, markDir
}

func assertRecovered(t *testing.T, markDir string) {
	t.Helper()
	for i := 1; i <= 10; i++ {
		want := 1
		if i == 7 {
			want = 2 // the crashed node ran again, and only that one
		}
		assert.Equal(t, want, markLines(t, markDir, fmt.Sprintf("n%d", i)), "n%d side effects", i)
	}
	// the re-run of n7 got the same idempotency key as the crashed attempt
	b, _ := os.ReadFile(filepath.Join(markDir, "n7.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	require.Len(t, lines, 2)
	key := func(l string) string { return l[strings.Index(l, "key="):] }
	assert.Equal(t, key(lines[0]), key(lines[1]))
	assert.True(t, strings.HasSuffix(key(lines[0]), ":n7:7"), lines[0])
	pid := func(l string) string { return strings.Fields(l)[1] }
	assert.NotEqual(t, pid(lines[0]), pid(lines[1]), "the second attempt is another process")
}

func wantTrail() []string {
	var w []string
	for i := 1; i <= 10; i++ {
		w = append(w, fmt.Sprintf("n%d", i))
	}
	return w
}

// THE REAL CRASH TEST: a separate process dies by SIGKILL inside node 7 of 10.
// Recover, in this process, must not re-run nodes 1-6, must re-run 7, and run
// 8-10 exactly once.
func TestRealCrashRecoverInProcess(t *testing.T) {
	storeDir, markDir := startCrash(t)
	store, err := FileStore(storeDir)
	require.NoError(t, err)
	ctx := context.Background()
	l, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, l, 1)
	assert.Equal(t, 6, l[0].Commits, "six nodes were committed before the kill")
	assert.Equal(t, "n6", l[0].LastNode)
	assert.Equal(t, "n7", l[0].Next)
	assert.Contains(t, []string{StatusRunning, StatusCrashed}, l[0].Status)
	recs, err := store.Load(ctx, l[0].RunID)
	require.NoError(t, err)
	assert.Equal(t, KindNodeStart, recs[len(recs)-1].Kind, "the log ends inside n7")

	r, err := crashGraph(markDir, "", store)
	require.NoError(t, err)
	out, rep, err := r.Recover(ctx, l[0].RunID)
	require.NoError(t, err)

	assertRecovered(t, markDir)
	assert.Equal(t, wantTrail(), out.Trail)
	assert.Equal(t, 10, out.N)
	assert.Equal(t, wantTrail(), rep.Path)
	assert.Len(t, rep.Steps, 10, "the report spans the crash")
	assert.InDelta(t, 5.0, rep.TotalCostUSD, 1e-9)
	l, _ = store.List(ctx)
	assert.Equal(t, StatusDone, l[0].Status)
}

// Same, but the recovery is itself a fresh process.
func TestRealCrashRecoverInAnotherProcess(t *testing.T) {
	storeDir, markDir := startCrash(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GRAPH_CHILD=recover10", "CRASH_STORE="+storeDir, "CRASH_MARK="+markDir)
	out, err := cmd.Output()
	require.NoError(t, err, string(out))
	var got struct {
		Trail []string
		Steps int
		Cost  float64
	}
	require.NoError(t, json.Unmarshal(out, &got), string(out))
	assert.Equal(t, wantTrail(), got.Trail)
	assert.Equal(t, 10, got.Steps)
	assertRecovered(t, markDir)
}
