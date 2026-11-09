package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tggo/claude-agent-go/runner"
)

// costRunner is a fake claude that spends $3 per task, or less when
// --max-budget-usd allows less (the CLI's own budget stop), after a short
// sleep so every worker has a task in flight before any finishes.
func costRunner(t *testing.T) *runner.Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
cat >/dev/null
cost=3
while [ $# -gt 0 ]; do
  if [ "$1" = "--max-budget-usd" ] && awk "BEGIN{exit !($2 < 3)}"; then cost=$2; fi
  shift
done
sleep 0.2
printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"s\",\"result\":\"OK\",\"total_cost_usd\":$cost}"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runner.New(runner.WithBinary(bin), runner.WithMaxBudgetUSD("3.00"))
}

// MaxSpendUSD $10, 8 workers, tasks of ~$3: all 8 used to claim before any
// finished and spend ~$24. In-flight tasks must not jointly overrun the cap.
func TestFleetSpendCapHoldsUnderConcurrency(t *testing.T) {
	r := costRunner(t)
	workers := make([]Worker, 8)
	for i := range workers {
		workers[i] = Worker{Name: fmt.Sprintf("w%d", i), Runner: r}
	}
	tasks := make([]Task, 16)
	for i := range tasks {
		tasks[i] = Task{ID: fmt.Sprintf("t%d", i), Input: runner.Input{Prompt: "x"}}
	}
	f, _ := New(Config{Workers: workers, MaxSpendUSD: 10})
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalCostUSD > 10+1e-9 {
		t.Errorf("spent $%.2f against a $10 cap", rep.TotalCostUSD)
	}
	if rep.TotalCostUSD < 10-1e-3 {
		t.Errorf("spent $%.2f, want the cap used up", rep.TotalCostUSD)
	}
	if ran := len(rep.Results) - rep.Skipped; ran != 4 || rep.Skipped != 12 {
		t.Errorf("ran %d, skipped %d; want 4 ($3+$3+$3+$1) and 12", ran, rep.Skipped)
	}
}

// A configured Retry keeps its own smaller cap.
func TestFleetGrantKeepsSmallerRetryCap(t *testing.T) {
	f, _ := New(Config{Workers: []Worker{{Name: "w", Runner: costRunner(t)}}, MaxSpendUSD: 10,
		Retry: &runner.RetryPolicy{MaxAttempts: 1, MaxSpendUSD: 0.5}})
	rep, err := f.Run(context.Background(), []Task{{ID: "a", Input: runner.Input{Prompt: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalCostUSD != 0.5 {
		t.Errorf("spent $%v, want the retry cap $0.50", rep.TotalCostUSD)
	}
}
