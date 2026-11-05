//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tggo/claude-agent-go/eval"
	"github.com/tggo/claude-agent-go/graph"
)

// TestIntegrationPizza drives the real model (through ANTHROPIC_* from the
// repo .env, e.g. cerber) against the mocks. The human is scripted: each case
// lists the answers given at each pause. Assertions are on the mock's state:
// what was actually placed and paid, not what the model said.
//
//	go test -tags integration -v ./examples/pizza/
func TestIntegrationPizza(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	LoadDotEnv("")
	model := os.Getenv("PIZZA_MODEL")
	if model == "" {
		model = "haiku"
	}

	type script struct {
		outOfStock []string
		answers    []string
		check      func(t *testing.T, s *Shop, st State)
	}
	scripts := map[string]script{
		"happy": {answers: []string{"yes"}, check: func(t *testing.T, s *Shop, st State) {
			o := one(t, s)
			if len(o.Draft.Lines) != 1 || o.Draft.Lines[0] != (Line{"pepperoni", Large, 2}) {
				t.Errorf("lines = %+v", o.Draft.Lines)
			}
			if o.Draft.TotalKop != 57200+6000 {
				t.Errorf("total = %d", o.Draft.TotalKop)
			}
		}},
		"out-of-stock-alternative": {outOfStock: []string{"margherita"}, answers: []string{"yes"}, check: func(t *testing.T, s *Shop, st State) {
			o := one(t, s)
			for _, l := range o.Draft.Lines {
				if l.ItemID != "veggie" {
					t.Errorf("want veggie fallback, got %+v", o.Draft.Lines)
				}
			}
		}},
		"change-then-confirm": {answers: []string{"make it medium instead of large", "yes"}, check: func(t *testing.T, s *Shop, st State) {
			o := one(t, s)
			if len(o.Draft.Lines) != 1 || o.Draft.Lines[0].Size != Medium || o.Draft.Lines[0].ItemID != "diavola" {
				t.Errorf("lines = %+v", o.Draft.Lines)
			}
			if st.Rounds != 2 {
				t.Errorf("rounds = %d", st.Rounds)
			}
		}},
		"outside-zone-asks": {answers: []string{"sorry, deliver to Sahaidachnoho 10, Kyiv", "yes"}, check: func(t *testing.T, s *Shop, st State) {
			if o := one(t, s); o.Draft.Address == "" || o.Draft.DeliveryKop != 6000 {
				t.Errorf("draft = %+v", o.Draft)
			}
		}},
	}

	set := eval.Set{ID: "pizza", Cases: []eval.Case{
		{ID: "happy", Prompt: "Two large pepperoni to Khreshchatyk 22, Kyiv please",
			ExpectedTools: []eval.ToolCall{{Name: "get_menu"}, {Name: "quote_delivery"}, {Name: "propose_order"}}, Match: eval.InOrder, IgnoreArgs: true},
		{ID: "out-of-stock-alternative", Prompt: "One margherita, or veggie if there's no margherita. Velyka Vasylkivska 5, Kyiv",
			ExpectedTools: []eval.ToolCall{{Name: "get_menu"}, {Name: "propose_order"}}, Match: eval.InOrder, IgnoreArgs: true},
		{ID: "change-then-confirm", Prompt: "A large diavola to Khreshchatyk 1, Kyiv"},
		{ID: "outside-zone-asks", Prompt: "Pepperoni to Rynok square 1, Lviv",
			// Must not propose before the address is fixed: quote fails, agent asks.
			ExpectedTools: []eval.ToolCall{{Name: "quote_delivery"}, {Name: "propose_order"}}, Match: eval.InOrder, IgnoreArgs: true},
	}}

	agent := func(ctx context.Context, c eval.Case) (eval.Run, error) {
		sc := scripts[c.ID]
		dir := t.TempDir()
		a, err := Build(Options{Model: model, RunsDir: filepath.Join(dir, "runs")})
		if err != nil {
			return eval.Run{}, err
		}
		defer a.Close()
		for _, id := range sc.outOfStock {
			a.Shop.SetOutOfStock(id)
		}
		st, rep, err := a.Graph.Run(ctx, State{Request: c.Prompt, CartID: "cart-" + c.ID})
		answers := sc.answers
		for {
			var intr *graph.Interrupted[State]
			if !errors.As(err, &intr) {
				break
			}
			if len(answers) == 0 {
				t.Errorf("%s: unexpected pause: %s", c.ID, intr.Question)
				return eval.Run{}, err
			}
			t.Logf("%s ⏸ %s\n  > %s", c.ID, intr.Question, answers[0])
			st, rep, err = a.Graph.Resume(ctx, intr.Checkpoint, answers[0])
			answers = answers[1:]
		}
		if err != nil {
			return eval.Run{}, err
		}
		t.Logf("%s: path=%v $%.4f order=%s %s", c.ID, rep.Path, rep.TotalCostUSD, st.OrderID, st.Status)
		sc.check(t, a.Shop, st)
		return eval.FromRecordingDir(a.RunsDir)
	}

	eval.RunT(t, set, agent, eval.Criteria{
		TrajectoryThreshold: eval.Threshold(1.0),
		MatchOptions:        []eval.MatchOption{eval.MCPNames()},
		MaxCostUSD:          0.50,
	})
}

func one(t *testing.T, s *Shop) Order {
	t.Helper()
	os := s.Orders()
	if len(os) != 1 || os[0].Status != "paid" {
		t.Errorf("orders = %+v", os)
		return Order{}
	}
	return os[0]
}
