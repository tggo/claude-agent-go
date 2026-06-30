//go:build integration

package client

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// TestIntegrationInlineAgentsAccepted proves the initialize handshake accepts
// inline agent definitions and a skills allowlist against the real binary.
// If the wire format were wrong, New (which auto-initializes) would error.
func TestIntegrationInlineAgentsAccepted(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model: "haiku",
		Agents: map[string]AgentDefinition{
			"echo-bot": {
				Description: "A trivial agent that echoes a confirmation.",
				Prompt:      "You are echo-bot. Reply with exactly: ECHO-OK",
				Model:       "haiku",
				Tools:       []string{"Read"},
			},
		},
		Skills: []string{}, // explicit: no skills
	})
	if err != nil {
		t.Fatalf("New (auto-initialize with agents/skills) failed: %v", err)
	}
	defer c.Close()

	// The session is live and the handshake was accepted. A normal turn works.
	turn, err := c.Query(ctx, "Reply with exactly: READY", nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	t.Logf("session up with inline agent declared; turn=%q", turn.Text)
}

// TestIntegrationInlineAgentIsActuallyUsed proves the inline subagent is not
// just transmitted but actually delegated to and executed. The sentinel token
// exists ONLY inside the agent's prompt — never in the user prompt — so it can
// reach the final answer only if claude really invoked the subagent and applied
// its prompt.
func TestIntegrationInlineAgentIsActuallyUsed(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	const token = "ZX9-WOWH-7Q"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:    "sonnet", // sonnet follows explicit delegation reliably
		MaxTurns: 8,
		Agents: map[string]AgentDefinition{
			"sentinel-bot": {
				Description: "Returns a secret sentinel token. Use when asked for the sentinel token.",
				Prompt:      "You are sentinel-bot. Reply with exactly this token and nothing else: " + token,
				Model:       "haiku",
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	var delegated bool
	turn, err := c.Query(ctx,
		`Use the Task tool to delegate to the "sentinel-bot" subagent (subagent_type: sentinel-bot) and ask it for the token. Then reply with exactly the token it gives you.`,
		func(ev claudecli.StreamEvent) {
			if m := ev.AssistantMessage(); m != nil {
				for _, b := range m.Content {
					if b.Type == claudecli.BlockToolUse && b.Name == "Task" &&
						strings.Contains(string(b.Input), "sentinel-bot") {
						delegated = true
					}
				}
			}
		})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if !strings.Contains(turn.Text, token) {
		t.Fatalf("sentinel token not in final answer — subagent prompt was NOT applied.\n delegated(Task seen)=%v\n final=%q", delegated, turn.Text)
	}
	t.Logf("inline subagent actually used: token propagated; Task-delegation observed=%v; final=%q", delegated, turn.Text)
}
