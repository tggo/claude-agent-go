// Command router shows a classifier in front of several workflows, without a
// model: a scripted classifier reads each task, picks a workflow and says how
// sure it is. Each workflow is its own graph.Graph, embedded with AddSubgraph,
// so graphweb draws it as one box (a group) and the board shows every task in
// the column of the workflow it went to.
//
//	            ┌─▶ Coding    (plan → implement → review ⟲)  ─┐
//	classify ───┼─▶ Research  (search → read → summarize)     ─┼─▶ reply
//	   │        ├─▶ Marketing (brief → draft)                 ─┤
//	   │        └─▶ answer                                    ─┘
//	   └─▶ ask a person (when the classifier is unsure) ─▶ one of the above
//
//	go run ./examples/router          # opens on http://127.0.0.1:8798, press "board"
//	go run ./examples/router -auto    # answers the unsure task itself, no browser
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

// Task is the state of one run. graphweb's board shows Title, Confidence and
// Tags on the task's card.
type Task struct {
	Title      string   `json:"title"`
	Kind       string   `json:"kind,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Notes      []string `json:"notes,omitempty"`
	Rounds     int      `json:"rounds,omitempty"`
	Reply      string   `json:"reply,omitempty"`
}

// The workflows the classifier can pick, and "answer" for a task that needs
// only a reply.
const (
	Coding    = "coding"
	Research  = "research"
	Marketing = "marketing"
	Answer    = "answer"
)

var tasks = []string{
	"Fix the 502 on /api/orders after the deploy",
	"Compare three vector databases for our search",
	"Write the launch email for the new pricing page",
	"What are our support hours?",
	"Refactor the flaky checkout tests",
	"Look into the churn spike last week",
	"Social posts for the spring campaign",
	"Can we do something about onboarding?",
}

func main() {
	addr := flag.String("web", "127.0.0.1:8798", "graphweb address")
	auto := flag.Bool("auto", false, "answer the unsure task without a browser")
	step := flag.Duration("step", 700*time.Millisecond, "how long each node takes")
	flag.Parse()
	srv, err := graphweb.New()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if !*auto {
		url, err := srv.Start(ctx, *addr)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("graphweb:", url, " (press \"board\" to see every task)")
		time.Sleep(2 * time.Second) // let a browser connect
	}
	results := runAll(ctx, srv, *step, *auto)
	for _, r := range results {
		fmt.Println(r)
	}
	if !*auto {
		fmt.Println("done; Ctrl-C to quit")
		select {}
	}
}

// runAll starts every task, a little apart, and waits for all of them.
func runAll(ctx context.Context, srv *graphweb.Server, step time.Duration, auto bool) []string {
	run, err := build(step, graph.WithObserver(srv))
	if err != nil {
		log.Fatal(err)
	}
	if auto {
		go answerUnsure(ctx, srv)
	}
	out := make([]string, len(tasks))
	var wg sync.WaitGroup
	for i, title := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _, err := graphweb.RunInteractive(ctx, run, Task{Title: title}, srv)
			res := s.Reply
			if err != nil {
				res = "failed: " + err.Error()
			}
			out[i] = fmt.Sprintf("%-50s %-9s %s", title, s.Kind, res)
		}()
		time.Sleep(step * 3 / 2)
	}
	wg.Wait()
	return out
}

// answerUnsure plays the person in -auto mode: it answers the classifier's
// question with "marketing".
func answerUnsure(ctx context.Context, srv *graphweb.Server) {
	for ctx.Err() == nil {
		if srv.Answer(Marketing) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// build wires the router. Each workflow is a separate graph; AddSubgraph
// embeds it under a name used like a node name in the edges.
func build(step time.Duration, opts ...graph.CompileOption) (*graph.Runnable[Task], error) {
	work := func(note string) graph.NodeFunc[Task] {
		return func(ctx context.Context, s *Task) error {
			select {
			case <-time.After(step):
			case <-ctx.Done():
				return ctx.Err()
			}
			s.Notes = append(s.Notes, note)
			return nil
		}
	}

	coding := graph.New[Task]("Coding").
		AddNode("plan", work("planned the change")).
		AddNode("implement", func(ctx context.Context, s *Task) error {
			if err := work("implemented")(ctx, s); err != nil {
				return err
			}
			if strings.Contains(strings.ToLower(s.Title), "flaky") && s.Rounds == 1 {
				return errors.New("test run timed out after 10m")
			}
			return nil
		}).
		AddNode("review", func(ctx context.Context, s *Task) error {
			s.Rounds++
			return work(fmt.Sprintf("review round %d", s.Rounds))(ctx, s)
		}).
		AddEdge(graph.Start, "plan").AddEdge("plan", "implement").AddEdge("implement", "review").
		AddConditionalEdge("review", func(s *Task) string {
			if s.Rounds < 2 {
				return "implement"
			}
			return graph.End
		}, "implement", graph.End).
		LabelEdge("review", "implement", "changes requested")

	research := graph.New[Task]("Research").
		AddNode("search", work("found 6 sources")).
		AddNode("read", work("read them")).
		AddNode("summarize", work("summarized")).
		AddEdge(graph.Start, "search").AddEdge("search", "read").AddEdge("read", "summarize").AddEdge("summarize", graph.End)

	marketing := graph.New[Task]("Marketing").
		AddNode("brief", work("wrote the brief")).
		AddNode("draft", work("drafted the copy")).
		AddEdge(graph.Start, "brief").AddEdge("brief", "draft").AddEdge("draft", graph.End)

	pick := func(s *Task) string {
		if s.Confidence < 0.6 {
			return "ask"
		}
		return s.Kind
	}
	g := graph.New[Task]("support-router").
		AddAgentNode("classify", func(ctx context.Context, s *Task) error {
			if err := work("classified")(ctx, s); err != nil {
				return err
			}
			s.Kind, s.Confidence, s.Tags = classify(s.Title)
			return nil
		}).
		AddNode("ask", func(ctx context.Context, s *Task) error {
			a, ok := graph.Answer(ctx)
			if !ok {
				return graph.Interrupt(fmt.Sprintf("Not sure where %q belongs (%.0f%%). coding, research, marketing or answer?", s.Title, s.Confidence*100))
			}
			a = strings.ToLower(strings.TrimSpace(a))
			switch a {
			case Coding, Research, Marketing, Answer:
				s.Kind, s.Confidence = a, 1
				s.Tags = append(s.Tags, "routed by a person")
				return nil
			}
			return graph.Interrupt(fmt.Sprintf("%q is not a workflow. coding, research, marketing or answer?", a))
		}).
		AddSubgraph(Coding, coding).
		AddSubgraph(Research, research).
		AddSubgraph(Marketing, marketing).
		AddNode(Answer, work("answered from the FAQ")).
		AddNode("reply", func(ctx context.Context, s *Task) error {
			if err := work("replied")(ctx, s); err != nil {
				return err
			}
			s.Reply = strings.Join(s.Notes, ", ")
			return nil
		}).
		AddEdge(graph.Start, "classify").
		AddConditionalEdge("classify", pick, Coding, Research, Marketing, Answer, "ask").
		AddConditionalEdge("ask", func(s *Task) string { return s.Kind }, Coding, Research, Marketing, Answer).
		LabelEdge("classify", "ask", "unsure").
		AddEdge(Coding, "reply").
		AddEdge(Research, "reply").
		AddEdge(Marketing, "reply").
		AddEdge(Answer, "reply").
		AddEdge("reply", graph.End)
	for _, k := range []string{Coding, Research, Marketing, Answer} {
		g.LabelEdge("classify", k, k)
	}
	return g.Compile(opts...)
}

// classify stands in for a model call: keyword rules with a confidence.
func classify(title string) (kind string, confidence float64, tags []string) {
	t := strings.ToLower(title)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(t, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("502", "fix", "refactor", "test", "bug"):
		kind, confidence = Coding, 0.94
		if has("502") {
			tags = []string{"outage", "api"}
		}
		if has("flaky", "test") {
			tags = []string{"tests"}
		}
	case has("compare", "look into", "spike"):
		kind, confidence, tags = Research, 0.82, []string{"analysis"}
	case has("email", "posts", "campaign", "launch"):
		kind, confidence, tags = Marketing, 0.9, []string{"copy"}
	case has("hours", "?") && !has("something"):
		kind, confidence, tags = Answer, 0.97, []string{"faq"}
	default:
		kind, confidence = Answer, 0.41
	}
	return kind, confidence, tags
}
