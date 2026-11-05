package main

import (
	"context"
	"flag"
	"testing"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/eval"
	"github.com/tggo/claude-agent-go/internal/goldentest"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from testdata/*.jsonl")

// testdata/happy.jsonl is one real run of the order-taker agent (haiku via
// cerber) for "two large pepperoni to Khreshchatyk 22, Kyiv". The init event
// was trimmed of local paths/plugins. The tools are real in-process MCP, so a
// replay does not call them again: these tests pin what the agent did.

func loadHappy(t *testing.T) *recording.Recording {
	t.Helper()
	rec, err := recording.ReadFile("testdata/happy.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReplayHappyInvariants(t *testing.T) {
	if vs := claudecli.CheckInvariants(loadHappy(t).Events()); len(vs) != 0 {
		t.Errorf("violations: %v", vs)
	}
}

func TestReplayHappyTrajectory(t *testing.T) {
	run := eval.FromRecording(loadHappy(t))
	want := []eval.ToolCall{{Name: "get_menu"}, {Name: "quote_delivery"}, {Name: "propose_order"}}
	if !eval.MatchTrajectory(run.ToolCalls, want, eval.InOrder, true, eval.MCPNames()) {
		t.Errorf("trajectory = %+v", run.ToolCalls)
	}
	if len(run.ToolCalls) != 3 {
		t.Errorf("want exactly 3 calls, got %d", len(run.ToolCalls))
	}
	if run.Response == "" || run.Metrics.CostUSD <= 0 {
		t.Errorf("response=%q cost=%v", run.Response, run.Metrics.CostUSD)
	}
}

func TestReplayHappyGolden(t *testing.T) {
	got := goldentest.Summary(loadHappy(t), goldentest.Options{MaxLen: 400})
	goldentest.Compare(t, "testdata/happy.golden", got, *update)
}

// The recording replays through the runner both ways (clitest, no sed; and
// recording.ReplayTransport) and yields the same run.
func TestReplayHappyThroughRunner(t *testing.T) {
	rec := loadHappy(t)
	want := eval.FromRecording(rec)

	check := func(t *testing.T, r *runner.Runner) {
		t.Helper()
		col := eval.NewCollector()
		res, err := r.RunStream(context.Background(), runner.Input{Prompt: "ignored"}, col.Progress)
		if err != nil {
			t.Fatal(err)
		}
		got := col.Run()
		if res.ResultText != want.Response || res.SessionID != want.SessionID {
			t.Errorf("result %q / %q, want %q / %q", res.ResultText, res.SessionID, want.Response, want.SessionID)
		}
		if !eval.MatchTrajectory(got.ToolCalls, want.ToolCalls, eval.Exact, false) {
			t.Errorf("trajectory %+v, want %+v", got.ToolCalls, want.ToolCalls)
		}
	}
	t.Run("clitest", func(t *testing.T) {
		check(t, runner.New(runner.WithBinary(clitest.FromRecording(t, "testdata/happy.jsonl"))))
	})
	t.Run("transport", func(t *testing.T) {
		check(t, runner.New(runner.WithTransport(recording.ReplayTransport{Path: "testdata/happy.jsonl"})))
	})
}
