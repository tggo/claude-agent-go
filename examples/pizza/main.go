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
	"github.com/tggo/claude-agent-go/graphweb"
)

func main() {
	order := flag.String("order", "", "what you want (asked interactively if empty)")
	out := flag.String("out", "", "runs dir (default: a new temp dir)")
	replay := flag.String("replay", "", "play back a runs dir instead of ordering")
	speed := flag.Float64("speed", 1, "replay speed multiplier (0 = no pauses)")
	outOfStock := flag.String("out-of-stock", "", "comma-separated menu ids to mark unavailable")
	web := flag.String("web", "", "serve the live graph UI on this address (e.g. 127.0.0.1:8799); the confirmation is then answered in the browser")
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
	opts := Options{Model: model, RunsDir: runs, Progress: pr.Progress()}
	var ui *graphweb.Server
	if *web != "" {
		if err := os.MkdirAll(runs, 0o755); err != nil {
			log.Fatal(err)
		}
		var err error
		if ui, err = graphweb.New(graphweb.WithJournal(filepath.Join(runs, graphweb.DefaultJournal))); err != nil {
			log.Fatal(err)
		}
		defer ui.Close()
		url, err := ui.Start(context.Background(), *web)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("web: %s   (answer the confirmation there)\n", url)
		opts.Observer = ui
	}
	a, err := Build(opts)
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
	init := State{Request: *order, CartID: newCartID()}
	var (
		st   State
		rep  *graph.Report
		cost float64
		path []string
	)
	if ui != nil {
		// The UI shows the question and posts the answer; the graph resumes.
		st, rep, err = graphweb.RunInteractive(ctx, a.Graph, init, ui)
		if rep != nil {
			cost, path = rep.TotalCostUSD, rep.Path // cumulative across resumes
		}
	} else {
		st, rep, err = a.Graph.Run(ctx, init)
		// Resume returns a cumulative report, so the last one is the total.
		tally := func() {
			if rep != nil {
				cost, path = rep.TotalCostUSD, rep.Path
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
	if ui != nil {
		fmt.Printf("web:    go run ./cmd/cag web --replay %s --open\n", runs)
	}
}

func newCartID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "cart-" + hex.EncodeToString(b[:])
}
