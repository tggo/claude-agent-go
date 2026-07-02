package runner

import (
	"context"
	"strings"
	"testing"
)

type capture struct{ recs []RunRecord }

func (c *capture) ObserveRun(r RunRecord) { c.recs = append(c.recs, r) }

func TestObserverSuccess(t *testing.T) {
	out := `[{"type":"system","subtype":"init","session_id":"s","model":"haiku"},` +
		`{"type":"result","session_id":"s","result":"ok","total_cost_usd":0.03,"num_turns":2,"usage":{"input_tokens":11,"output_tokens":7}}]`
	bin := fakeClaude(t, out)
	wd := t.TempDir()
	obs := &capture{}
	r := New(WithBinary(bin), WithModel("haiku"), WithObserver(obs))

	if _, err := r.RunJSON(context.Background(), Input{Prompt: "hi", WorkDir: wd}); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if len(obs.recs) != 1 {
		t.Fatalf("records = %d, want 1", len(obs.recs))
	}
	rec := obs.recs[0]
	if rec.Mode != "json" || rec.Model != "haiku" || rec.WorkDir != wd {
		t.Errorf("rec = %+v", rec)
	}
	if rec.CostUSD != 0.03 || rec.NumTurns != 2 || rec.InputTokens != 11 || rec.OutputTokens != 7 {
		t.Errorf("metrics = %+v", rec)
	}
	if !strings.Contains(rec.Transport, "Local") {
		t.Errorf("transport = %q", rec.Transport)
	}
	if rec.Err != nil || rec.Attempts != 1 || rec.Duration <= 0 {
		t.Errorf("rec err/attempts/duration = %+v", rec)
	}
}

func TestObserverError(t *testing.T) {
	bin := failingClaude(t)
	obs := &capture{}
	r := New(WithBinary(bin), WithObserver(obs))
	if _, err := r.Run(context.Background(), Input{Prompt: "x"}); err == nil {
		t.Fatal("expected error")
	}
	if len(obs.recs) != 1 || obs.recs[0].Err == nil || obs.recs[0].Mode != "plain" {
		t.Errorf("error record wrong: %+v", obs.recs)
	}
}

func TestObserverNilSafe(t *testing.T) {
	bin := fakeClaude(t, "hi")
	r := New(WithBinary(bin)) // no observer
	if _, err := r.Run(context.Background(), Input{Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
