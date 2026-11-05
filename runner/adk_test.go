package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/budget"
	"github.com/tggo/claude-agent-go/claudecli"
)

// Regression tests for bugs Google ADK users reported, checked against this SDK.

func writeBin(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubborn emits two events then ignores SIGTERM and never exits.
const stubborn = `trap '' TERM
echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial answer"}]}}'
while :; do sleep 1; done
`

// ADK #2425: cancellation must stop a process that traps SIGTERM, return a
// typed error and keep the partial output.
func TestADK2425CancelKillsSigtermTrapperWithPartial(t *testing.T) {
	r := New(WithBinary(writeBin(t, stubborn)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	res, err := r.RunStream(ctx, Input{Prompt: "x"}, func(ev claudecli.StreamEvent, _ int) {
		if ev.IsAssistant() {
			cancel()
		}
	})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res == nil || res.SessionID != "sess-1" || res.ResultText != "partial answer" {
		t.Errorf("partial result = %+v", res)
	}
}

func TestADK2425TimeoutKillsSigtermTrapperWithPartial(t *testing.T) {
	r := New(WithBinary(writeBin(t, stubborn)), WithTimeout(500*time.Millisecond))
	res, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	if !IsTimeout(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want TimeoutError wrapping DeadlineExceeded", err)
	}
	if SessionIDOf(err) != "sess-1" || res == nil || res.ResultText != "partial answer" {
		t.Errorf("sid=%q res=%+v", SessionIDOf(err), res)
	}
}

func TestADK2425CancelJSONAndPlain(t *testing.T) {
	r := New(WithBinary(writeBin(t, "trap '' TERM\nwhile :; do sleep 1; done\n")))
	for name, run := range map[string]func(context.Context) error{
		"json":  func(c context.Context) error { _, e := r.RunJSON(c, Input{Prompt: "x"}); return e },
		"plain": func(c context.Context) error { _, e := r.Run(c, Input{Prompt: "x"}); return e },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			cctx, stop := context.WithCancel(ctx)
			go func() { time.Sleep(100 * time.Millisecond); stop() }()
			if err := run(cctx); !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// ADK #1214: rate limits are retryable.
func TestADK1214DefaultRetryableRateLimits(t *testing.T) {
	resErr := func(text, status string) *Result {
		m := &claudecli.ExecutionMetadata{IsError: true}
		if status != "" {
			m.APIErrorStatus = []byte(status)
		}
		return &Result{IsError: true, ResultText: text, Metadata: m}
	}
	stdoutRes := func(line string) error {
		return &ProcessError{ExitCode: 1, Stdout: "{\"type\":\"assistant\"}\n" + line}
	}
	cases := []struct {
		name string
		res  *Result
		err  error
		want bool
	}{
		{"429 status", resErr("API Error", "429"), nil, true},
		{"529 status", resErr("API Error", "529"), nil, true},
		{"rate_limit text", resErr("rate_limit_error", ""), nil, true},
		{"overloaded text", resErr("Overloaded", ""), nil, true},
		{"auth error", resErr("Invalid API key", "401"), nil, false},
		{"max turns", resErr("Reached max turns", ""), nil, false},
		{"stderr 429", nil, &ProcessError{ExitCode: 1, Stderr: "429 Too Many Requests"}, true},
		{"stdout result 429 by status", nil, stdoutRes(`{"type":"result","is_error":true,"api_error_status":429,"result":"API Error"}`), true},
		{"stdout result overloaded", nil, stdoutRes(`{"type":"result","is_error":true,"result":"Overloaded"}`), true},
		{"stdout truncated json 529", nil, stdoutRes(`{"type":"result","api_error_status":529,"resu`), true},
		{"stdout result 401", nil, stdoutRes(`{"type":"result","is_error":true,"api_error_status":401,"result":"bad key"}`), false},
		{"assistant text mentioning timeout", nil, &ProcessError{ExitCode: 1, Stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"set a timeout"}]}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultRetryable(tc.res, tc.err); got != tc.want {
				t.Errorf("DefaultRetryable = %v, want %v", got, tc.want)
			}
		})
	}
}

// failing N times with a 429 on stdout and exit 1, then succeeding.
func TestADK1214StreamRetryAfterExit1With429(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "n")
	bin := writeBin(t, fmt.Sprintf(`CF=%q
n=$(cat "$CF" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$CF"
if [ $n -le 2 ]; then
  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"API Error: 429 rate_limit_error"}'
  exit 1
fi
echo '{"type":"result","subtype":"success","session_id":"s","result":"OK","total_cost_usd":0.03}'
`, cf))
	var delays []time.Duration
	res, err := New(WithBinary(bin)).RunStreamWithRetry(context.Background(), Input{Prompt: "x"}, nil,
		fastPolicy(RetryPolicy{MaxAttempts: 5, OnRetry: func(i RetryInfo) { delays = append(delays, i.Delay) }}))
	if err != nil || res.ResultText != "OK" || res.Attempts != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(delays) != 2 || delays[0] <= 0 {
		t.Errorf("backoff delays = %v", delays)
	}
}

func TestBackoffGrowsWithJitter(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Minute}
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		d := p.backoff(3, nil, nil) // 400ms +-20%
		if d < 320*time.Millisecond || d > 480*time.Millisecond {
			t.Fatalf("delay %v outside 400ms +-20%%", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("no jitter")
	}
}

// Retries must not double-count cost: per-attempt RunRecords sum to the
// cumulative Result cost, and feeding the Result to a budget counts it once.
func TestADK1214RetryCostCountedOnce(t *testing.T) {
	bin, _ := fakeClaudeRetry(t, 2) // 0.02 + 0.02 + 0.03
	obs := &capture{}
	res, err := New(WithBinary(bin), WithObserver(obs)).RunJSONWithRetry(context.Background(), Input{Prompt: "x"},
		fastPolicy(RetryPolicy{MaxAttempts: 5}))
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, rec := range obs.recs {
		sum += rec.CostUSD
		if rec.Attempts != 1 {
			t.Errorf("per-attempt record Attempts = %d", rec.Attempts)
		}
	}
	if len(obs.recs) != 3 || diff(sum, 0.07) || diff(res.TotalCostUSD, 0.07) {
		t.Errorf("records=%d sum=%v result=%v", len(obs.recs), sum, res.TotalCostUSD)
	}
	b := budget.New(1)
	b.Add(res.SessionID, res.TotalCostUSD)
	if diff(b.Spent(), 0.07) {
		t.Errorf("budget spent = %v", b.Spent())
	}
}

func diff(a, b float64) bool { return a-b > 1e-9 || b-a > 1e-9 }

// ADK #5054: hitting --max-turns is a distinct, detectable outcome.
func TestADK5054MaxTurnsDetectable(t *testing.T) {
	line := func(sub string, isErr bool) string {
		return fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":%v,"session_id":"s","num_turns":3,"result":"partial","total_cost_usd":0.01}`, sub, isErr)
	}
	cases := []struct {
		name, sub string
		isErr     bool
		want      bool
	}{
		{"max turns, is_error true", "error_max_turns", true, true},
		{"max turns, is_error false", "error_max_turns", false, true},
		{"success", "success", false, false},
		{"other error", "error_during_execution", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeClaude(t, line(tc.sub, tc.isErr))
			r := New(WithBinary(bin))
			js, err := r.RunJSON(context.Background(), Input{Prompt: "x"})
			if err != nil {
				t.Fatal(err)
			}
			st, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for mode, res := range map[string]*Result{"json": js, "stream": st} {
				if IsMaxTurns(res) != tc.want || res.Subtype != tc.sub {
					t.Errorf("%s: IsMaxTurns=%v subtype=%q", mode, IsMaxTurns(res), res.Subtype)
				}
			}
		})
	}
	if IsMaxTurns(nil) {
		t.Error("nil result")
	}
}

func TestADK5054MaxTurnsNotRetried(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "n")
	bin := writeBin(t, fmt.Sprintf(`echo x >> %q
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s","result":"Reached max turns (3)","total_cost_usd":0.01}'
`, cf))
	res, err := New(WithBinary(bin)).RunStreamWithRetry(context.Background(), Input{Prompt: "x"}, nil,
		fastPolicy(RetryPolicy{MaxAttempts: 5}))
	if err == nil || !IsMaxTurns(res) {
		t.Fatalf("res=%+v err=%v, want error + IsMaxTurns", res, err)
	}
	b, _ := os.ReadFile(cf)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("ran %d times, want 1", n)
	}
}
