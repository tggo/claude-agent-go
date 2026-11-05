package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

type wst struct{ N int }

func writeJournal(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	srv, err := graphweb.New(graphweb.WithJournal(path))
	require.NoError(t, err)
	r, err := graph.New[wst]("w").AddNode("a", graph.Func(func(s *wst) error { s.N++; return nil })).
		AddEdge(graph.Start, "a").AddEdge("a", graph.End).Compile(graph.WithObserver(srv))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), wst{})
	require.NoError(t, err)
	require.NoError(t, srv.Close())
	return path
}

func TestWebServesReplay(t *testing.T) {
	path := writeJournal(t)
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuf
	done := make(chan error, 1)
	openedCh := make(chan string, 1)
	old := openFile
	openFile = func(p string) error { openedCh <- p; return nil }
	defer func() { openFile = old }()
	go func() { done <- webRun(ctx, []string{"--replay", path, "--open"}, &out, io.Discard) }()
	var url string
	require.Eventually(t, func() bool {
		m := regexp.MustCompile(`http://\S+`).FindString(out.String())
		url = m
		return m != ""
	}, 3*time.Second, 10*time.Millisecond)
	resp, err := http.Get(url + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(body), `"readOnly":true`)
	assert.Equal(t, url, <-openedCh)
	cancel()
	require.NoError(t, <-done)

	// positional arg and a directory work too
	ctx2, cancel2 := context.WithCancel(context.Background())
	var out2 syncBuf
	go func() { done <- webRun(ctx2, []string{filepath.Dir(path)}, &out2, io.Discard) }()
	require.Eventually(t, func() bool { return strings.Contains(out2.String(), "http://") }, 3*time.Second, 10*time.Millisecond)
	cancel2()
	require.NoError(t, <-done)
}

func TestWebErrors(t *testing.T) {
	ctx := context.Background()
	err := webRun(ctx, nil, io.Discard, io.Discard)
	assert.ErrorAs(t, err, new(usageError))
	err = webRun(ctx, []string{"a", "b"}, io.Discard, io.Discard)
	assert.ErrorAs(t, err, new(usageError))
	err = webRun(ctx, []string{"--nope"}, io.Discard, io.Discard)
	assert.Error(t, err)
	err = webRun(ctx, []string{"--replay", filepath.Join(t.TempDir(), "x")}, io.Discard, io.Discard)
	assert.ErrorContains(t, err, "cag trace")
	good := writeJournal(t)
	err = webRun(ctx, []string{"--replay", good, "--addr", "bad"}, io.Discard, io.Discard)
	assert.Error(t, err)
	_ = os.Remove(good)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
