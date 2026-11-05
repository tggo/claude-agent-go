// Command pipeline runs a configurable coding graph on top of Claude Code:
//
//	plan ──▶ code ──▶ review ──approved──▶ docs ──▶ end
//	                    ▲   └──issues──▶ fix ─┘
//	                    └──────────────────────┘   (gives up after 3 rounds)
//
// Every node is a real Claude Code agent working in one directory: the coder
// writes files and runs the tests, the reviewer runs them again and approves
// through an in-process Go tool, the writer produces README.md. The planner
// may pause the graph with a question (graph.Interrupt); the answer is read
// from stdin and the graph resumes from its checkpoint.
//
// Each agent run is recorded; inspect the whole run afterwards with
//
//	go run ./cmd/cag trace <out>/runs --open
//
// Usage:
//
//	go run ./examples/pipeline -task "Write slugify(text) with Ukrainian transliteration"
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/graph"
)

func main() {
	task := flag.String("task", "Write fizzbuzz(n) returning a list of strings for 1..n; n <= 0 returns [].", "coding task")
	model := flag.String("model", "haiku", "model for every agent (haiku is cheap; sonnet writes better code)")
	out := flag.String("out", "", "output dir (default: a new temp dir)")
	flag.Parse()

	dir := *out
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "pipeline-"); err != nil {
			log.Fatal(err)
		}
	}
	work := filepath.Join(dir, "project")
	if err := os.MkdirAll(work, 0o755); err != nil {
		log.Fatal(err)
	}

	p, err := Build(*model, filepath.Join(dir, "runs"))
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()
	fmt.Printf("graph:\n%s\n", p.Graph.Mermaid())
	st, rep, err := p.Graph.Run(ctx, State{Task: *task, WorkDir: work})

	// Human in the loop: answer the planner's question, resume from checkpoint.
	in := bufio.NewReader(os.Stdin)
	for {
		var intr *graph.Interrupted[State]
		if !errors.As(err, &intr) {
			break
		}
		fmt.Printf("\n⏸  %s asks: %s\n> ", intr.Node, intr.Question)
		answer, _ := in.ReadString('\n')
		st, rep, err = p.Graph.Resume(ctx, intr.Checkpoint, strings.TrimSpace(answer))
	}
	if err != nil {
		log.Fatalf("pipeline failed: %v", err)
	}

	fmt.Printf("\npath: %s\n", strings.Join(rep.Path, " → "))
	for _, s := range rep.Steps {
		fmt.Printf("  %-7s %8s  $%.4f\n", s.Node, s.Duration.Round(time.Second), s.CostUSD)
	}
	fmt.Printf("total: $%.4f in %s, review rounds: %d\n", rep.TotalCostUSD, rep.Duration.Round(time.Second), st.Rounds)
	if st.Approved {
		fmt.Printf("approved: %s\n\n%s\n", st.Reason, st.Docs)
	} else {
		fmt.Printf("NOT approved after %d rounds; last review:\n%s\n", st.Rounds, st.Review)
	}
	fmt.Printf("\nproject: %s\ntrace:   go run ./cmd/cag trace %s --open\n", work, p.RunsDir)
}
