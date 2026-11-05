//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tggo/claude-agent-go/eval"
)

// TestIntegrationPipeline runs the whole graph with real Claude Code agents
// and evaluates it with the eval package: the combined trajectory of all
// nodes must write code, run it, and approve through the in-process Go tool,
// and the docs node must have produced README.md for approved code.
//
//	go test -tags integration ./examples/pipeline/   (~5 cents with haiku)
func TestIntegrationPipeline(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	agent := func(ctx context.Context, c eval.Case) (eval.Run, error) {
		dir := t.TempDir()
		work := filepath.Join(dir, "project")
		os.MkdirAll(work, 0o755)
		p, err := Build("haiku", filepath.Join(dir, "runs"))
		if err != nil {
			return eval.Run{}, err
		}
		defer p.Close()

		st, rep, err := p.Graph.Run(ctx, State{Task: c.Prompt, WorkDir: work})
		if err != nil {
			return eval.Run{}, err
		}
		t.Logf("path=%v cost=$%.4f rounds=%d", rep.Path, rep.TotalCostUSD, st.Rounds)

		// One recording per node → one combined Run for the whole graph.
		all, err := eval.FromRecordingDir(p.RunsDir)
		if err != nil {
			return eval.Run{}, err
		}
		all.Response = st.Docs // what the pipeline delivers
		return all, nil
	}

	set := eval.Set{ID: "pipeline", Cases: []eval.Case{{
		ID:     "fizzbuzz",
		Prompt: "Write fizzbuzz(n) returning a list of strings for 1..n; n <= 0 returns [].",
		ExpectedTools: []eval.ToolCall{
			{Name: "Write"}, {Name: "Bash"}, {Name: "approve"},
		},
		Match:            eval.InOrder,
		IgnoreArgs:       true,
		ResponseContains: []string{"fizzbuzz", "## Usage", "## API"},
	}}}

	eval.RunT(t, set, agent, eval.Criteria{
		TrajectoryThreshold: eval.Threshold(1.0),
		MatchOptions:        []eval.MatchOption{eval.MCPNames()},
		MaxCostUSD:          1.00,
	})
}
