// Command basic demonstrates a one-shot JSON run with the SDK.
//
//	go run ./examples/basic -prompt "Summarize what this repo does"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/tggo/claude-agent-go/runner"
)

func main() {
	prompt := flag.String("prompt", "Say hello in one sentence.", "prompt to send")
	model := flag.String("model", "sonnet", "model alias or id")
	dir := flag.String("dir", ".", "working directory for the run")
	flag.Parse()

	r := runner.New(
		runner.WithModel(*model),
		runner.WithTimeout(5*time.Minute),
		runner.WithEntrypoint("claude-agent-go-example"),
	)

	res, err := r.RunJSON(context.Background(), runner.Input{
		Prompt:  *prompt,
		WorkDir: *dir,
	})
	if err != nil {
		log.Fatalf("run failed: %v", err)
	}

	fmt.Println(res.ResultText)
	fmt.Printf("\n--- session=%s cost=$%.4f turns=%d wall=%s\n",
		res.SessionID, res.TotalCostUSD, res.NumTurns, res.Duration)
}
