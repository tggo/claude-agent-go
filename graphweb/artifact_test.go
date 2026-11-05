package graphweb

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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
