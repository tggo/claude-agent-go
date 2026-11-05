package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
)

func shFake(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return bin
}

const initLine = `echo '{"type":"system","subtype":"init","session_id":"sess-x"}'`
const asstLine = `echo '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial"}]}}'`

func TestLineTooLongKillsRunAndReturnsPartial(t *testing.T) {
	// After the oversized line the agent would keep working for 20s; the run
	// must be killed immediately, not drained.
	bin := shFake(t, initLine+"\n"+asstLine+"\nhead -c 300000 /dev/zero | tr '\\0' 'x'; echo\nsleep 20\n"+
		`echo '{"type":"result","result":"late"}'`)
	r := New(WithBinary(bin), WithMaxBufferSize(1000), WithTimeout(30*time.Second))
	start := time.Now()
	res, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	assert.Less(t, time.Since(start), 10*time.Second, "run was drained instead of killed")

	var lt *LineTooLongError
	require.ErrorAs(t, err, &lt)
	assert.Equal(t, 1000, lt.Limit)
	assert.Contains(t, err.Error(), "WithMaxBufferSize")
	require.NotNil(t, res)
	assert.Equal(t, "sess-x", res.SessionID)
	assert.Equal(t, "partial", res.ResultText)
	assert.Equal(t, "partial", lt.PartialText)
	assert.Equal(t, "sess-x", SessionIDOf(err))
	assert.False(t, DefaultRetryable(res, err))
}

// truncFake emits a truncated stream (no result event) and logs each call.
func truncFake(t *testing.T) (bin, log string) {
	log = filepath.Join(t.TempDir(), "calls")
	bin = shFake(t, "cat >/dev/null\necho x >> "+log+"\n"+initLine+"\n"+asstLine)
	return bin, log
}

func callCount(t *testing.T, log string) int {
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	return strings.Count(string(b), "\n")
}

func TestResumeSpendCapCountsTruncatedAttempts(t *testing.T) {
	bin, log := truncFake(t)
	r := New(WithBinary(bin), WithLogger(quietLogger()))
	res, err := r.RunStreamWithRetry(context.Background(), Input{Prompt: "x"}, nil,
		RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, Resume: true, MaxSpendUSD: 1})
	require.Error(t, err)
	// Default estimate is the per-run budget ($5) > cap: stop after the first.
	assert.Equal(t, 1, callCount(t, log))
	require.NotNil(t, res)
	assert.Equal(t, 1, res.Attempts, "Attempts must be the real count, not MaxAttempts")
	assert.Zero(t, res.TotalCostUSD, "estimates must not leak into known spend")
}

func TestResumeSpendCapExplicitEstimate(t *testing.T) {
	bin, log := truncFake(t)
	r := New(WithBinary(bin), WithLogger(quietLogger()))
	res, err := r.RunStreamWithRetry(context.Background(), Input{Prompt: "x"}, nil,
		RetryPolicy{MaxAttempts: 9, BaseDelay: time.Millisecond, Resume: true, MaxSpendUSD: 1, UnknownAttemptCostUSD: 0.4})
	require.Error(t, err)
	assert.Equal(t, 3, callCount(t, log)) // 0.4, 0.8, 1.2 >= 1
	assert.Equal(t, 3, res.Attempts)
}

func TestRetryAttemptsOnNonRetryableStop(t *testing.T) {
	bin := shFake(t, `cat >/dev/null; echo boom >&2; exit 2`)
	r := New(WithBinary(bin), WithLogger(quietLogger()))
	_, err := r.RunJSONWithRetry(context.Background(), Input{Prompt: "x"}, RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond})
	require.Error(t, err)
	// error-result variant exposes Attempts via a result:
	bin2 := shFake(t, `cat >/dev/null; echo '{"type":"result","is_error":true,"result":"bad request","total_cost_usd":0.01}'`)
	res, err := New(WithBinary(bin2), WithLogger(quietLogger())).RunStreamWithRetry(context.Background(), Input{Prompt: "x"}, nil,
		RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond})
	require.Error(t, err)
	assert.Equal(t, 1, res.Attempts)
}

type recPlugin struct {
	mu     sync.Mutex
	events int
	ends   []RunRecord
	prompt string
	failAt string // event type to abort on
	startE error
}

func (p *recPlugin) Name() string { return "rec" }
func (p *recPlugin) OnRunStart(_ context.Context, in Input) (Input, error) {
	if p.startE != nil {
		return in, p.startE
	}
	if p.prompt != "" {
		in.Prompt = p.prompt
	}
	return in, nil
}
func (p *recPlugin) OnEvent(_ context.Context, ev claudecli.StreamEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events++
	if p.failAt != "" && ev.Type == p.failAt {
		return errors.New("nope")
	}
	return nil
}
func (p *recPlugin) OnRunEnd(_ context.Context, rec *RunRecord) {
	p.mu.Lock()
	p.ends = append(p.ends, *rec)
	p.mu.Unlock()
}

type namedOnly struct{}

func (namedOnly) Name() string { return "named-only" }

func TestPluginRewritesPromptAndSeesEvents(t *testing.T) {
	log := filepath.Join(t.TempDir(), "p")
	bin := shFake(t, "cat > "+log+"\n"+initLine+"\n"+asstLine+"\n"+
		`echo '{"type":"result","session_id":"sess-x","result":"ok","total_cost_usd":0.02}'`)
	p := &recPlugin{prompt: "rewritten"}
	obs := &labels{}
	var progress int
	r := New(WithBinary(bin), WithLogger(quietLogger()), WithObserver(obs), WithPlugins(namedOnly{}, p))
	res, err := r.RunStream(context.Background(), Input{Prompt: "orig", Label: "L"}, func(claudecli.StreamEvent, int) { progress++ })
	require.NoError(t, err)
	got, _ := os.ReadFile(log)
	assert.Equal(t, "rewritten", string(got))
	assert.Equal(t, 3, p.events)
	assert.Equal(t, 3, progress, "Progress still works alongside plugins")
	assert.Equal(t, []string{"L"}, obs.ls, "Observer still works alongside plugins")
	require.Len(t, p.ends, 1)
	assert.Equal(t, "stream", p.ends[0].Mode)
	assert.InDelta(t, 0.02, p.ends[0].CostUSD, 1e-9)
	assert.Equal(t, "ok", res.ResultText)
}

func TestPluginEventAbortKillsRun(t *testing.T) {
	bin := shFake(t, "cat >/dev/null\n"+initLine+"\n"+asstLine+"\nsleep 20\n"+`echo '{"type":"result","result":"late"}'`)
	p := &recPlugin{failAt: "assistant"}
	r := New(WithBinary(bin), WithLogger(quietLogger()), WithPlugins(p), WithTimeout(30*time.Second))
	start := time.Now()
	res, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	assert.Less(t, time.Since(start), 10*time.Second)
	var pa *PluginAbortError
	require.ErrorAs(t, err, &pa)
	assert.Equal(t, "rec", pa.Plugin)
	assert.Contains(t, err.Error(), "nope")
	assert.Equal(t, "sess-x", SessionIDOf(err))
	require.NotNil(t, res)
	assert.Equal(t, "sess-x", res.SessionID)
	assert.False(t, DefaultRetryable(res, err))
	require.Len(t, p.ends, 1)
	assert.Error(t, p.ends[0].Err)
}

func TestPluginStartVetoAllModes(t *testing.T) {
	boom := errors.New("veto")
	p := &recPlugin{startE: boom}
	// The binary does not exist: a veto must happen before the CLI is started.
	r := New(WithBinary("/nonexistent/claude"), WithLogger(quietLogger()), WithPlugins(p))
	ctx := context.Background()
	_, e1 := r.Run(ctx, Input{Prompt: "x"})
	_, e2 := r.RunJSON(ctx, Input{Prompt: "x"})
	_, e3 := r.RunStream(ctx, Input{Prompt: "x"}, nil)
	for _, e := range []error{e1, e2, e3} {
		assert.ErrorIs(t, e, boom)
		assert.False(t, IsCLINotFound(e))
	}
	assert.Len(t, p.ends, 3)
}

func TestSpendCapPlugin(t *testing.T) {
	bin := shFake(t, "cat >/dev/null\n"+initLine+"\n"+`echo '{"type":"result","session_id":"sess-x","result":"ok","total_cost_usd":0.6}'`)
	c := NewSpendCap(1)
	r := New(WithBinary(bin), WithLogger(quietLogger()), WithPlugins(c))
	ctx := context.Background()
	for i := 0; i < 2; i++ { // 0.6, then 1.2: the second completes, result is kept
		res, err := r.RunStream(ctx, Input{Prompt: "x"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", res.ResultText)
	}
	assert.InDelta(t, 1.2, c.SpentUSD(), 1e-9)
	_, err := r.RunStream(ctx, Input{Prompt: "x"}, nil)
	assert.ErrorIs(t, err, ErrSpendCap)
	assert.Equal(t, "spend-cap", c.Name())
}

func TestSpendCapAbortsOnCostBearingMidStreamEvent(t *testing.T) {
	bin := shFake(t, "cat >/dev/null\n"+initLine+"\n"+
		`echo '{"type":"assistant","total_cost_usd":2,"message":{"id":"m","role":"assistant","content":[{"type":"text","text":"hi"}]}}'`+"\nsleep 20")
	c := NewSpendCap(1)
	r := New(WithBinary(bin), WithLogger(quietLogger()), WithPlugins(c), WithTimeout(30*time.Second))
	start := time.Now()
	_, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.ErrorIs(t, err, ErrSpendCap)
}

func TestSpendCapCountsJSONRuns(t *testing.T) {
	bin := shFake(t, `cat >/dev/null; echo '[{"type":"result","session_id":"s","result":"ok","total_cost_usd":0.7}]'`)
	c := NewSpendCap(0.5)
	r := New(WithBinary(bin), WithLogger(quietLogger()), WithPlugins(c))
	_, err := r.RunJSON(context.Background(), Input{Prompt: "x"})
	require.NoError(t, err)
	assert.InDelta(t, 0.7, c.SpentUSD(), 1e-9)
	_, err = r.RunJSON(context.Background(), Input{Prompt: "x"})
	assert.ErrorIs(t, err, ErrSpendCap)
	// stream-mode records are not double counted through OnRunEnd
	c.OnRunEnd(context.Background(), &RunRecord{Mode: "stream", CostUSD: 99})
	assert.InDelta(t, 0.7, c.SpentUSD(), 1e-9)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type labels struct {
	mu sync.Mutex
	ls []string
}

func (l *labels) ObserveRun(r RunRecord) {
	l.mu.Lock()
	l.ls = append(l.ls, r.Label)
	l.mu.Unlock()
}
