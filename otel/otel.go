// Package otel turns claude-agent-go runs and graph executions into
// OpenTelemetry spans. It is a separate Go module so the core SDK stays free
// of the OpenTelemetry dependency.
//
//	obs := otel.NewObserver(tracer)
//	r := runner.New(runner.WithObserver(obs))
//
//	g.Compile(graph.WithObserver(otel.NewGraphObserver(tracer)))
package otel

import (
	"context"
	"sync"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/runner"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Observer implements runner.Observer: one "claude.run" span per run, built
// retroactively from the RunRecord so its start and end times are exact.
type Observer struct{ tracer trace.Tracer }

var _ runner.Observer = (*Observer)(nil)

// NewObserver returns a runner.Observer that records spans on tracer.
func NewObserver(tracer trace.Tracer) *Observer { return &Observer{tracer: tracer} }

// ObserveRun implements runner.Observer.
func (o *Observer) ObserveRun(r runner.RunRecord) {
	name := "claude.run"
	if r.Label != "" {
		name += " " + r.Label
	}
	_, span := o.tracer.Start(context.Background(), name,
		trace.WithTimestamp(r.StartedAt), trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(runAttrs(r)...)
	if r.Err != nil {
		span.RecordError(r.Err, trace.WithTimestamp(r.StartedAt.Add(r.Duration)))
		span.SetStatus(codes.Error, r.Err.Error())
	}
	span.End(trace.WithTimestamp(r.StartedAt.Add(r.Duration)))
}

func runAttrs(r runner.RunRecord) []attribute.KeyValue {
	a := []attribute.KeyValue{
		attribute.String("claude.mode", r.Mode),
		attribute.String("claude.model", r.Model),
		attribute.String("claude.transport", r.Transport),
		attribute.Float64("claude.cost_usd", r.CostUSD),
		attribute.Int("claude.turns", r.NumTurns),
		attribute.Int("claude.input_tokens", r.InputTokens),
		attribute.Int("claude.output_tokens", r.OutputTokens),
		attribute.Int("claude.attempts", r.Attempts),
	}
	if r.Label != "" {
		a = append(a, attribute.String("claude.label", r.Label))
	}
	if r.SessionID != "" {
		a = append(a, attribute.String("claude.session_id", r.SessionID))
	}
	if r.WorkDir != "" {
		a = append(a, attribute.String("claude.workdir", r.WorkDir))
	}
	return a
}

// GraphObserver implements graph.Observer: node executions become spans.
// A node's span is a child of its parallel node's span (for a branch), else a
// child of the graph run's span when ctx carries one (see StartRun), else a
// root. runner runs started inside a node share its ctx, so with a
// context-propagating runner integration they nest under the node.
type GraphObserver struct {
	tracer trace.Tracer
	mu     sync.Mutex
	open   map[nodeKey]trace.Span
}

var _ graph.Observer = (*GraphObserver)(nil)

type nodeKey struct {
	graph, node string
	step        int
}

// NewGraphObserver returns a graph.Observer recording on tracer.
func NewGraphObserver(tracer trace.Tracer) *GraphObserver {
	return &GraphObserver{tracer: tracer, open: map[nodeKey]trace.Span{}}
}

// StartRun opens the parent span for one graph run. Use the returned ctx for
// the run call and end the span afterwards, so node spans nest under it:
//
//	ctx, end := obs.StartRun(ctx, "pizza")
//	state, rep, err := g.Run(ctx, in)
//	end(err)
func (g *GraphObserver) StartRun(ctx context.Context, graphName string) (context.Context, func(error)) {
	ctx, span := g.tracer.Start(ctx, "graph.run "+graphName,
		trace.WithAttributes(attribute.String("graph.name", graphName)))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// NodeStart implements graph.Observer.
func (g *GraphObserver) NodeStart(ctx context.Context, info graph.StepInfo) {
	k := nodeKey{info.Graph, info.Node, info.Step}
	parentCtx := ctx
	if info.Parent != "" {
		g.mu.Lock()
		p := g.open[nodeKey{info.Graph, info.Parent, info.Step}]
		g.mu.Unlock()
		if p != nil {
			parentCtx = trace.ContextWithSpan(ctx, p)
		}
	}
	_, span := g.tracer.Start(parentCtx, "graph.node "+info.Node, trace.WithAttributes(
		attribute.String("graph.name", info.Graph),
		attribute.String("graph.node", info.Node),
		attribute.Int("graph.step", info.Step),
	))
	g.mu.Lock()
	g.open[k] = span
	g.mu.Unlock()
}

// NodeEnd implements graph.Observer.
func (g *GraphObserver) NodeEnd(_ context.Context, info graph.StepInfo, err error) {
	k := nodeKey{info.Graph, info.Node, info.Step}
	g.mu.Lock()
	span := g.open[k]
	delete(g.open, k)
	g.mu.Unlock()
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.Float64("graph.cost_usd", info.CostUSD),
		attribute.Int64("graph.duration_ms", info.Duration.Milliseconds()),
	)
	if info.Parent != "" {
		span.SetAttributes(attribute.String("graph.parent", info.Parent))
	}
	if info.Paused {
		span.SetAttributes(attribute.Bool("graph.paused", true), attribute.String("graph.question", info.Question))
		span.AddEvent("interrupt", trace.WithAttributes(attribute.String("question", info.Question)))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
