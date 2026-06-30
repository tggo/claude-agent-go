// Command hooks demonstrates a PreToolUse hook callback that blocks a tool
// before it runs.
//
//	go run ./examples/hooks
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/tggo/claude-agent-go/client"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := client.New(ctx, client.Config{
		Model: "haiku",
		Hooks: map[string][]client.HookMatcher{
			"PreToolUse": {{
				Matcher: "Bash",
				Callbacks: []client.HookCallback{
					func(_ context.Context, input json.RawMessage, _ string) (json.RawMessage, error) {
						fmt.Printf("[hook] PreToolUse Bash: %s\n", string(input))
						return json.RawMessage(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked by demo hook"}}`), nil
					},
				},
			}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	turn, err := c.Query(ctx, "Run the bash command: echo hello", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nfinal: %s\n", turn.Text)
}
