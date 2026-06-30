//go:build integration

// Integration test for the interactive client against the real `claude` binary.
// Run with:
//
//	go test -tags integration ./client/...
//
// Requires `claude` on PATH and valid credentials.
package client

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

func TestIntegrationInteractiveSession(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:      "haiku",
		MaxTurns:   3,
		Entrypoint: "claude-agent-go-client-itest",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Turn 1: establish a fact.
	t1, err := c.Query(ctx, "Remember the number 7. Reply with exactly: OK", nil)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if t1.SessionID == "" {
		t.Error("turn 1 missing session id")
	}
	t.Logf("turn1=%q cost=$%.5f", t1.Text, t1.TotalCostUSD)

	// Turn 2: rely on conversational memory from turn 1.
	t2, err := c.Query(ctx, "What number did I ask you to remember? Reply with just the digit.", nil)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(t2.Text, "7") {
		t.Errorf("turn 2 did not recall the number: %q", t2.Text)
	}
	if t2.SessionID != t1.SessionID {
		t.Errorf("session changed between turns: %q -> %q", t1.SessionID, t2.SessionID)
	}
	t.Logf("turn2=%q cost=$%.5f", t2.Text, t2.TotalCostUSD)
}

func TestIntegrationInitialize(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{Model: "haiku"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	raw, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(raw) == 0 {
		t.Error("expected a non-empty capability response")
	}
	t.Logf("init capabilities: %d bytes", len(raw))
}

func TestIntegrationPartialMessages(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{Model: "haiku", IncludePartialMessages: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	var deltas []string
	turn, err := c.Query(ctx, "Reply with exactly: alpha beta gamma delta",
		func(ev claudecli.StreamEvent) {
			if d := ev.TextDelta(); d != "" {
				deltas = append(deltas, d)
			}
		})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(deltas) == 0 {
		t.Error("expected token-level text deltas, got none")
	}
	joined := strings.Join(deltas, "")
	t.Logf("collected %d deltas, joined=%q final=%q", len(deltas), joined, turn.Text)
}

func TestIntegrationInterrupt(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{Model: "haiku", MaxTurns: 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Fire the interrupt shortly after the turn starts.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Second)
		if err := c.Interrupt(ctx); err != nil {
			t.Logf("interrupt error: %v", err)
		}
	}()

	turn, err := c.Query(ctx, "Count slowly from 1 to 100, one number per line, reasoning about each.", nil)
	wg.Wait()
	if err != nil {
		// An interrupted turn may surface as an error — acceptable.
		t.Logf("query returned error after interrupt: %v", err)
		return
	}
	t.Logf("interrupted turn: isError=%v text-len=%d", turn.IsError, len(turn.Text))
}
