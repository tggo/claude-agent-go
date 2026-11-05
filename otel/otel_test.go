package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/runner"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func setup() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	sr := tracetest.NewSpanRecorder()
	return sr, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
}

func attrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestObserveRun(t *testing.T) {
	sr, tp := setup()
	obs := NewObserver(tp.Tracer("t"))
	start := time.Now().Add(-2 * time.Second)
	var _ runner.Observer = obs

	obs.ObserveRun(runner.RunRecord{Mode: "json", Label: "node-a", Model: "haiku", Transport: "transport.Local",
		WorkDir: "/w", SessionID: "sess", CostUSD: 0.25, NumTurns: 3, InputTokens: 10, OutputTokens: 20,
		Attempts: 2, StartedAt: start, Duration: time.Second})
	obs.ObserveRun(runner.RunRecord{Mode: "plain", Model: "m", StartedAt: start, Duration: time.Millisecond, Err: errors.New("boom")})

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	s := spans[0]
	if s.Name() != "claude.run node-a" {
		t.Errorf("name %q", s.Name())
	}
	if !s.StartTime().Equal(start) || s.EndTime().Sub(s.StartTime()) != time.Second {
		t.Errorf("timestamps not retroactive: %v..%v", s.StartTime(), s.EndTime())
	}
	a := attrs(s)
	if a["claude.model"].AsString() != "haiku" || a["claude.label"].AsString() != "node-a" ||
		a["claude.session_id"].AsString() != "sess" || a["claude.cost_usd"].AsFloat64() != 0.25 ||
		a["claude.turns"].AsInt64() != 3 || a["claude.input_tokens"].AsInt64() != 10 ||
		a["claude.output_tokens"].AsInt64() != 20 || a["claude.attempts"].AsInt64() != 2 ||
		a["claude.workdir"].AsString() != "/w" {
		t.Errorf("attrs %v", a)
	}
	if s.Status().Code == codes.Error {
		t.Error("success span marked error")
	}

	e := spans[1]
	if e.Name() != "claude.run" || e.Status().Code != codes.Error || e.Status().Description != "boom" {
		t.Errorf("error span: %q %v", e.Name(), e.Status())
	}
	if len(e.Events()) != 1 || e.Events()[0].Name != "exception" {
		t.Errorf("events %v", e.Events())
	}
	if _, ok := attrs(e)["claude.label"]; ok {
		t.Error("empty label attribute emitted")
	}
}

func TestGraphObserver(t *testing.T) {
	sr, tp := setup()
	g := NewGraphObserver(tp.Tracer("t"))
	var _ graph.Observer = g

	ctx, end := g.StartRun(context.Background(), "pizza")
	g.NodeStart(ctx, graph.StepInfo{Graph: "pizza", Node: "fan", Step: 1})
	g.NodeStart(ctx, graph.StepInfo{Graph: "pizza", Node: "b1", Step: 1, Parent: "fan"})
	g.NodeEnd(ctx, graph.StepInfo{Graph: "pizza", Node: "b1", Step: 1, Parent: "fan", CostUSD: 0.1, Duration: time.Second}, nil)
	g.NodeEnd(ctx, graph.StepInfo{Graph: "pizza", Node: "fan", Step: 1}, errors.New("bad"))
	g.NodeStart(ctx, graph.StepInfo{Graph: "pizza", Node: "ask", Step: 2})
	g.NodeEnd(ctx, graph.StepInfo{Graph: "pizza", Node: "ask", Step: 2, Paused: true, Question: "size?"}, nil)
	g.NodeEnd(ctx, graph.StepInfo{Graph: "pizza", Node: "ghost", Step: 9}, nil) // unmatched end: ignored
	end(errors.New("run failed"))

	by := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		by[s.Name()] = s
	}
	if len(by) != 4 {
		t.Fatalf("spans: %v", by)
	}
	run, fan, b1, ask := by["graph.run pizza"], by["graph.node fan"], by["graph.node b1"], by["graph.node ask"]

	if run.Parent().IsValid() {
		t.Error("run span should be root")
	}
	if fan.Parent().SpanID() != run.SpanContext().SpanID() || ask.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Error("nodes must be children of the run span")
	}
	if b1.Parent().SpanID() != fan.SpanContext().SpanID() {
		t.Error("branch must be a child of its parallel node")
	}
	if b1.SpanContext().TraceID() != run.SpanContext().TraceID() {
		t.Error("single trace expected")
	}
	if fan.Status().Code != codes.Error || run.Status().Code != codes.Error {
		t.Error("errors not recorded")
	}
	a := attrs(b1)
	if a["graph.cost_usd"].AsFloat64() != 0.1 || a["graph.parent"].AsString() != "fan" || a["graph.duration_ms"].AsInt64() != 1000 {
		t.Errorf("b1 attrs %v", a)
	}
	if !attrs(ask)["graph.paused"].AsBool() || len(ask.Events()) != 1 || ask.Events()[0].Name != "interrupt" {
		t.Errorf("pause not recorded: %v", ask.Events())
	}

	// Without StartRun nodes are roots; branch with unknown parent falls back to ctx.
	sr2, tp2 := setup()
	g2 := NewGraphObserver(tp2.Tracer("t"))
	g2.NodeStart(context.Background(), graph.StepInfo{Graph: "g", Node: "x", Step: 1, Parent: "missing"})
	g2.NodeEnd(context.Background(), graph.StepInfo{Graph: "g", Node: "x", Step: 1, Parent: "missing"}, nil)
	if s := sr2.Ended(); len(s) != 1 || s[0].Parent().IsValid() {
		t.Errorf("expected one root span, got %v", s)
	}
	end2 := func() { _, e := g2.StartRun(context.Background(), "ok"); e(nil) }
	end2()
}
