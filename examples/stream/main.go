// Command stream demonstrates a long-running streaming run with a progress
// callback — the seam for live progress or heartbeats.
//
//	go run ./examples/stream -prompt "Investigate the codebase and report"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

func main() {
	prompt := flag.String("prompt", "List the files here and describe them.", "prompt to send")
	dir := flag.String("dir", ".", "working directory for the run")
	flag.Parse()

	r := runner.New(runner.WithEntrypoint("claude-agent-go-stream-example"))

	// progress fires per stream event — wire it to a heartbeat or live UI.
	progress := func(ev claudecli.StreamEvent, n int) {
		if n%5 == 0 {
			fmt.Printf("[progress] %d events, last type=%s\n", n, ev.Type)
		}
	}

	res, err := r.RunStream(context.Background(), runner.Input{
		Prompt:  *prompt,
		WorkDir: *dir,
	}, progress)
	if err != nil {
		log.Fatalf("run failed: %v", err)
	}

	fmt.Println("\n=== result ===")
	fmt.Println(res.ResultText)
	fmt.Printf("\n--- session=%s cost=$%.4f turns=%d wall=%s\n",
		res.SessionID, res.TotalCostUSD, res.NumTurns, res.Duration)
}
