// Command agents demonstrates declaring a subagent inline from Go. The main
// agent delegates to it via the Task tool.
//
//	go run ./examples/agents
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/tggo/claude-agent-go/client"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := client.New(ctx, client.Config{
		Model: "sonnet",
		Agents: map[string]client.AgentDefinition{
			"haiku-poet": {
				Description: "Writes a single haiku on request. Use when the user wants a haiku.",
				Prompt:      "You are haiku-poet. Reply with exactly one haiku (3 lines, 5-7-5) and nothing else.",
				Model:       "haiku",
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	turn, err := c.Query(ctx,
		`Use the Task tool to delegate to the "haiku-poet" subagent and ask it for a haiku about Go. Reply with exactly what it returns.`, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(turn.Text)
}
