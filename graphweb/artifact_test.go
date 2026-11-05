package graphweb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

func artRun(t *testing.T, s *Server, paths map[string]string) {
	t.Helper()
	g := graph.New[st]("art").
		AddNode("render", func(ctx context.Context, _ *st) error {
			graph.EmitArtifact(ctx, graph.Artifact{Name: "p01", Path: paths["png"], Caption: "first"})
			graph.EmitArtifact(ctx, graph.Artifact{Name: "notes", Path: paths["md"], Mime: "text/x-custom"})
			graph.EmitArtifact(ctx, graph.Artifact{Name: "gone", Path: paths["gone"]})
			graph.EmitArtifact(ctx, graph.Artifact{Name: "dir", Path: filepath.Dir(paths["png"])})
			graph.EmitArtifact(ctx, graph.Artifact{Name: "blob", Path: paths["bin"]})
			return nil
		}).AddEdge(graph.Start, "render").AddEdge("render", graph.End)
	r, err := g.Compile(graph.WithObserver(s))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.NoError(t, err)
}

func get(t *testing.T, url, host string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestArtifactJournaledAndServed(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{
		"png": filepath.Join(dir, "p01.png"), "md": filepath.Join(dir, "n.md"),
		"gone": filepath.Join(dir, "gone.png"), "bin": filepath.Join(dir, "x.unknownext"),
	}
	require.NoError(t, os.WriteFile(paths["png"], []byte("\x89PNGdata"), 0o600))
	require.NoError(t, os.WriteFile(paths["md"], []byte("# hi"), 0o600))
	require.NoError(t, os.WriteFile(paths["bin"], []byte("b"), 0o600))
	jp := filepath.Join(t.TempDir(), "j.jsonl")
	s := newSrv(t, WithJournal(jp))
	artRun(t, s, paths)
	ts := newTS(t, s)

	seqs := map[string]int64{}
	for _, ev := range s.Events() {
		if ev.Type == TypeArtifact {
			require.NotNil(t, ev.Artifact)
			assert.Equal(t, "render", ev.Node)
			assert.NotEmpty(t, ev.Run)
			seqs[ev.Artifact.Name] = ev.Seq
		}
	}
	require.Len(t, seqs, 5)
	for _, ev := range s.Events() {
		if ev.Type == TypeArtifact && ev.Artifact.Name == "p01" {
			assert.Equal(t, "image/png", ev.Artifact.Mime) // filled in by the server
			assert.Equal(t, "first", ev.Artifact.Caption)
		}
	}

	url := func(seq int64) string { return ts.URL + "/api/artifact/" + strconv.FormatInt(seq, 10) }
	resp, body := get(t, url(seqs["p01"]), "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	assert.Equal(t, "sandbox", resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "\x89PNGdata", string(body))

	resp, body = get(t, url(seqs["notes"]), "")
	assert.Equal(t, "text/x-custom", resp.Header.Get("Content-Type"))
	assert.Equal(t, "# hi", string(body))

	resp, _ = get(t, url(seqs["blob"]), "")
	assert.Equal(t, 200, resp.StatusCode)

	for name, u := range map[string]string{
		"missing file": url(seqs["gone"]), "directory": url(seqs["dir"]),
		"unknown seq": url(9999), "zero": url(0), "not a number": ts.URL + "/api/artifact/abc",
		"non-artifact seq": url(1),
	} {
		resp, _ = get(t, u, "")
		assert.Equal(t, 404, resp.StatusCode, name)
	}

	resp, _ = get(t, url(seqs["p01"]), "evil.example")
	assert.Equal(t, 403, resp.StatusCode)

	// Replay serves the same bytes straight from disk.
	require.NoError(t, s.Close())
	rp, err := NewReplay(jp)
	require.NoError(t, err)
	rts := newTS(t, rp)
	resp, body = get(t, rts.URL+"/api/artifact/"+strconv.FormatInt(seqs["p01"], 10), "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "\x89PNGdata", string(body))
	require.NoError(t, os.Remove(paths["png"]))
	resp, _ = get(t, rts.URL+"/api/artifact/"+strconv.FormatInt(seqs["p01"], 10), "")
	assert.Equal(t, 404, resp.StatusCode)
}

func TestGuessMime(t *testing.T) {
	assert.Equal(t, "image/png", guessMime("a.PNG"))
	assert.Contains(t, guessMime("a.md"), "markdown")
	assert.Equal(t, "application/octet-stream", guessMime("a.zzzunknown"))
	for _, ext := range []string{".json", ".webp", ".txt"} {
		assert.NotEqual(t, "application/octet-stream", guessMime("a"+ext))
	}
}

func TestReviewItemsServedAndAnswered(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "p01.png")
	require.NoError(t, os.WriteFile(p, []byte("IMG"), 0o600))
	items := []graph.ReviewItem{{Name: "p01", Path: p, Caption: "c"}, {Name: "gone", Path: filepath.Join(dir, "nope.png")}}
	s := newSrv(t)
	ts := newTS(t, s)
	g := graph.New[st]("rev").
		AddNode("letter", func(ctx context.Context, x *st) error {
			rv, ok := graph.ReviewAnswer(ctx)
			if !ok {
				return graph.InterruptReview("look", items)
			}
			x.Note = rv.Comments["p01"] + "|" + rv.General
			return nil
		}).AddEdge(graph.Start, "letter").AddEdge("letter", graph.End)
	run, err := g.Compile(graph.WithObserver(s))
	require.NoError(t, err)

	done := make(chan st, 1)
	go func() {
		final, _, err := RunInteractive(context.Background(), run, st{}, s)
		assert.NoError(t, err)
		done <- final
	}()
	var seq int64
	require.Eventually(t, func() bool {
		for _, ev := range s.Events() {
			if ev.Type == TypeInterrupt {
				seq = ev.Seq
				assert.Equal(t, items, ev.Items)
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	u := ts.URL + "/api/review/" + strconv.FormatInt(seq, 10) + "/"
	resp, body := get(t, u+"0", "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	assert.Equal(t, "IMG", string(body))
	for _, bad := range []string{"1", "2", "-1", "x"} {
		resp, _ = get(t, u+bad, "")
		assert.Equal(t, 404, resp.StatusCode, bad)
	}
	resp, _ = get(t, ts.URL+"/api/review/1/0", "") // not an interrupt event
	assert.Equal(t, 404, resp.StatusCode)
	resp, _ = get(t, ts.URL+"/api/review/zz/0", "")
	assert.Equal(t, 404, resp.StatusCode)
	resp, _ = get(t, u+"0", "evil.example")
	assert.Equal(t, 403, resp.StatusCode)

	ans, _ := json.Marshal(map[string]any{"approved": false, "comments": map[string]string{"p01": "fix"}, "general": "g"})
	body2, _ := json.Marshal(map[string]string{"answer": string(ans)})
	req, _ := http.NewRequest("POST", ts.URL+"/api/answer", bytes.NewReader(body2))
	req.Header.Set("X-Graphweb-Token", s.Token())
	r2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	r2.Body.Close()
	assert.Equal(t, 200, r2.StatusCode)
	select {
	case final := <-done:
		assert.Equal(t, "fix|g", final.Note)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not resume")
	}
}

func TestArtifactDataServed(t *testing.T) {
	dir := t.TempDir()
	md, js := filepath.Join(dir, "s.md"), filepath.Join(dir, "s.json")
	require.NoError(t, os.WriteFile(md, []byte("# Script"), 0o600))
	require.NoError(t, os.WriteFile(js, []byte(`{"a":1}`), 0o600))
	s := newSrv(t)
	s.Artifact(context.Background(), graph.StepInfo{Node: "writer", Step: 1}, graph.Artifact{Name: "script", Path: md, Mime: "text/markdown", Data: js})
	s.Artifact(context.Background(), graph.StepInfo{Node: "writer", Step: 1}, graph.Artifact{Name: "plain", Path: md})
	ts := newTS(t, s)
	var seq, plain int64
	for _, ev := range s.Events() {
		if ev.Type == TypeArtifact && ev.Artifact.Name == "script" {
			seq = ev.Seq
		}
		if ev.Type == TypeArtifact && ev.Artifact.Name == "plain" {
			plain = ev.Seq
		}
	}
	u := ts.URL + "/api/artifact/" + strconv.FormatInt(seq, 10)
	resp, body := get(t, u, "")
	assert.Equal(t, "# Script", string(body))
	assert.Equal(t, "text/markdown", resp.Header.Get("Content-Type"))
	resp, body = get(t, u+"?data=1", "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, `{"a":1}`, string(body))
	resp, _ = get(t, ts.URL+"/api/artifact/"+strconv.FormatInt(plain, 10)+"?data=1", "")
	assert.Equal(t, 404, resp.StatusCode)
}

func TestProgressJournaled(t *testing.T) {
	s := newSrv(t)
	s.Progress(context.Background(), graph.StepInfo{Node: "panels", Step: 3}, graph.Progress{Done: 12, Total: 20, Label: "render p13"})
	var got *Event
	for _, ev := range s.Events() {
		if ev.Type == TypeProgress {
			e := ev
			got = &e
		}
	}
	require.NotNil(t, got)
	assert.Equal(t, "panels", got.Node)
	assert.Equal(t, graph.Progress{Done: 12, Total: 20, Label: "render p13"}, *got.Progress)
}
