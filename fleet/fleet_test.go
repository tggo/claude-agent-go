package fleet

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tggo/claude-agent-go/runner"
)

// fakeRunner returns a runner backed by a fake claude that succeeds (cost 0.02)
// unless the prompt contains "FAIL", in which case it exits non-zero.
func fakeRunner(t *testing.T) *runner.Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
in=$(cat)
case "$in" in
  *FAIL*) echo 'boom' >&2; exit 1 ;;
esac
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"OK","total_cost_usd":0.02}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return runner.New(runner.WithBinary(bin))
}

func twoWorkers(t *testing.T) []Worker {
	r := fakeRunner(t)
	return []Worker{{Name: "w1", Runner: r}, {Name: "w2", Runner: r}}
}

func TestFleetFanOut(t *testing.T) {
	f, err := New(Config{Workers: twoWorkers(t)})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []Task{
		{ID: "a", Input: runner.Input{Prompt: "one"}},
		{ID: "b", Input: runner.Input{Prompt: "two"}},
		{ID: "c", Input: runner.Input{Prompt: "three"}},
	}
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 3 || rep.Failed != 0 || rep.Skipped != 0 {
		t.Errorf("report = %+v", rep)
	}
	if diff := rep.TotalCostUSD - 0.06; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("TotalCostUSD = %v, want 0.06", rep.TotalCostUSD)
	}
}

func TestFleetDependencies(t *testing.T) {
	f, _ := New(Config{Workers: twoWorkers(t)})
	// b and c depend on a — a must complete (and be emitted) before them.
	tasks := []Task{
		{ID: "b", Input: runner.Input{Prompt: "b"}, DependsOn: []string{"a"}},
		{ID: "a", Input: runner.Input{Prompt: "a"}},
		{ID: "c", Input: runner.Input{Prompt: "c"}, DependsOn: []string{"a"}},
	}
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 || rep.Skipped != 0 || len(rep.Results) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	// a is emitted before b and c (it must complete before they can be claimed).
	order := map[string]int{}
	for i, r := range rep.Results {
		order[r.TaskID] = i
	}
	if order["a"] > order["b"] || order["a"] > order["c"] {
		t.Errorf("dependency order violated: %v", order)
	}
}

func TestFleetFailedDependencySkips(t *testing.T) {
	f, _ := New(Config{Workers: twoWorkers(t)})
	tasks := []Task{
		{ID: "a", Input: runner.Input{Prompt: "please FAIL"}},
		{ID: "b", Input: runner.Input{Prompt: "b"}, DependsOn: []string{"a"}},
	}
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Errorf("Failed = %d, want 1", rep.Failed)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (b blocked by failed a)", rep.Skipped)
	}
	for _, r := range rep.Results {
		if r.TaskID == "b" && !r.Skipped {
			t.Errorf("b should be skipped, got %+v", r)
		}
	}
}

func TestFleetSpendCap(t *testing.T) {
	f, _ := New(Config{Workers: twoWorkers(t), MaxSpendUSD: 0.05})
	// 5 tasks × $0.02 = $0.10, but cap is $0.05 → some skipped.
	var tasks []Task
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		tasks = append(tasks, Task{ID: id, Input: runner.Input{Prompt: id}})
	}
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	// Soft cap: workers stop claiming once spent >= 0.05, but up to len(Workers)
	// tasks already in flight may finish — so max ≈ cap + workers*cost.
	if rep.TotalCostUSD > 0.05+2*0.02+1e-9 {
		t.Errorf("spend %.4f overshot the soft cap materially", rep.TotalCostUSD)
	}
	if rep.Skipped == 0 {
		t.Errorf("expected some tasks skipped under the cap; report=%+v", rep)
	}
}

func TestFleetValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("no workers should error")
	}
	if _, err := New(Config{Workers: []Worker{{Name: "x"}}}); err == nil {
		t.Error("nil runner should error")
	}
	if _, err := New(Config{Workers: twoWorkers(t), Repo: "/x"}); err == nil {
		t.Error("Repo without Workspace should error")
	}
}
