// Command permissions demonstrates a can_use_tool callback that decides, in Go,
// whether each tool call may proceed. Here every Write is denied.
//
//	go run ./examples/permissions
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
		CanUseTool: func(_ context.Context, tool string, input json.RawMessage, _ client.PermissionContext) (client.PermissionResult, error) {
			fmt.Printf("[permission] tool=%s input=%s\n", tool, string(input))
			if tool == "Write" {
				return client.Deny("writing is disabled in this demo"), nil
			}
			return client.Allow(), nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	turn, err := c.Query(ctx, "Create a file demo.txt containing 'hi' using the Write tool.", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nfinal: %s\n", turn.Text)
}
