// Command artifacts shows graph.EmitArtifact: a two-node graph renders a few
// PNG "panels" and a markdown report, publishes each as it is written, and
// graphweb shows them live (thumbnails on the node, a run-wide gallery).
//
//	go run ./examples/artifacts            # then open http://127.0.0.1:8799
//
// The server keeps running after the graph finishes; stop it with Ctrl-C.
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

type state struct {
	Dir    string   `json:"dir"`
	Panels []string `json:"panels"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "graphweb listen address")
	panels := flag.Int("panels", 4, "how many panels to render")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dir, err := os.MkdirTemp("", "artifacts-")
	if err != nil {
		log.Fatal(err)
	}
	srv, err := graphweb.New()
	if err != nil {
		log.Fatal(err)
	}
	url, err := srv.Start(ctx, *addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("graph UI:", url, " files in", dir)

	g := graph.New[state]("artifacts").
		AddNode("render", func(ctx context.Context, s *state) error {
			for i := 1; i <= *panels; i++ {
				p := filepath.Join(s.Dir, fmt.Sprintf("p%02d.png", i))
				if err := writePanel(p, i); err != nil {
					return err
				}
				s.Panels = append(s.Panels, p)
				graph.EmitArtifact(ctx, graph.Artifact{Name: fmt.Sprintf("p%02d", i), Path: p, Caption: fmt.Sprintf("panel %d of %d", i, *panels)})
				time.Sleep(700 * time.Millisecond)
			}
			return nil
		}).
		AddNode("report", func(ctx context.Context, s *state) error {
			p := filepath.Join(s.Dir, "report.md")
			if err := os.WriteFile(p, []byte(fmt.Sprintf("# Report\n\n%d panels rendered.\n", len(s.Panels))), 0o600); err != nil {
				return err
			}
			graph.EmitArtifact(ctx, graph.Artifact{Name: "report", Path: p, Caption: "summary of the run"})
			return nil
		}).
		AddEdge(graph.Start, "render").AddEdge("render", "report").AddEdge("report", graph.End)
	run, err := g.Compile(graph.WithObserver(srv))
	if err != nil {
		log.Fatal(err)
	}
	if _, _, err := run.Run(ctx, state{Dir: dir}); err != nil {
		log.Fatal(err)
	}
	fmt.Println("done; Ctrl-C to stop the server")
	<-ctx.Done()
}

// writePanel draws a simple gradient so every panel looks different.
func writePanel(path string, n int) error {
	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / 320), G: uint8(y * 255 / 200), B: uint8(n * 60), A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
