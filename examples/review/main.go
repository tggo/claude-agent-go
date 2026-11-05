// Command review shows graph.InterruptReview: a node renders a few PNG panels
// and pauses; the operator sees them all in the graphweb UI, writes a comment
// under any panel (or approves all), and the graph continues with the Review.
//
//	go run ./examples/review     # then open http://127.0.0.1:8799
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

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

type state struct {
	Dir     string        `json:"dir"`
	Verdict *graph.Review `json:"verdict,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "graphweb listen address")
	n := flag.Int("panels", 6, "number of panels")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dir, err := os.MkdirTemp("", "review-")
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
	fmt.Println("graph UI:", url)

	g := graph.New[state]("review").
		AddNode("render", func(ctx context.Context, s *state) error {
			for i := 1; i <= *n; i++ {
				p := filepath.Join(s.Dir, fmt.Sprintf("p%02d.png", i))
				if err := writePanel(p, i); err != nil {
					return err
				}
			}
			return nil
		}).
		AddNode("letter", func(ctx context.Context, s *state) error {
			rv, answered := graph.ReviewAnswer(ctx)
			if !answered {
				var items []graph.ReviewItem
				for i := 1; i <= *n; i++ {
					items = append(items, graph.ReviewItem{Name: fmt.Sprintf("p%02d", i),
						Path: filepath.Join(s.Dir, fmt.Sprintf("p%02d.png", i)), Caption: fmt.Sprintf("panel %d, lettered", i)})
				}
				return graph.InterruptReview("Episode is rendered and lettered. Anything to fix?", items)
			}
			s.Verdict = &rv
			return nil
		}).
		AddEdge(graph.Start, "render").AddEdge("render", "letter").AddEdge("letter", graph.End)
	run, err := g.Compile(graph.WithObserver(srv))
	if err != nil {
		log.Fatal(err)
	}
	final, _, err := graphweb.RunInteractive(ctx, run, state{Dir: dir}, srv)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("review: %+v\n", *final.Verdict)
	<-ctx.Done()
}

func writePanel(path string, n int) error {
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / 640), G: uint8(y * 255 / 400), B: uint8(n * 40), A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
