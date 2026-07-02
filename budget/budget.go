// Package budget tracks cumulative spend across many agent runs and fires
// callbacks as it crosses a warning threshold or a hard cap. It is a standalone,
// thread-safe building block: feed it each run's cost (e.g. from
// runner.Result.TotalCostUSD or fleet.TaskResult) and gate work with CanSpend.
//
// This complements the per-invocation --max-budget-usd flag and RetryPolicy's
// per-call cap with a process-wide / fleet-wide ceiling.
package budget

import (
	"math"
	"sync"
)

var posInf = math.Inf(1)

// Tracker accumulates spend and enforces an optional cap. The zero value is not
// usable; construct with New.
type Tracker struct {
	mu         sync.Mutex
	max        float64            // hard cap in USD; 0 = unlimited
	warnFrac   float64            // warn once spent/max crosses this (0..1); 0 disables
	spent      float64            // cumulative spend
	perSession map[string]float64 // spend by session id
	warned     bool
	onWarn     func(Snapshot)
	onExceed   func(Snapshot)
}

// Snapshot is a point-in-time view passed to callbacks.
type Snapshot struct {
	SessionID string
	Spent     float64
	Max       float64
	Remaining float64
}

// Option configures a Tracker.
type Option func(*Tracker)

// WithWarnFraction fires OnWarn (once) when spend crosses frac*Max (0..1).
func WithWarnFraction(frac float64) Option {
	return func(t *Tracker) { t.warnFrac = frac }
}

// OnWarn sets the callback fired once when the warn threshold is crossed.
func OnWarn(fn func(Snapshot)) Option { return func(t *Tracker) { t.onWarn = fn } }

// OnExceed sets the callback fired each time an Add pushes spend over the cap.
func OnExceed(fn func(Snapshot)) Option { return func(t *Tracker) { t.onExceed = fn } }

// New creates a Tracker with a hard cap (USD; 0 = unlimited) and options.
func New(maxUSD float64, opts ...Option) *Tracker {
	t := &Tracker{max: maxUSD, perSession: map[string]float64{}}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Add records usd spent (optionally attributed to sessionID) and fires the
// warn/exceed callbacks as thresholds are crossed. Callbacks run AFTER the lock
// is released, so they may call back into the Tracker without deadlocking.
func (t *Tracker) Add(sessionID string, usd float64) {
	t.mu.Lock()
	t.spent += usd
	if sessionID != "" {
		t.perSession[sessionID] += usd
	}
	spent, max := t.spent, t.max
	var fireWarn, fireExceed bool
	if max > 0 {
		if t.warnFrac > 0 && !t.warned && spent >= t.warnFrac*max {
			t.warned = true
			fireWarn = true
		}
		if spent > max {
			fireExceed = true
		}
	}
	t.mu.Unlock()

	snap := Snapshot{SessionID: sessionID, Spent: spent, Max: max, Remaining: remaining(spent, max)}
	if fireWarn && t.onWarn != nil {
		t.onWarn(snap)
	}
	if fireExceed && t.onExceed != nil {
		t.onExceed(snap)
	}
}

// Spent returns cumulative spend.
func (t *Tracker) Spent() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent
}

// SessionSpent returns spend attributed to a session id.
func (t *Tracker) SessionSpent(id string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.perSession[id]
}

// Remaining returns how much budget is left, or +Inf when uncapped.
func (t *Tracker) Remaining() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return remaining(t.spent, t.max)
}

// Exceeded reports whether spend has passed the cap.
func (t *Tracker) Exceeded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.max > 0 && t.spent > t.max
}

// CanSpend reports whether estUSD more can be spent without exceeding the cap.
// Uncapped trackers always return true.
func (t *Tracker) CanSpend(estUSD float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.max <= 0 {
		return true
	}
	return t.spent+estUSD <= t.max
}

func remaining(spent, max float64) float64 {
	if max <= 0 {
		return posInf
	}
	if r := max - spent; r > 0 {
		return r
	}
	return 0
}
