package runner

import (
	"fmt"
	"time"
)

// Observer receives a RunRecord after each run completes (success or error). It
// is the dependency-free seam for tracing and metrics: build an OpenTelemetry
// span (RunRecord carries StartedAt+Duration, so a retroactive span is exact),
// increment Prometheus counters, or emit a structured log.
//
//	type otelObs struct{ tr trace.Tracer }
//	func (o otelObs) ObserveRun(r runner.RunRecord) {
//	    _, span := o.tr.Start(context.Background(), "claude.run",
//	        trace.WithTimestamp(r.StartedAt))
//	    span.SetAttributes(
//	        attribute.String("claude.model", r.Model),
//	        attribute.String("claude.transport", r.Transport),
//	        attribute.Float64("claude.cost_usd", r.CostUSD),
//	        attribute.Int("claude.turns", r.NumTurns))
//	    if r.Err != nil { span.RecordError(r.Err) }
//	    span.End(trace.WithTimestamp(r.StartedAt.Add(r.Duration)))
//	}
type Observer interface {
	ObserveRun(RunRecord)
}

// RunRecord is a summary of one run, passed to an Observer.
type RunRecord struct {
	Mode         string        // "plain" | "json" | "stream"
	Model        string        // resolved model
	Transport    string        // transport Go type, e.g. "transport.SSH"
	WorkDir      string        // working directory
	SessionID    string        // Claude session id, when known
	CostUSD      float64       // cost of the run (cumulative across retries)
	NumTurns     int           // agentic turns, when known
	InputTokens  int           // input tokens (JSON mode)
	OutputTokens int           // output tokens (JSON mode)
	Attempts     int           // 1 unless run via *WithRetry
	StartedAt    time.Time     // run start
	Duration     time.Duration // wall-clock duration
	Err          error         // non-nil on failure
}

// observe builds and emits a RunRecord if an Observer is configured.
func (r *Runner) observe(mode string, in Input, start time.Time, res *Result, err error) {
	if r.cfg.Observer == nil {
		return
	}
	rec := RunRecord{
		Mode:      mode,
		Model:     r.modelOf(in),
		Transport: fmt.Sprintf("%T", r.cfg.Transport),
		WorkDir:   in.WorkDir,
		StartedAt: start,
		Duration:  time.Since(start),
		Err:       err,
	}
	if res != nil {
		rec.SessionID = res.SessionID
		rec.CostUSD = res.TotalCostUSD
		rec.NumTurns = res.NumTurns
		rec.Attempts = max1(res.Attempts)
		if res.Metadata != nil && res.Metadata.TokenUsage != nil {
			rec.InputTokens = res.Metadata.TokenUsage.InputTokens
			rec.OutputTokens = res.Metadata.TokenUsage.OutputTokens
		}
	}
	r.cfg.Observer.ObserveRun(rec)
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
