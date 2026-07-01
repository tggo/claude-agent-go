// Command retry demonstrates RunJSONWithRetry: transient failures (rate limits,
// overloaded, 5xx) are retried with exponential backoff, while the cost of every
// attempt is accumulated and a spend cap stops retries from multiplying spend.
//
//	go run ./examples/retry -prompt "Say hi" -max-spend 0.50
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
	prompt := flag.String("prompt", "Reply with exactly: PONG", "prompt")
	attempts := flag.Int("attempts", 4, "max attempts")
	maxSpend := flag.Float64("max-spend", 0.50, "stop retrying once cumulative $ spend reaches this (0 = no cap)")
	flag.Parse()

	r := runner.New(runner.WithModel("haiku"), runner.WithTimeout(2*time.Minute))

	res, err := r.RunJSONWithRetry(context.Background(), runner.Input{Prompt: *prompt},
		runner.RetryPolicy{
			MaxAttempts: *attempts,
			MaxSpendUSD: *maxSpend,
			OnRetry: func(ri runner.RetryInfo) {
				log.Printf("attempt %d failed (%v) — retrying in %s; spent so far $%.4f",
					ri.Attempt, ri.Err, ri.Delay.Round(time.Millisecond), ri.SpentUSD)
			},
		})
	if err != nil {
		log.Fatalf("gave up: %v", err)
	}

	fmt.Println(res.ResultText)
	fmt.Printf("\n--- attempts=%d total spend across attempts=$%.5f\n", res.Attempts, res.TotalCostUSD)
}
