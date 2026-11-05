// Command pizza is a pizza-ordering agent on top of Claude Code. The outside
// world (menu, delivery, payment) is simulated by in-memory mocks exposed to
// the model as in-process Go tools:
//
//	order ──▶ confirm ──yes──▶ place ──▶ end
//	  ▲          └──change──┘
//
// The model can read the menu, quote delivery and propose a priced draft; it
// cannot place or pay. Placing is a plain Go step that runs only after a human
// confirmed the exact draft the mock stored.
//
// The model is reached through whatever ANTHROPIC_* variables are set; the
// .env in the working directory (else the repo root's; override with -env) is
// loaded, allowlisted keys only (it is gitignored):
//
//	ANTHROPIC_BASE_URL=https://cerber.ihatebot.com
//	ANTHROPIC_API_KEY=...
//	PIZZA_MODEL=claude-haiku-5-5
//
// Every agent run is shown live and recorded; play a run back with
//
//	go run ./examples/pizza -replay <runs dir> [-speed 4]
//
// Usage:
//
//	go run ./examples/pizza -order "two large pepperoni to Khreshchatyk 22, Kyiv"
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/tggo/claude-agent-go/graph"
)

func main() {
	order := flag.String("order", "", "what you want (asked interactively if empty)")
	out := flag.String("out", "", "runs dir (default: a new temp dir)")
	replay := flag.String("replay", "", "play back a runs dir instead of ordering")
	speed := flag.Float64("speed", 1, "replay speed multiplier (0 = no pauses)")
	outOfStock := flag.String("out-of-stock", "", "comma-separated menu ids to mark unavailable")
	envFile := flag.String("env", "", "env file to load (default: .env in cwd, else the repo root)")
	flag.Parse()

	if *replay != "" {
		if err := Replay(os.Stdout, *replay, *speed); err != nil {
			log.Fatal(err)
		}
		return
	}

	if path, err := LoadDotEnv(*envFile); err != nil {
		log.Fatal(err)
	} else if path != "" {
		fmt.Printf("env: %s\n", path)
	}
	model := os.Getenv("PIZZA_MODEL")
	if model == "" {
		model = "haiku"
	}

	in := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		s, _ := in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	if *order == "" {
		*order = ask("What would you like? > ")
	}

	dir := *out
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "pizza-"); err != nil {
			log.Fatal(err)
		}
	}
	runs := filepath.Join(dir, "runs")

	pr := NewPrinter(os.Stdout)
	a, err := Build(Options{Model: model, RunsDir: runs, Progress: pr.Progress()})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	for _, id := range strings.Split(*outOfStock, ",") {
		if id = strings.TrimSpace(id); id != "" {
			a.Shop.SetOutOfStock(id)
		}
	}
	j, err := OpenJournal(runs)
	if err != nil {
		log.Fatal(err)
	}
	defer j.Close()

	ctx := context.Background()
	j.Note(pr, "customer: %s  (model %s)", *order, model)
	st, rep, err := a.Graph.Run(ctx, State{Request: *order, CartID: newCartID()})
	cost, path := 0.0, []string{}
	tally := func() {
		if rep != nil {
			cost += rep.TotalCostUSD
			path = append(path, rep.Path...)
		}
	}
	tally()
	for {
		var intr *graph.Interrupted[State]
		if !errors.As(err, &intr) {
			break
		}
		j.Note(pr, "⏸ %s", intr.Question)
		answer := ask("> ")
		j.Note(pr, "customer: %s", answer)
		st, rep, err = a.Graph.Resume(ctx, intr.Checkpoint, answer)
		tally()
	}
	if err != nil {
		log.Fatalf("pizza failed: %v", err)
	}

	switch {
	case st.OrderID != "":
		j.Note(pr, "order %s: %s", st.OrderID, st.Status)
	case st.Cancelled:
		j.Note(pr, "cancelled")
	default:
		j.Note(pr, "gave up after %d rounds", st.Rounds)
	}
	fmt.Printf("\npath: %s   total $%.4f\n", strings.Join(path, " → "), cost)
	fmt.Printf("replay: go run ./examples/pizza -replay %s -speed 4\n", runs)
	fmt.Printf("trace:  go run ./cmd/cag trace %s --open\n", runs)
}

func newCartID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "cart-" + hex.EncodeToString(b[:])
}
