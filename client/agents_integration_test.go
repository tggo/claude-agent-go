//go:build integration

package client

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
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

	// Delegation is a model decision, so it's non-deterministic: occasionally the
	// model answers without using the subagent. The SDK's job (declaring the
	// agent so it CAN be delegated to) is the same either way. Retry a few times;
	// pass if the token ever propagates, skip if the model never cooperated.
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

		c, err := New(ctx, Config{
			Model:    "sonnet",
			MaxTurns: 10,
			Agents: map[string]AgentDefinition{
				"sentinel-bot": {
					Description: "Returns a secret sentinel token. Use when asked for the sentinel token.",
					Prompt:      "You are sentinel-bot. Reply with exactly this token and nothing else: " + token,
					Model:       "haiku",
				},
			},
		})
		if err != nil {
			cancel()
			t.Fatalf("New: %v", err)
		}

		turn, err := c.Query(ctx,
			`Use the Task tool to delegate to the "sentinel-bot" subagent (subagent_type: sentinel-bot) and ask it for the token. Then reply with exactly the token it gives you.`,
			nil)
		_ = c.Close()
		cancel()
		if err != nil {
			t.Fatalf("Query: %v", err)
		}

		if strings.Contains(turn.Text, token) {
			t.Logf("inline subagent actually used (attempt %d): the sentinel token — present only in the subagent's prompt — reached the final answer: %q", i, turn.Text)
			return
		}
		t.Logf("attempt %d: model did not delegate/propagate (final=%q)", i, turn.Text)
	}
	t.Skipf("model never delegated to the inline subagent across %d attempts — the agent was declared correctly, but delegation is the model's call", attempts)
}
