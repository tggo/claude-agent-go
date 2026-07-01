package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeClaudeRetry emits an error-result (is_error, "Overloaded", cost 0.02) for
// the first failFirst invocations, then a success (cost 0.03). It counts calls
// via a file so state persists across the separate processes RunJSON spawns.
func fakeClaudeRetry(t *testing.T, failFirst int) (bin string, counter string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	counter = filepath.Join(dir, "count")
	script := fmt.Sprintf(`#!/bin/sh
CF=%q
n=$(cat "$CF" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$CF"
if [ "$n" -le %d ]; then
  printf '%%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s","result":"Overloaded, please retry","total_cost_usd":0.02}'
else
  printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"OK","total_cost_usd":0.03}'
fi
`, counter, failFirst)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return bin, counter
}

func fastPolicy(p RetryPolicy) RetryPolicy {
	p.BaseDelay = time.Millisecond
	p.MaxDelay = 3 * time.Millisecond
	return p
}

func TestRetrySucceedsAfterTransient(t *testing.T) {
	bin, _ := fakeClaudeRetry(t, 2) // fail twice, then succeed
	r := New(WithBinary(bin))
	res, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 5}))
	if err != nil {
		t.Fatalf("RunJSONWithRetry: %v", err)
	}
	if res.ResultText != "OK" {
		t.Errorf("ResultText = %q", res.ResultText)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	// cumulative spend: 0.02 + 0.02 + 0.03
	if diff := res.TotalCostUSD - 0.07; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("TotalCostUSD = %v, want cumulative 0.07", res.TotalCostUSD)
	}
}

func TestRetrySpendCapStops(t *testing.T) {
	bin, _ := fakeClaudeRetry(t, 100) // always fails
	r := New(WithBinary(bin))
	res, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 10, MaxSpendUSD: 0.05}))
	if err == nil {
		t.Fatal("expected error after exhausting/capping")
	}
	// attempts: 0.02,0.04,0.06 — after attempt 3 spent(0.06) >= cap(0.05) -> stop.
	if res == nil {
		t.Fatal("expected last result to be returned")
	}
	if diff := res.TotalCostUSD - 0.06; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("TotalCostUSD = %v, want 0.06 (capped)", res.TotalCostUSD)
	}
}

func TestRetryNonRetryable(t *testing.T) {
	// Missing binary -> CLINotFound -> not retried.
	var retries int
	r := New(WithBinary("/nonexistent/claude"))
	_, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 5, OnRetry: func(RetryInfo) { retries++ }}))
	if err == nil || !errors.Is(err, err) {
		t.Fatal("expected error")
	}
	if !IsCLINotFound(err) {
		t.Errorf("want CLINotFound, got %v", err)
	}
	if retries != 0 {
		t.Errorf("should not have retried a missing binary; retries=%d", retries)
	}
}

func TestRetryOnRetryCallback(t *testing.T) {
	bin, _ := fakeClaudeRetry(t, 1) // one failure then success
	r := New(WithBinary(bin))
	var seen []RetryInfo
	_, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 3, OnRetry: func(ri RetryInfo) { seen = append(seen, ri) }}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("OnRetry calls = %d, want 1", len(seen))
	}
	if seen[0].Attempt != 1 || seen[0].SpentUSD <= 0 {
		t.Errorf("retry info = %+v", seen[0])
	}
}

func TestDefaultRetryable(t *testing.T) {
	// timeouts retry; missing binary doesn't
	if !DefaultRetryable(nil, &TimeoutError{Timeout: "1s"}) {
		t.Error("timeout should be retryable")
	}
	if DefaultRetryable(nil, &CLINotFoundError{Binary: "x"}) {
		t.Error("CLI-not-found should not be retryable")
	}
	// transient process error
	if !DefaultRetryable(nil, &ProcessError{ExitCode: 1, Stderr: "API error: overloaded_error"}) {
		t.Error("overloaded process error should retry")
	}
	if DefaultRetryable(nil, &ProcessError{ExitCode: 1, Stderr: "invalid api key"}) {
		t.Error("auth error should not retry")
	}
	// transient error-result (no Go error)
	if !DefaultRetryable(&Result{IsError: true, ResultText: "Rate limit exceeded"}, nil) {
		t.Error("rate-limit result should retry")
	}
	// clean result never retries
	if DefaultRetryable(&Result{IsError: false, ResultText: "OK"}, nil) {
		t.Error("clean result should not retry")
	}
}

func TestRetryAfterExtraction(t *testing.T) {
	pe := &ProcessError{Stderr: "429 Too Many Requests. Retry-After: 7"}
	if d := retryAfter(nil, pe); d != 7*time.Second {
		t.Errorf("retryAfter = %v, want 7s", d)
	}
	if d := retryAfter(&Result{ResultText: "nothing here"}, nil); d != 0 {
		t.Errorf("retryAfter = %v, want 0", d)
	}
}

func TestBackoffHonorsRetryAfter(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	applyRetryDefaults(&p)
	// server says 3s -> backoff must be at least 3s despite tiny base/max.
	d := p.backoff(1, nil, &ProcessError{Stderr: "retry after: 3"})
	if d < 3*time.Second {
		t.Errorf("backoff = %v, want >= 3s (retry-after)", d)
	}
}
