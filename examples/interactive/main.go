// Command interactive demonstrates a multi-turn session that keeps context
// across turns in a single long-running claude process.
//
//	go run ./examples/interactive
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

	c, err := client.New(ctx, client.Config{Model: "haiku"})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	t1, err := c.Query(ctx, "Remember the number 7. Reply with exactly: OK", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("turn 1: %s\n", t1.Text)

	t2, err := c.Query(ctx, "What number did I ask you to remember?", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("turn 2: %s  (same session: %v)\n", t2.Text, t2.SessionID == t1.SessionID)
}
