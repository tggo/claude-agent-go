package graph_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tggo/claude-agent-go/graph"
)

type Work struct {
	Plan    string
	Code    string
	Rounds  int
	Verdict string
	Docs    string
}

// A plan -> code -> review loop -> docs flow with plain Go nodes. Swap any
// node for graph.Agent to have Claude Code do the work.
func Example() {
	g := graph.New[Work]("ship").
		AddNode("plan", graph.Func(func(w *Work) error { w.Plan = "add a /health endpoint"; return nil })).
		AddNode("code", graph.Func(func(w *Work) error { w.Code = "v" + fmt.Sprint(w.Rounds+1); return nil })).
		AddNode("review", graph.Func(func(w *Work) error {
			w.Rounds++
			w.Verdict = "changes requested"
			if w.Rounds >= 2 {
				w.Verdict = "approved"
			}
			return nil
		})).
		AddNode("docs", graph.Func(func(w *Work) error {
			w.Docs = strings.ToUpper(w.Plan) + " (" + w.Code + ")"
			return nil
		})).
		AddEdge(graph.Start, "plan").
		AddEdge("plan", "code").
		AddEdge("code", "review").
		AddConditionalEdge("review", func(w *Work) string {
			if w.Verdict == "approved" {
				return "docs"
			}
			return "code" // fix and review again
		}, "code", "docs").
		AddEdge("docs", graph.End)

	run, err := g.Compile(graph.WithMaxSteps(20))
	if err != nil {
		panic(err)
	}
	final, report, err := run.Run(context.Background(), Work{})
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Join(report.Path, " -> "))
	fmt.Println(final.Docs)
	// Output:
	// plan -> code -> review -> code -> review -> docs
	// ADD A /HEALTH ENDPOINT (v2)
}

// A node pauses for a human with graph.Interrupt; the Checkpoint can be stored
// as JSON and handed back to Resume later.
func ExampleRunnable_Resume() {
	type S struct{ Answer string }
	g := graph.New[S]("hitl").
		AddNode("ask", func(ctx context.Context, s *S) error {
			if a, ok := graph.Answer(ctx); ok {
				s.Answer = a
				return nil
			}
			return graph.Interrupt("deploy to prod?")
		}).
		AddEdge(graph.Start, "ask").AddEdge("ask", graph.End)
	run, _ := g.Compile()

	_, _, err := run.Run(context.Background(), S{})
	var paused *graph.Interrupted[S]
	if errors.As(err, &paused) {
		fmt.Println("question:", paused.Question)
	}
	final, _, _ := run.Resume(context.Background(), paused.Checkpoint, "yes")
	fmt.Println("answer:", final.Answer)
	// Output:
	// question: deploy to prod?
	// answer: yes
}

func ExampleRunnable_Mermaid() {
	run, _ := graph.New[Work]("m").
		AddNode("a", graph.Func(func(*Work) error { return nil })).
		AddEdge(graph.Start, "a").AddEdge("a", graph.End).Compile()
	fmt.Print(run.Mermaid())
	// Output:
	// flowchart TD
	//     __start__(["start"])
	//     __end__(["end"])
	//     a["a"]
	//     __start__ --> a
	//     a --> __end__
}
