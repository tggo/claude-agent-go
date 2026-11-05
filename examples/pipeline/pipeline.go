package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/mcp"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/tools"
)

// State flows through the graph. Agents do the real work on disk in WorkDir
// (they write files and run tests with Bash); the state carries the decisions.
type State struct {
	Task     string `json:"task"`
	WorkDir  string `json:"work_dir"`
	Plan     string `json:"plan"`
	Review   string `json:"review"`
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
	Rounds   int    `json:"rounds"`
	Docs     string `json:"docs"`
	Answers  string `json:"answers"` // clarifications collected from the user
}

// MaxRounds bounds the review → fix loop.
const MaxRounds = 3

// Pipeline is the compiled graph plus the resources it needs.
type Pipeline struct {
	Graph   *graph.Runnable[State]
	RunsDir string // one recording per agent node; view with `cag trace`
	close   func()
}

// Close stops the in-process tool server.
func (p *Pipeline) Close() { p.close() }

// approvals is the bridge between the in-process `approve` tool (called by the
// reviewer agent over MCP) and the review node. Keyed by work dir so several
// pipelines can share one tool server.
type approvals struct {
	mu sync.Mutex
	m  map[string]string // workDir -> reason
}

func (a *approvals) take(workDir string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.m[workDir]
	delete(a.m, workDir)
	return r, ok
}

type approveIn struct {
	WorkDir string `json:"work_dir" jsonschema:"the project directory you reviewed"`
	Reason  string `json:"reason" jsonschema:"one sentence: why the code is acceptable"`
}

type approveOut struct {
	Approved bool `json:"approved"`
}

// Build wires plan → code → review → (fix → review)* → docs.
func Build(model, runsDir string) (*Pipeline, error) {
	ap := &approvals{m: map[string]string{}}
	reg := tools.NewRegistry("pipeline")
	tools.Register(reg, "approve",
		"Approve the code in work_dir. Call ONLY after the tests ran and passed and no issues remain.",
		func(_ context.Context, in approveIn) (approveOut, error) {
			ap.mu.Lock()
			ap.m[filepath.Clean(in.WorkDir)] = in.Reason
			ap.mu.Unlock()
			return approveOut{Approved: true}, nil
		})
	srv, err := reg.Serve()
	if err != nil {
		return nil, err
	}
	name, cfg := srv.Config()
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		srv.Close()
		return nil, err
	}
	mcpPath := filepath.Join(runsDir, "mcp.json")
	if err := mcp.WriteConfig(mcpPath, map[string]mcp.ServerConfig{name: cfg}); err != nil {
		srv.Close()
		return nil, err
	}
	approveTool := "mcp__" + name + "__approve"

	// One runner per role: each gets only the tools it needs.
	base := []runner.Option{
		runner.WithModel(model),
		runner.WithTimeout(10 * time.Minute),
		runner.WithMaxBudgetUSD("2.00"),
		runner.WithRecorder(recording.Dir(runsDir)),
		runner.WithEntrypoint("claude-agent-go-pipeline"),
	}
	role := func(maxTurns int, allowed ...string) *runner.Runner {
		return runner.New(append(base, runner.WithMaxTurns(maxTurns), runner.WithAllowedTools(allowed...))...)
	}
	planner := role(3, "Read", "Glob", "Grep")
	coder := role(25, "Read", "Write", "Edit", "Bash", "Glob", "Grep")
	reviewer := role(15, "Read", "Bash", "Glob", "Grep", approveTool)
	writer := role(10, "Read", "Write", "Glob")

	// Survive a dropped stream by resuming the session instead of redoing work.
	retry := graph.WithRetry(runner.RetryPolicy{MaxAttempts: 2, Resume: true})

	g := graph.New[State]("coding-pipeline")

	// plan: may stop the graph to ask the user (graph.Interrupt), but only when
	// the task cannot be started without an answer.
	g.AddNode("plan", func(ctx context.Context, s *State) error {
		if a, ok := graph.Answer(ctx); ok {
			s.Answers += "\n- " + a
		}
		prompt := "Task: " + s.Task + "\n"
		if s.Answers != "" {
			prompt += "User clarifications:" + s.Answers + "\n"
		}
		prompt += `
Write a short numbered implementation plan (3-7 steps) for a small Python 3
stdlib-only module in the current directory: public API, edge cases, and the
unittest cases that must pass. Prefer reasonable assumptions and list them under
"Assumptions". ONLY if the task is impossible to start without an answer (e.g.
requirements contradict each other) reply with a single line starting with
"QUESTION:" and nothing else.`
		res, err := planner.RunStreamWithRetry(ctx, runner.Input{Prompt: prompt, WorkDir: s.WorkDir, Label: "plan"}, nil,
			runner.RetryPolicy{MaxAttempts: 2, Resume: true})
		if res != nil {
			graph.AddCost(ctx, res.TotalCostUSD)
		}
		if err != nil {
			return err
		}
		text := strings.TrimSpace(res.ResultText)
		if q, ok := strings.CutPrefix(text, "QUESTION:"); ok {
			return graph.Interrupt(strings.TrimSpace(q))
		}
		s.Plan = text
		return nil
	})

	g.AddNode("code", graph.Agent(coder, func(s *State) runner.Input {
		return runner.Input{WorkDir: s.WorkDir, Prompt: "Implement this plan in the current directory.\n\n" + s.Plan + `

Create the module and a test file (unittest). Run the tests with
"python3 -m unittest -v" and iterate until they pass. Reply with a two-line
summary: files written, test result.`}
	}, nil, retry))

	g.AddNode("review", graph.Agent(reviewer, func(s *State) runner.Input {
		return runner.Input{WorkDir: s.WorkDir, MCPConfigPath: mcpPath, Prompt: "You are a strict code reviewer. Project dir: " + s.WorkDir + "\nPlan:\n" + s.Plan + `

1. Read the code and run "python3 -m unittest -v" yourself.
2. Check the plan's edge cases are implemented and tested, and the code is clear.
3. If everything is fine, call the approve tool with work_dir="` + s.WorkDir + `".
4. Otherwise reply with a numbered list of concrete issues (include failing
   output). Do not edit files.`}
	}, func(s *State, res *runner.Result) error {
		s.Review = res.ResultText
		s.Reason, s.Approved = ap.take(filepath.Clean(s.WorkDir))
		return nil
	}, retry))

	g.AddConditionalEdge("review", func(s *State) string {
		switch {
		case s.Approved:
			return "docs"
		case s.Rounds >= MaxRounds:
			return graph.End // give up: no docs for unapproved code
		default:
			return "fix"
		}
	}, "docs", "fix", graph.End)

	g.AddNode("fix", graph.Agent(coder, func(s *State) runner.Input {
		return runner.Input{WorkDir: s.WorkDir, Prompt: "Fix the code in the current directory according to this review:\n\n" + s.Review +
			"\n\nRun \"python3 -m unittest -v\" until the tests pass. Reply with a short summary of the changes."}
	}, func(s *State, _ *runner.Result) error {
		s.Rounds++
		return nil
	}, retry))

	g.AddNode("docs", graph.Agent(writer, func(s *State) runner.Input {
		return runner.Input{WorkDir: s.WorkDir, Prompt: `Read the module in the current directory and write README.md:
title, one-paragraph summary, "## Usage" with an example, "## API" for each
public function (params, returns, raises), "## Edge cases". Reply "done".`}
	}, func(s *State, _ *runner.Result) error {
		b, err := os.ReadFile(filepath.Join(s.WorkDir, "README.md"))
		if err != nil {
			return fmt.Errorf("docs agent did not write README.md: %w", err)
		}
		s.Docs = string(b)
		return nil
	}, retry))

	g.AddEdge(graph.Start, "plan").
		AddEdge("plan", "code").
		AddEdge("code", "review").
		AddEdge("fix", "review").
		AddEdge("docs", graph.End)

	r, err := g.Compile(graph.WithMaxSteps(20))
	if err != nil {
		srv.Close()
		return nil, err
	}
	// cag trace shows this diagram above the per-node timelines.
	if err := os.WriteFile(filepath.Join(runsDir, "graph.mmd"), []byte(r.Mermaid()), 0o644); err != nil {
		srv.Close()
		return nil, err
	}
	return &Pipeline{Graph: r, RunsDir: runsDir, close: func() { srv.Close() }}, nil
}
