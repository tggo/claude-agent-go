package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBudgetCLI records each attempt's argv and, like the CLI's own budget
// check, spends what --max-budget-usd allows (default 7.00) and then fails
// with a retryable error result.
func fakeBudgetCLI(t *testing.T) (bin, argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	bin, argvLog = filepath.Join(dir, "claude"), filepath.Join(dir, "argv")
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
cost=7.00
while [ $# -gt 0 ]; do [ "$1" = "--max-budget-usd" ] && cost=$2; shift; done
printf '%%s\n' "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"session_id\":\"s\",\"result\":\"Overloaded, please retry\",\"total_cost_usd\":$cost}"
`, argvLog)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

// MaxSpendUSD $1 with a Runner budget of $7: the single attempt must run
// with --max-budget-usd 1, not 7, so it cannot be billed $7 against a $1 cap.
func TestRetrySpendCapBoundsFirstAttempt(t *testing.T) {
	bin, argvLog := fakeBudgetCLI(t)
	r := New(WithBinary(bin), WithMaxBudgetUSD("7.00"))
	res, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 3, MaxSpendUSD: 1}))
	if err == nil {
		t.Fatal("want error result")
	}
	if res.TotalCostUSD > 1 {
		t.Errorf("spent $%v against a $1 cap", res.TotalCostUSD)
	}
	b, _ := os.ReadFile(argvLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || budgetArg(lines[0]) != "1" {
		t.Errorf("attempt argv = %q, want one attempt with --max-budget-usd 1", lines)
	}
}

// Later attempts get only what is left of the cap; a Runner budget below the
// remainder is kept.
func TestRetrySpendCapLowersLaterAttempts(t *testing.T) {
	bin, argvLog := fakeBudgetCLI(t)
	r := New(WithBinary(bin), WithMaxBudgetUSD("0.25"))
	res, _ := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 9, MaxSpendUSD: 0.6}))
	b, _ := os.ReadFile(argvLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"0.25", "0.25", "0.0999"}
	if len(lines) != len(want) {
		t.Fatalf("attempts = %d (%q), want %d", len(lines), lines, len(want))
	}
	for i, w := range want {
		if budgetArg(lines[i]) != w {
			t.Errorf("attempt %d argv = %q, want budget %s", i+1, lines[i], w)
		}
	}
	if res.TotalCostUSD > 0.6+1e-9 {
		t.Errorf("spent $%v against a $0.60 cap", res.TotalCostUSD)
	}
}

func budgetArg(argv string) string {
	f := strings.Fields(argv)
	for i := range f[:len(f)-1] {
		if f[i] == "--max-budget-usd" {
			return f[i+1]
		}
	}
	return ""
}

func TestRetrySpendCapTooSmallForAnAttempt(t *testing.T) {
	bin, argvLog := fakeBudgetCLI(t)
	res, err := New(WithBinary(bin)).RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxSpendUSD: 0.00001}))
	if err == nil || res != nil {
		t.Fatalf("res=%v err=%v, want an error and no run", res, err)
	}
	if _, statErr := os.Stat(argvLog); statErr == nil {
		t.Error("an attempt ran")
	}
}
