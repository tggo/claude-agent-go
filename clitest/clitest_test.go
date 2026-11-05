package clitest_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
)

func stream(t *testing.T, bin string, opts ...runner.Option) (*runner.Result, []claudecli.StreamEvent, error) {
	t.Helper()
	var evs []claudecli.StreamEvent
	r := runner.New(append([]runner.Option{runner.WithBinary(bin)}, opts...)...)
	res, err := r.RunStream(context.Background(), runner.Input{Prompt: "order pizza"},
		func(ev claudecli.StreamEvent, _ int) { evs = append(evs, ev) })
	return res, evs, err
}

func TestFakeCLIStream(t *testing.T) {
	bin := clitest.FakeCLI(t,
		clitest.RecordStdin(),
		clitest.Init("sess-1"),
		clitest.Text("looking"),
		clitest.ToolUse("t1", "get_menu", ""),
		clitest.ToolResult("t1", "it's a \"menu\"\nwith lines"),
		clitest.ToolUse("t2", "bad", `{not json`),
		clitest.ToolResult("t2", "x"),
		clitest.Raw(`{"type":"rate_limit_event"}`),
		clitest.Result("done", 0.25),
	)
	res, evs, err := stream(t, bin)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "done" || res.SessionID != "sess-1" || res.TotalCostUSD != 0.25 {
		t.Errorf("result = %+v", res)
	}
	if vs := claudecli.CheckInvariants(evs); len(vs) != 0 {
		t.Errorf("violations: %v", vs)
	}
	var sessions int
	for _, ev := range evs {
		if ev.SessionID == "sess-1" {
			sessions++
		}
	}
	if sessions < 6 {
		t.Errorf("session id stamped on %d events", sessions)
	}
	if got := clitest.Stdin(t, bin); !strings.Contains(got, "order pizza") {
		t.Errorf("stdin = %q", got)
	}
	args := clitest.Args(t, bin)
	if len(args) == 0 || !contains(args, "stream-json") {
		t.Errorf("args = %v", args)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestFakeCLIArgsPreserved(t *testing.T) {
	bin := clitest.FakeCLI(t, clitest.Init("s"), clitest.Result("ok", 0))
	if _, _, err := stream(t, bin, runner.WithModel("my model's name")); err != nil {
		t.Fatal(err)
	}
	args := clitest.Args(t, bin)
	i := -1
	for j, a := range args {
		if a == "--model" {
			i = j
		}
	}
	if i < 0 || args[i+1] != "my model's name" {
		t.Errorf("args = %q", args)
	}
}

func TestFakeCLIExitStderrSleep(t *testing.T) {
	bin := clitest.FakeCLI(t,
		clitest.Init("s"),
		clitest.Stderr("boom: it's bad"),
		clitest.Sleep(1500*time.Microsecond),
		clitest.ExitCode(3),
	)
	_, _, err := stream(t, bin)
	if err == nil {
		t.Fatal("want error for exit 3")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr not in error: %v", err)
	}
}

func TestFakeCLISleepTimeout(t *testing.T) {
	bin := clitest.FakeCLI(t, clitest.Init("s"), clitest.Sleep(5*time.Second), clitest.Result("late", 0))
	start := time.Now()
	_, _, err := stream(t, bin, runner.WithTimeout(300*time.Millisecond))
	if err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
	// whole-second sleeps take the integer path
	clitest.FakeCLI(t, clitest.Sleep(2*time.Second))
}

func TestArgsEmpty(t *testing.T) {
	bin := clitest.FakeCLI(t)
	if err := runBare(bin); err != nil {
		t.Fatal(err)
	}
	if a := clitest.Args(t, bin); a != nil {
		t.Errorf("args = %v", a)
	}
}

func TestFromRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.NewWriter(f, recording.Header{Label: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{
		`{"type":"system","subtype":"init","session_id":"r1"}`,
		`{"type":"assistant","session_id":"r1","message":{"content":[{"type":"tool_use","id":"u1","name":"n","input":{"a":"it's"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"ok"}]}}`,
		`{"type":"result","session_id":"r1","result":"replayed","total_cost_usd":0.5}`,
	} {
		if err := w.WriteLine([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bin := clitest.FromRecording(t, path)
	res, evs, err := stream(t, bin)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "replayed" || len(evs) != 4 {
		t.Errorf("res=%+v events=%d", res, len(evs))
	}
	if vs := claudecli.CheckInvariants(evs); len(vs) != 0 {
		t.Errorf("violations: %v", vs)
	}
	want, _ := recording.ReplayLines(path)
	var got [][]byte
	for _, ev := range evs {
		got = append(got, ev.Raw)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replayed lines differ:\n%s\nvs\n%s", got, want)
	}

	// extra steps run after the body
	bin = clitest.FromRecording(t, path, clitest.Stderr("tail"), clitest.ExitCode(2))
	if _, _, err := stream(t, bin); err == nil {
		t.Error("want exit error")
	}
}

func TestReplayLinesErrors(t *testing.T) {
	if _, err := recording.ReplayLines(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("want error")
	}
}
