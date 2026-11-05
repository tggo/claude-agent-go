package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type artObs struct {
	mu   sync.Mutex
	got  []Artifact
	info []StepInfo
}

func (*artObs) NodeStart(context.Context, StepInfo)      {}
func (*artObs) NodeEnd(context.Context, StepInfo, error) {}
func (o *artObs) Artifact(_ context.Context, i StepInfo, a Artifact) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got, o.info = append(o.got, a), append(o.info, i)
}

type plainObs struct{}

func (plainObs) NodeStart(context.Context, StepInfo)      {}
func (plainObs) NodeEnd(context.Context, StepInfo, error) {}

func TestEmitArtifact(t *testing.T) {
	g := New[int]("art").
		AddNode("a", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Name: "one", Path: "/tmp/one.png", Caption: "c"})
			EmitArtifact(ctx, Artifact{Path: "rel/two.txt"})
			return nil
		}).
		AddNode("b1", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Name: "br", Path: "/tmp/br.png"})
			return nil
		}).
		AddNode("b2", func(context.Context, *int) error { return nil }).
		AddParallel("par", []string{"b1", "b2"}, func(*int, []int) error { return nil }).
		AddEdge(Start, "a").AddEdge("a", "par").AddEdge("par", End)
	obs := &artObs{}
	r, err := g.Compile(WithObserver(obs))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), 0)
	require.NoError(t, err)

	require.Len(t, obs.got, 3)
	assert.Equal(t, Artifact{Name: "one", Path: "/tmp/one.png", Caption: "c"}, obs.got[0])
	assert.Equal(t, "a", obs.info[0].Node)
	assert.NotEmpty(t, obs.info[0].RunID)
	assert.Equal(t, "two.txt", obs.got[1].Name)
	assert.True(t, filepath.IsAbs(obs.got[1].Path))
	assert.Equal(t, "b1", obs.info[2].Node)
	assert.Equal(t, "par", obs.info[2].Parent)
}

func TestEmitArtifactNoop(t *testing.T) {
	EmitArtifact(context.Background(), Artifact{Path: "/x"}) // outside a run

	g := New[int]("plain").
		AddNode("a", func(ctx context.Context, _ *int) error {
			EmitArtifact(ctx, Artifact{Path: "/x"})
			return nil
		}).AddEdge(Start, "a").AddEdge("a", End)
	for _, opts := range [][]CompileOption{nil, {WithObserver(plainObs{})}} {
		r, err := g.Compile(opts...)
		require.NoError(t, err)
		_, _, err = r.Run(context.Background(), 0)
		require.NoError(t, err)
	}
}

func TestEmitData(t *testing.T) {
	dir := t.TempDir()
	obs := &artObs{}
	g := New[int]("d").AddNode("n", func(ctx context.Context, _ *int) error {
		if err := EmitData(ctx, dir, "script_v1", "script v1", map[string]int{"panels": 3}, func() string { return "# Script\n" }, "3 panels"); err != nil {
			return err
		}
		return EmitData(ctx, dir, "raw", "", []int{1}, nil, "")
	}).AddEdge(Start, "n").AddEdge("n", End)
	r, err := g.Compile(WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(obs.got) != 2 {
		t.Fatalf("got %d artifacts", len(obs.got))
	}
	a := obs.got[0]
	if a.Mime != "text/markdown" || filepath.Base(a.Path) != "script_v1.md" || filepath.Base(a.Data) != "script_v1.json" || a.Caption != "3 panels" {
		t.Fatalf("%+v", a)
	}
	if b, _ := os.ReadFile(a.Data); !strings.Contains(string(b), `"panels": 3`) {
		t.Fatal(string(b))
	}
	if b := obs.got[1]; b.Mime != "application/json" || b.Data != "" || b.Name != "raw.json" {
		t.Fatalf("%+v", b)
	}
	// outside a run the files are still written
	if err := EmitData(context.Background(), filepath.Join(dir, "x"), "f", "f", 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := EmitData(context.Background(), dir, "bad", "bad", make(chan int), nil, ""); err == nil {
		t.Fatal("unmarshalable value must fail")
	}
}

type progObs struct {
	plainObs
	mu  sync.Mutex
	got []Progress
}

func (o *progObs) Progress(_ context.Context, i StepInfo, p Progress) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if i.Node == "n" {
		o.got = append(o.got, p)
	}
}

func TestSetProgress(t *testing.T) {
	SetProgress(context.Background(), 1, 2, "outside") // no run: no-op
	obs := &progObs{}
	g := New[int]("p").AddNode("n", func(ctx context.Context, _ *int) error {
		SetProgress(ctx, 0, 3, "start")
		SetProgress(ctx, 3, 3, "")
		return nil
	}).AddEdge(Start, "n").AddEdge("n", End)
	r, err := g.Compile(WithObserver(obs))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), 0)
	require.NoError(t, err)
	assert.Equal(t, []Progress{{0, 3, "start"}, {3, 3, ""}}, obs.got)
	// an observer without the extension is fine
	r2, _ := g.Compile(WithObserver(plainObs{}))
	_, _, err = r2.Run(context.Background(), 0)
	require.NoError(t, err)
}
