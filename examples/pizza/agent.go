package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/tools"
)

// State flows through the graph.
type State struct {
	Request   string `json:"request"`  // what the customer asked for
	CartID    string `json:"cart_id"`  // ties the agent's propose_order calls to this run
	Feedback  string `json:"feedback"` // clarifications / change requests from the human
	Reply     string `json:"reply"`    // agent's last message to the customer
	Confirmed bool   `json:"confirmed"`
	Cancelled bool   `json:"cancelled"`
	OrderID   string `json:"order_id"`
	Status    string `json:"status"`
	Rounds    int    `json:"rounds"`
	// DraftHash is Draft.Hash of the draft the human confirmed; place refuses
	// if the stored draft no longer matches.
	DraftHash string `json:"draft_hash,omitempty"`
}

// MaxRounds bounds order → confirm → change loops.
const MaxRounds = 4

// Agent is the compiled graph plus its mocks.
type Agent struct {
	Graph   *graph.Runnable[State]
	Shop    *Shop
	RunsDir string
	close   func()
}

// Close stops the in-process tool server.
func (a *Agent) Close() { a.close() }

type empty struct{}

type menuOut struct {
	Items []MenuItem `json:"items"`
	Note  string     `json:"note"`
}

type quoteIn struct {
	Address string `json:"address" jsonschema:"full delivery address including city"`
}

type quoteOut struct {
	DeliveryFee string `json:"delivery_fee"`
	ETAMinutes  int    `json:"eta_minutes"`
}

type proposeIn struct {
	CartID  string `json:"cart_id" jsonschema:"the cart id given in the instructions"`
	Lines   []Line `json:"lines"`
	Address string `json:"address"`
	Notes   string `json:"notes,omitempty" jsonschema:"kitchen/courier notes, optional"`
}

type proposeOut struct {
	Total   string `json:"total"`
	Summary string `json:"summary"`
	Next    string `json:"next"`
}

// Options tunes Build.
type Options struct {
	Model    string
	RunsDir  string
	Progress runner.ProgressFunc // live view of every agent run; nil = quiet
}

// Build wires the mocks as Go tools and the graph
//
//	order ──▶ confirm ──yes──▶ place ──▶ end
//	  ▲          └──change──┘
//	  └─ QUESTION: pauses the graph (graph.Interrupt)
func Build(o Options) (*Agent, error) {
	shop := NewShop()
	reg := tools.NewRegistry("pizza")
	tools.Register(reg, "get_menu", "List today's pizzas with ingredients, base price (size M, in kopecks) and stock.",
		func(context.Context, empty) (menuOut, error) {
			return menuOut{Items: shop.Menu(), Note: "Size S = 80% of base, M = 100%, L = 130%."}, nil
		})
	tools.Register(reg, "quote_delivery", "Delivery fee and ETA for an address. Fails outside the delivery zone.",
		func(_ context.Context, in quoteIn) (quoteOut, error) {
			fee, eta, err := shop.QuoteDelivery(in.Address)
			return quoteOut{DeliveryFee: UAH(fee), ETAMinutes: eta}, err
		})
	tools.Register(reg, "propose_order", "Validate and price the order and save it for the customer's confirmation. Does NOT place or pay it.",
		func(_ context.Context, in proposeIn) (proposeOut, error) {
			d, err := shop.Propose(in.CartID, in.Lines, in.Address, in.Notes)
			if err != nil {
				return proposeOut{}, err
			}
			return proposeOut{Total: UAH(d.TotalKop), Summary: d.Summary(shop),
				Next: "Stop here. The customer will confirm; you cannot place the order."}, nil
		})
	mcpPath, closeSrv, err := reg.ServeMCP(o.RunsDir)
	if err != nil {
		return nil, err
	}
	name := "pizza"
	tool := func(n string) string { return "mcp__" + name + "__" + n }

	// The order-taker sees only the pizza tools: no shell, no files, no web.
	taker := runner.New(
		runner.WithModel(o.Model),
		runner.WithTimeout(3*time.Minute),
		runner.WithMaxTurns(12),
		runner.WithMaxBudgetUSD("0.50"),
		runner.WithAllowedTools(tool("get_menu"), tool("quote_delivery"), tool("propose_order")),
		runner.WithRecorder(recording.Dir(o.RunsDir)),
		runner.WithEntrypoint("claude-agent-go-pizza"),
		runner.WithLogger(slog.New(slog.DiscardHandler)),
	)

	agentOpts := []graph.AgentOption{graph.WithRetry(runner.RetryPolicy{MaxAttempts: 2, Resume: true})}
	if o.Progress != nil {
		agentOpts = append(agentOpts, graph.WithProgress(o.Progress))
	}

	g := graph.New[State]("pizza")

	g.AddNode("order", graph.Agent(taker, func(s *State) runner.Input {
		p := `You take pizza delivery orders for "Forno", a pizzeria in Kyiv.
Customer request: ` + s.Request + `
`
		if s.Feedback != "" {
			p += "Customer follow-ups (latest last):" + s.Feedback + "\n"
		}
		p += `
Steps: call get_menu, then quote_delivery for the address, then propose_order
ONCE with cart_id="` + s.CartID + `". Map the request onto real menu ids and sizes
(default size M, qty 1). If something is out of stock and the customer gave an
alternative, use it. You cannot place or pay orders; propose_order is the last step.
If the address is missing, outside the zone, or the request cannot be mapped
to the menu, do NOT propose; reply with one line "QUESTION: <what you need>".
Otherwise reply with a one-sentence summary for the customer.`
		return runner.Input{Prompt: p, MCPConfigPath: mcpPath, ExtraArgs: []string{"--tools", ""}, Label: fmt.Sprintf("order-%d", s.Rounds+1)}
	}, func(s *State, res *runner.Result) error {
		s.Reply = strings.TrimSpace(res.ResultText)
		s.Rounds++
		return nil
	}, agentOpts...))

	// confirm is plain Go and the only way to "place": a human must approve the
	// exact priced draft the mock stored, not the model's description of it.
	g.AddNode("confirm", func(ctx context.Context, s *State) error {
		if a, ok := graph.Answer(ctx); ok {
			switch strings.ToLower(strings.TrimSpace(a)) {
			case "y", "yes", "так", "ok", "confirm":
				d, ok := shop.Draft(s.CartID)
				if !ok {
					return graph.Interrupt("There is no draft to confirm. What should I change?")
				}
				s.Confirmed, s.DraftHash = true, d.Hash()
			case "n", "no", "ні", "cancel":
				s.Cancelled = true
			default:
				s.Feedback += "\n- " + a
			}
			return nil
		}
		if q, ok := strings.CutPrefix(s.Reply, "QUESTION:"); ok {
			return graph.Interrupt(strings.TrimSpace(q))
		}
		d, ok := shop.Draft(s.CartID)
		if !ok {
			return graph.Interrupt("I could not build an order from that: " + s.Reply + "\nWhat should I change?")
		}
		return graph.Interrupt("Please confirm the order (yes / no / or say what to change):\n" + d.Summary(shop))
	})

	g.AddConditionalEdge("confirm", func(s *State) string {
		switch {
		case s.Confirmed:
			return "place"
		case s.Cancelled, s.Rounds >= MaxRounds:
			return graph.End
		default:
			return "order"
		}
	}, "place", "order", graph.End)

	g.AddNode("place", func(_ context.Context, s *State) error {
		o, err := shop.PlaceAndPay(s.CartID, s.DraftHash, "tok_test_visa")
		s.OrderID, s.Status = o.ID, o.Status
		if err != nil {
			s.Status = "payment_failed: " + err.Error()
		}
		return nil
	})

	g.AddEdge(graph.Start, "order")
	g.AddEdge("order", "confirm")
	g.AddEdge("place", graph.End)

	r, err := g.Compile(graph.WithMaxSteps(20))
	if err != nil {
		closeSrv()
		return nil, err
	}
	return &Agent{Graph: r, Shop: shop, RunsDir: o.RunsDir, close: func() { _ = closeSrv() }}, nil
}
