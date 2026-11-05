package main

import (
	"context"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/eval"
	"github.com/tggo/claude-agent-go/internal/goldentest"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
)

var update = flag.Bool("update", false, "rewrite testdata/pipeline.golden from testdata/*.jsonl")

// testdata/ holds one real fizzbuzz run, one recording per graph node in
// execution order (plan, code, review, docs; haiku via cerber). Init events
// were trimmed of local paths/plugins and the temp dir became /work.

var nodes = []string{"1-plan", "2-code", "3-review", "4-docs"}

func loadNode(t *testing.T, n string) *recording.Recording {
	t.Helper()
	rec, err := recording.ReadFile(filepath.Join("testdata", n+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReplayPipelineInvariants(t *testing.T) {
	for _, n := range nodes {
		t.Run(n, func(t *testing.T) {
			if vs := claudecli.CheckInvariants(loadNode(t, n).Events()); len(vs) != 0 {
				t.Errorf("violations: %v", vs)
			}
		})
	}
}

func TestReplayPipelineTrajectory(t *testing.T) {
	all, err := eval.FromRecordingDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	// The combined trajectory of the graph: write code, run it, approve
	// through the in-process Go tool (extra calls like ToolSearch are fine).
	want := []eval.ToolCall{{Name: "Write"}, {Name: "Bash"}, {Name: "approve"}}
	if !eval.MatchTrajectory(all.ToolCalls, want, eval.InOrder, true, eval.MCPNames()) {
		t.Errorf("trajectory = %+v", all.ToolCalls)
	}
}

func TestReplayPipelineGolden(t *testing.T) {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString("== " + n + "\n")
		b.WriteString(goldentest.Summary(loadNode(t, n), goldentest.Options{MaxLen: 120}))
	}
	goldentest.Compare(t, "testdata/pipeline.golden", b.String(), *update)
}

// Every node's recording replays through the runner via clitest.
func TestReplayPipelineThroughRunner(t *testing.T) {
	for _, n := range nodes {
		t.Run(n, func(t *testing.T) {
			want := eval.FromRecording(loadNode(t, n))
			bin := clitest.FromRecording(t, filepath.Join("testdata", n+".jsonl"))
			res, err := runner.New(runner.WithBinary(bin)).RunStream(context.Background(), runner.Input{Prompt: "ignored"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.ResultText != want.Response {
				t.Errorf("result %q, want %q", res.ResultText, want.Response)
			}
		})
	}
}
