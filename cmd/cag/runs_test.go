package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

// storeWith writes a done run, a paused run and a failed run into a FileStore.
func storeWith(t *testing.T) (dir string, ids map[string]string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "store")
	store, err := graph.FileStore(dir)
	require.NoError(t, err)
	mk := func(pause, fail bool) *graph.Runnable[wst] {
		r, err := graph.New[wst]("w").
			AddNode("a", graph.Func(func(s *wst) error { s.N++; return nil })).
			AddNode("b", func(ctx context.Context, s *wst) error {
				if _, ok := graph.Answer(ctx); !ok && pause {
					return graph.Interrupt("sure?")
				}
				if fail {
					return errors.New("boom")
				}
				return nil
			}).
			AddEdge(graph.Start, "a").AddEdge("a", "b").AddEdge("b", graph.End).
			Compile(graph.WithCheckpointStore(store))
		require.NoError(t, err)
		return r
	}
	before := map[string]bool{}
	ids = map[string]string{}
	one := func(label string) {
		l, err := store.List(context.Background())
		require.NoError(t, err)
		for _, s := range l {
			if !before[s.RunID] {
				before[s.RunID] = true
				ids[label] = s.RunID
			}
		}
	}
	_, _, err = mk(false, false).Run(context.Background(), wst{})
	require.NoError(t, err)
	one("done")
	_, _, err = mk(true, false).Run(context.Background(), wst{})
	require.ErrorIs(t, err, graph.ErrInterrupt)
	one("paused")
	_, _, err = mk(false, true).Run(context.Background(), wst{})
	require.Error(t, err)
	one("failed")
	require.Len(t, ids, 3)
	return dir, ids
}

func TestRunsListAndTimeline(t *testing.T) {
	dir, ids := storeWith(t)
	var out bytes.Buffer
	require.NoError(t, runsRun([]string{dir}, &out, io.Discard))
	got := out.String()
	for label, id := range ids {
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(id) + `.*$`).FindString(got)
		require.NotEmpty(t, line, got)
		assert.Contains(t, line, label)
	}
	assert.Contains(t, got, "RUN")
	assert.Contains(t, got, "s ago")

	out.Reset()
	require.NoError(t, runsRun([]string{dir, ids["paused"]}, &out, io.Discard))
	assert.Contains(t, out.String(), "run_start")
	assert.Contains(t, out.String(), "node_commit")
	assert.Contains(t, out.String(), `question="sure?"`)
	assert.Contains(t, out.String(), "next=b")

	out.Reset()
	require.NoError(t, runsRun([]string{dir, ids["failed"]}, &out, io.Discard))
	assert.Contains(t, out.String(), "err=")
}

func TestRunsErrors(t *testing.T) {
	var b bytes.Buffer
	assert.ErrorAs(t, runsRun(nil, &b, &b), new(usageError))
	assert.ErrorAs(t, runsRun([]string{"a", "b", "c"}, &b, &b), new(usageError))
	assert.Error(t, runsRun([]string{"--nope"}, &b, &b))
	assert.Error(t, runsRun([]string{filepath.Join(t.TempDir(), "missing")}, &b, &b))
	f := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(f, nil, 0o600))
	assert.ErrorContains(t, runsRun([]string{f}, &b, &b), "not a directory")
	dir := t.TempDir()
	b.Reset()
	require.NoError(t, runsRun([]string{dir}, &b, &b))
	assert.Contains(t, b.String(), "no runs")
	assert.ErrorContains(t, runsRun([]string{dir, "nope"}, &b, &b), "not found")
	assert.Equal(t, traceUsage, usageError{}.Error())
}

func TestHelpers(t *testing.T) {
	assert.Equal(t, "-", dash(""))
	assert.Equal(t, "x", dash("x"))
	assert.Equal(t, "", stepStr(0))
	assert.Equal(t, "3", stepStr(3))
	assert.Equal(t, "-", ago(time.Time{}))
	assert.Equal(t, "5m ago", ago(time.Now().Add(-5*time.Minute)))
	assert.Equal(t, "3h ago", ago(time.Now().Add(-3*time.Hour)))
	assert.Equal(t, "3d ago", ago(time.Now().Add(-72*time.Hour)))
	d := detail(graph.Record{Kind: graph.KindFork, From: "r1", FromSeq: 4, Next: "x", CostUSD: 1, DurationMS: 5, SessionID: "s", Answer: "a"})
	for _, w := range []string{"next=x", "cost=$1.0000", "took=5ms", "session=s", `answer="a"`, "from=r1@4"} {
		assert.Contains(t, d, w)
	}
	assert.Contains(t, detail(graph.Record{Kind: graph.KindEdge, From: "p"}), "from=p")
}

func TestWebReplaysCheckpointStore(t *testing.T) {
	dir, ids := storeWith(t)
	serve := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		var out syncBuf
		done := make(chan error, 1)
		go func() { done <- webRun(ctx, args, &out, io.Discard) }()
		var url string
		require.Eventually(t, func() bool { url = regexp.MustCompile(`http://\S+`).FindString(out.String()); return url != "" }, 3*time.Second, 10*time.Millisecond)
		resp, err := http.Get(url + "/")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Contains(t, string(body), `"readOnly":true`)
		cancel()
		require.NoError(t, <-done)
		return string(body)
	}
	for _, args := range [][]string{
		{"--replay", dir, "--run", ids["done"]},
		{"--replay", dir},
		{"--replay", filepath.Join(dir, ids["paused"]+".jsonl")},
	} {
		serve(args...)
	}

	ctx := context.Background()
	assert.Error(t, webRun(ctx, []string{"--replay", dir, "--run", "nope"}, io.Discard, io.Discard))
	assert.Error(t, webRun(ctx, []string{"--replay", t.TempDir()}, io.Discard, io.Discard), "empty store")
	txt := filepath.Join(t.TempDir(), "x.txt")
	require.NoError(t, os.WriteFile(txt, []byte("x"), 0o600))
	err := webRun(ctx, []string{"--replay", txt}, io.Discard, io.Discard)
	assert.True(t, err != nil && strings.Contains(err.Error(), "not a journal"), "%v", err)
}
