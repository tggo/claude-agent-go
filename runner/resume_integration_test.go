//go:build integration

package runner

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/transport"
)

// truncateFirst cuts the FIRST invocation's stdout after one line (the init
// event) — `claude | head -n 1`: the CLI gets SIGPIPE, the shell exits 0, and
// the runner sees a clean exit without a result event, i.e. a real mid-stream
// truncation. Later invocations run claude normally.
type truncateFirst struct{ calls atomic.Int32 }

func (t *truncateFirst) Command(ctx context.Context, args []string, opt transport.CommandOpts) *exec.Cmd {
	if t.calls.Add(1) == 1 {
		cmd := exec.Command("/bin/sh", append([]string{"-c", `claude "$@" | head -n 1`, "sh"}, args...)...)
		cmd.Dir = opt.WorkDir
		cmd.Env = append(cmd.Environ(), opt.Env...)
		return cmd
	}
	return transport.Local{Binary: "claude"}.Command(ctx, args, opt)
}

// TestIntegrationResumeAfterTruncation asserts the effect of RetryPolicy.Resume:
// the retry continues the SAME conversation (it knows a secret that only the
// truncated first attempt's prompt contained), rather than starting fresh.
func TestIntegrationResumeAfterTruncation(t *testing.T) {
	requireClaude(t)
	tr := &truncateFirst{}
	r := New(WithModel("haiku"), WithMaxTurns(3), WithTimeout(2*time.Minute),
		WithEntrypoint("claude-agent-go-itest"), WithTransport(tr))

	var sawTruncation bool
	res, err := r.RunStreamWithRetry(context.Background(), Input{
		Prompt: "Remember this secret word: ZEBRA-4217. Now write two sentences about zebras.",
	}, nil, RetryPolicy{
		MaxAttempts:  2,
		BaseDelay:    time.Second,
		Resume:       true,
		ResumePrompt: "What was the secret word I gave you? Reply with only the word.",
		OnRetry: func(ri RetryInfo) {
			_, sawTruncation = IsTruncatedStream(ri.Err)
		},
	})
	if err != nil {
		t.Fatalf("RunStreamWithRetry: %v", err)
	}
	if !sawTruncation {
		t.Fatal("first attempt was not reported as a truncated stream")
	}
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", res.Attempts)
	}
	if !strings.Contains(res.ResultText, "ZEBRA-4217") {
		t.Errorf("resumed answer = %q; the retry did not continue the session", res.ResultText)
	}
}
