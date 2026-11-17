package graph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests build testdata/debugbin twice: the default build, whose node n3
// is broken, and `-tags fixed`, where it is fixed. They stand for the loop a
// babysitter goes through: the run fails or is paused, the code is patched
// and rebuilt, and the new binary continues the same run from its log.

type debugResult struct {
	Status     string   `json:"status"`
	Build      string   `json:"build"`
	Trail      []string `json:"trail"`
	StateBuild string   `json:"state_build"`
	RunID      string   `json:"run_id"`
	Node       string   `json:"node"`
	Err        string   `json:"err"`
}

// buildDebugbin compiles testdata/debugbin with tags into a temp dir.
func buildDebugbin(t *testing.T, tags string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "debugbin-"+strings.NewReplacer(",", "-").Replace(tags+"default"))
	args := []string{"build", "-o", bin}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	out, err := exec.Command("go", append(args, "./testdata/debugbin")...).CombinedOutput()
	require.NoError(t, err, "go build: %s", out)
	return bin
}

func debugbinPair(t *testing.T) (buggy, fixed string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary twice")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	return buildDebugbin(t, ""), buildDebugbin(t, "fixed")
}

// runDebugbin runs bin and decodes its JSON line; wantCode is its exit code.
func runDebugbin(t *testing.T, cmd *exec.Cmd, wantCode int) debugResult {
	t.Helper()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return decodeDebugbin(t, cmd, cmd.Run(), &stdout, &stderr, wantCode)
}

// decodeDebugbin checks the exit status err of a finished cmd and decodes the
// JSON line it printed.
func decodeDebugbin(t *testing.T, cmd *exec.Cmd, err error, stdout, stderr *strings.Builder, wantCode int) debugResult {
	t.Helper()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		require.NoError(t, err)
	}
	require.Equal(t, wantCode, cmd.ProcessState.ExitCode(), "exit code; stdout %s stderr %s", stdout, stderr)
	var res debugResult
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &res), "stdout %s stderr %s", stdout, stderr)
	return res
}

func debugCmd(bin, store, mark string, args ...string) *exec.Cmd {
	return exec.Command(bin, append([]string{"-store", store, "-mark", mark}, args...)...)
}

func markBuilds(t *testing.T, mark, node string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mark, node+".log"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var builds []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		builds = append(builds, strings.TrimPrefix(strings.Fields(l)[1], "build="))
	}
	return builds
}

// The buggy build fails in n3; the run is logged as failed. The fixed build,
// another binary and another process, recovers the same run id: n1 and n2 do
// not run again, n3 runs with the fix, n4 runs once.
func TestFixedBinaryRecoversFailedRun(t *testing.T) {
	buggy, fixed := debugbinPair(t)
	store, mark := filepath.Join(t.TempDir(), "runs"), t.TempDir()

	res := runDebugbin(t, debugCmd(buggy, store, mark, "run"), 1)
	assert.Equal(t, "error", res.Status)
	assert.Contains(t, res.Err, "index out of range")

	st, err := FileStore(store)
	require.NoError(t, err)
	l, err := st.List(context.Background())
	require.NoError(t, err)
	require.Len(t, l, 1)
	assert.Equal(t, StatusFailed, l[0].Status)
	assert.Equal(t, "n2", l[0].LastNode)
	assert.Equal(t, "n3", l[0].Next)

	res = runDebugbin(t, debugCmd(fixed, store, mark, "recover", l[0].RunID), 0)
	assert.Equal(t, "done", res.Status)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4"}, res.Trail)
	assert.Equal(t, "fixed", res.StateBuild)

	assert.Equal(t, []string{"buggy"}, markBuilds(t, mark, "n1"))
	assert.Equal(t, []string{"buggy"}, markBuilds(t, mark, "n2"))
	assert.Equal(t, []string{"buggy", "fixed"}, markBuilds(t, mark, "n3"))
	assert.Equal(t, []string{"fixed"}, markBuilds(t, mark, "n4"))
	l, _ = st.List(context.Background())
	assert.Equal(t, StatusDone, l[0].Status)
}

// The debugging session: while the buggy build is busy in n2, another process
// requests a pause (FilePause, as `cag pause` would). The run stops before n3,
// so the bug never runs; its state is readable from the store; the fixed
// build continues it.
func TestPauseRunningBinaryThenRecoverWithFixedBuild(t *testing.T) {
	buggy, fixed := debugbinPair(t)
	store, mark := filepath.Join(t.TempDir(), "runs"), t.TempDir()
	release := filepath.Join(t.TempDir(), "release")

	cmd := debugCmd(buggy, store, mark, "-wait", release, "run")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait until n2 is running, then pause the run from outside and let n2 end.
	st, err := FileStore(store)
	require.NoError(t, err)
	var runID string
	require.Eventually(t, func() bool {
		if len(markBuilds(t, mark, "n2")) == 0 {
			return false
		}
		l, err := st.List(context.Background())
		if err != nil || len(l) != 1 {
			return false
		}
		runID = l[0].RunID
		return true
	}, 30*time.Second, 10*time.Millisecond)
	require.NoError(t, FilePause{Dir: store}.Request(runID))
	require.NoError(t, os.WriteFile(release, nil, 0o600))

	var res debugResult
	select {
	case err := <-done:
		res = decodeDebugbin(t, cmd, err, &stdout, &stderr, 2)
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the buggy build did not stop on the pause")
	}
	assert.Equal(t, "paused", res.Status)
	assert.Equal(t, "n3", res.Node)
	assert.Equal(t, runID, res.RunID)
	assert.Empty(t, markBuilds(t, mark, "n3"), "the broken node never ran")

	// What a babysitter sees while it debugs: a paused run and its state.
	l, err := st.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusPaused, l[0].Status)
	assert.Equal(t, "n3", l[0].Next)
	recs, err := st.Load(context.Background(), runID)
	require.NoError(t, err)
	last := recs[len(recs)-1]
	assert.Equal(t, KindPause, last.Kind)
	assert.JSONEq(t, `{"trail":["n1","n2"]}`, string(last.State))

	res = runDebugbin(t, debugCmd(fixed, store, mark, "recover", runID), 0)
	assert.Equal(t, "done", res.Status)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4"}, res.Trail)
	assert.Equal(t, []string{"buggy"}, markBuilds(t, mark, "n1"))
	assert.Equal(t, []string{"buggy"}, markBuilds(t, mark, "n2"))
	assert.Equal(t, []string{"fixed"}, markBuilds(t, mark, "n3"))
	assert.Equal(t, []string{"fixed"}, markBuilds(t, mark, "n4"))
	ok, err := FilePause{Dir: store}.PauseRequested(context.Background(), runID)
	require.NoError(t, err)
	assert.False(t, ok, "the fixed build cleared the pause")
}
