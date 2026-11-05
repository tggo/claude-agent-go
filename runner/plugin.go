package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tggo/claude-agent-go/claudecli"
)

// Plugin hooks into every run of a Runner (see WithPlugins). Name identifies
// the plugin in errors. A plugin implements only the optional interfaces it
// needs: BeforeRun, EventPlugin, AfterRun. Plugins are called in registration
// order, and one Plugin value serves concurrent runs, so it must be
// goroutine-safe.
//
// Plugins complement, not replace, Observer (a read-only record after a run)
// and ProgressFunc (a per-call callback): a plugin is registered once, sees
// every run, and can veto or rewrite it.
type Plugin interface {
	Name() string
}

// BeforeRun is called before a run starts (Run, RunJSON, RunStream), and may
// return a modified Input. A non-nil error aborts the run before the CLI is
// started; it is returned as a *PluginAbortError.
type BeforeRun interface {
	OnRunStart(ctx context.Context, in Input) (Input, error)
}

// EventPlugin sees every parsed stream event of RunStream before the
// ProgressFunc. A non-nil error aborts the run: the CLI process group is
// killed at once and RunStream returns the partial Result and a
// *PluginAbortError (not retried by DefaultRetryable).
type EventPlugin interface {
	OnEvent(ctx context.Context, ev claudecli.StreamEvent) error
}

// AfterRun is called with the RunRecord after each run, success or failure,
// just after the Observer.
type AfterRun interface {
	OnRunEnd(ctx context.Context, rec *RunRecord)
}

// PluginAbortError is the error a run fails with when a plugin vetoes it.
type PluginAbortError struct {
	Plugin string
	// SessionID is the session id seen before the abort (stream mode), or "".
	SessionID string
	Err       error
}

func (e *PluginAbortError) Error() string {
	return fmt.Sprintf("claude cli: aborted by plugin %q: %v", e.Plugin, e.Err)
}
func (e *PluginAbortError) Unwrap() error { return e.Err }

func (r *Runner) beginRun(ctx context.Context, in Input) (Input, error) {
	if r.cfg.Preflight {
		r.preflightOnce.Do(func() {
			r.preflightInfo, r.preflightErr = r.runPreflight(ctx)
		})
		if r.preflightErr != nil {
			return in, r.preflightErr
		}
	}
	for _, p := range r.cfg.Plugins {
		if b, ok := p.(BeforeRun); ok {
			var err error
			if in, err = b.OnRunStart(ctx, in); err != nil {
				return in, &PluginAbortError{Plugin: p.Name(), Err: err}
			}
		}
	}
	return in, nil
}

func (r *Runner) pluginEvent(ctx context.Context, ev claudecli.StreamEvent) error {
	for _, p := range r.cfg.Plugins {
		if e, ok := p.(EventPlugin); ok {
			if err := e.OnEvent(ctx, ev); err != nil {
				return &PluginAbortError{Plugin: p.Name(), Err: err}
			}
		}
	}
	return nil
}

func (r *Runner) pluginEnd(rec *RunRecord) {
	for _, p := range r.cfg.Plugins {
		if a, ok := p.(AfterRun); ok {
			a.OnRunEnd(context.Background(), rec)
		}
	}
}

// ErrSpendCap is wrapped by the error SpendCap aborts with.
var ErrSpendCap = errors.New("runner: spend cap reached")

// SpendCap is a Plugin that caps cumulative spend across every run of the
// Runner it is attached to. Spend is read from cost-bearing stream events
// (the CLI reports cost on the result event, so enforcement is per run: a run
// is refused at start once the cap has been reached, and a non-result event
// that carries cost pushing the total over the cap aborts the stream). It
// cannot stop a single run overshooting mid-flight; combine it with
// WithMaxBudgetUSD for a per-run limit. Stream runs are counted from events;
// RunJSON and Run runs through OnRunEnd.
type SpendCap struct {
	max float64

	mu    sync.Mutex
	spent float64
}

// NewSpendCap returns a SpendCap of maxUSD (> 0).
func NewSpendCap(maxUSD float64) *SpendCap { return &SpendCap{max: maxUSD} }

// Name implements Plugin.
func (c *SpendCap) Name() string { return "spend-cap" }

// SpentUSD is the cumulative spend seen so far.
func (c *SpendCap) SpentUSD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spent
}

// OnRunStart refuses a new run once the cap has been reached.
func (c *SpendCap) OnRunStart(_ context.Context, in Input) (Input, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spent >= c.max {
		return in, fmt.Errorf("%w: $%.4f spent of $%.4f", ErrSpendCap, c.spent, c.max)
	}
	return in, nil
}

// OnEvent accumulates event cost and aborts the stream past the cap.
func (c *SpendCap) OnEvent(_ context.Context, ev claudecli.StreamEvent) error {
	cost := ev.Cost()
	if cost <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spent += cost
	// The result event closes the run: aborting there would throw away work
	// already paid for, so it only counts; the next run is refused instead.
	if c.spent > c.max && !ev.IsResult() {
		return fmt.Errorf("%w: $%.4f spent of $%.4f", ErrSpendCap, c.spent, c.max)
	}
	return nil
}

// OnRunEnd counts the cost of runs that never went through OnEvent (JSON mode).
func (c *SpendCap) OnRunEnd(_ context.Context, rec *RunRecord) {
	if rec.Mode == "stream" {
		return
	}
	c.mu.Lock()
	c.spent += rec.CostUSD
	c.mu.Unlock()
}
