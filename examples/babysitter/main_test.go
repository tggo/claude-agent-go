package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

func kinds(h *harness) []string {
	var k []string
	for _, e := range h.srv.Events() {
		if e.Type == graphweb.TypeBabysitter {
			k = append(k, e.Babysitter.Kind)
		}
	}
	return k
}

func status(t *testing.T, h *harness) string {
	t.Helper()
	l, err := h.store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, l, 1)
	return l[0].Status
}

// Yes: the babysitter patches and recovers the same run to done.
func TestDemoFixesAndRecovers(t *testing.T) {
	h, err := newHarness(t.TempDir(), time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, h.demo(context.Background(), true))
	assert.Equal(t, graph.StatusDone, status(t, h))
	assert.Equal(t, []string{"woke", "doing", "said", "asked", "doing", "doing", "idle"}, kinds(h))
}

// No, given through the panel's Answer: it hands the failed run over; the
// person's Continue recovers it once the code is fixed.
func TestDemoHandsOverThenPersonContinues(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(t.TempDir(), time.Millisecond)
	require.NoError(t, err)
	go func() {
		require.Eventually(t, func() bool { return h.srv.BabysitterAct(ctx, h.runIDSafe(), "answer", "q1", "no") == nil },
			10*time.Second, 5*time.Millisecond)
	}()
	require.NoError(t, h.demo(ctx, false))
	k := kinds(h)
	assert.Equal(t, []string{"woke", "doing", "said", "asked", "answered", "handover"}, k)
	assert.Equal(t, graph.StatusFailed, status(t, h))

	assert.Error(t, h.Answer(ctx, h.runID, "q9", "x"), "unknown question")
	for _, act := range []func() error{
		func() error { return h.Pause(ctx, "other") }, func() error { return h.Continue(ctx, "other") },
		func() error { return h.TakeOver(ctx, "other") }, func() error { return h.Answer(ctx, "other", "q1", "x") },
	} {
		assert.ErrorContains(t, act(), "not this harness's run")
	}
	h.fixed.Store(true)
	require.True(t, h.startRecover(), "simulate a Recover in flight")
	assert.ErrorContains(t, h.Continue(ctx, h.runID), "already being continued")
	h.endRecover()
	require.NoError(t, h.srv.BabysitterAct(ctx, h.runID, "continue", "", ""))
	require.NoError(t, <-h.done)
	assert.Equal(t, graph.StatusDone, status(t, h))
}

// Take over while it waits for an answer: it stops and leaves the run alone.
func TestDemoTakeOver(t *testing.T) {
	ctx := context.Background()
	h, err := newHarness(t.TempDir(), time.Millisecond)
	require.NoError(t, err)
	go func() {
		require.Eventually(t, func() bool {
			k := kinds(h)
			return len(k) > 0 && k[len(k)-1] == graphweb.BabysitterAsked
		}, 10*time.Second, 5*time.Millisecond)
		require.NoError(t, h.srv.BabysitterAct(ctx, h.runID, "takeover", "", ""))
	}()
	require.NoError(t, h.demo(ctx, false))
	assert.Equal(t, graph.StatusFailed, status(t, h))
	assert.Equal(t, graphweb.BabysitterTakeover, kinds(h)[len(kinds(h))-1])
	assert.Error(t, h.Answer(ctx, h.runID, "q1", "late"), "nobody waits for an answer")
	require.NoError(t, h.Pause(ctx, h.runID))
	ok, _ := h.sig.PauseRequested(ctx, h.runID)
	assert.True(t, ok)
}
