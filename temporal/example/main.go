// Command example runs a small three-node graph as a Temporal workflow:
// fetch -> review (pauses for a human answer) -> publish.
//
//	temporal server start-dev            # in another terminal
//	go run ./example                     # worker + starter in one process
//	# in a third terminal, when it prints the question:
//	go run ./example -answer yes -id demo-1
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/tggo/claude-agent-go/graph"
	cagt "github.com/tggo/claude-agent-go/temporal"
)

type State struct {
	Title     string   `json:"title"`
	Verdict   string   `json:"verdict"`
	Trail     []string `json:"trail"`
	Published bool     `json:"published"`
}

func build() (*graph.Runnable[State], error) {
	return graph.New[State]("publish").
		AddNode("fetch", func(ctx context.Context, s *State) error {
			// graph.IdempotencyKey(ctx) is "<workflow id>:fetch:1": pass it to any
			// external system so a retried node does not act twice.
			s.Title = "draft for " + graph.IdempotencyKey(ctx)
			s.Trail = append(s.Trail, "fetch")
			return nil
		}).
		AddNode("review", func(ctx context.Context, s *State) error {
			if a, ok := graph.Answer(ctx); ok {
				s.Verdict = a
				s.Trail = append(s.Trail, "review")
				return nil
			}
			return graph.Interrupt("publish " + s.Title + "? (yes/no)")
		}).
		AddNode("publish", func(_ context.Context, s *State) error {
			s.Published = s.Verdict == "yes"
			s.Trail = append(s.Trail, "publish")
			return nil
		}).
		AddEdge(graph.Start, "fetch").AddEdge("fetch", "review").AddEdge("review", "publish").AddEdge("publish", graph.End).
		Compile()
}

func main() {
	addr := flag.String("addr", client.DefaultHostPort, "Temporal frontend address")
	id := flag.String("id", "demo-1", "workflow id")
	answer := flag.String("answer", "", "send this answer to a paused workflow and exit")
	flag.Parse()

	r, err := build()
	if err != nil {
		log.Fatal(err)
	}
	c, err := client.Dial(client.Options{HostPort: *addr})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	if *answer != "" {
		if err := cagt.Answer(ctx, c, *id, *answer); err != nil {
			log.Fatal(err)
		}
		fmt.Println("answer sent")
		return
	}

	w := worker.New(c, cagt.TaskQueue(r), worker.Options{})
	cagt.Register(w, r,
		cagt.WithActivityTimeout(10*time.Minute),
		cagt.WithHeartbeat(30*time.Second, 10*time.Second))
	if err := w.Start(); err != nil {
		log.Fatal(err)
	}
	defer w.Stop()

	run, err := cagt.Start(ctx, c, r, State{}, cagt.StartOptions{ID: *id})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("started workflow %s; waiting for review (run again with -answer yes -id %s)\n", run.GetID(), *id)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	final, res, err := cagt.Wait[State](ctx, run)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("done: %+v cost=$%.4f path=%v\n", final, res.TotalCostUSD, res.Path)
}
