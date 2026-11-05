package graph

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Step is the record of one node execution.
type Step struct {
	Node     string
	Parent   string // the parallel node, for a branch; empty otherwise
	Started  time.Time
	Duration time.Duration
	Err      error
	CostUSD  float64
	// Paused is true when the node called Interrupt. Err is nil then; the
	// question is in Question.
	Paused   bool
	Question string
}

// Report summarises a run (or the resumed part of one).
type Report struct {
	// Steps lists executions in completion order. Branches of a parallel node
	// appear before the parallel node's own step.
	Steps []Step
	// TotalCostUSD sums the cost of every step. Branch costs are attributed to
	// the branch steps only, so nothing is counted twice.
	TotalCostUSD float64
	// Duration is the wall-clock time of the call.
	Duration time.Duration
	// Path is the top-level execution trace (parallel branches excluded). After
	// Resume it continues the checkpoint's path, and an interrupted node
	// appears once per attempt.
	Path []string
}

// StepInfo identifies a node execution to an Observer.
type StepInfo struct {
	Graph  string
	Node   string
	Step   int    // 1-based execution number; branches share their parallel node's
	Parent string // the parallel node, for a branch
	// Duration and CostUSD are set on NodeEnd only.
	Duration time.Duration
	CostUSD  float64
	// Paused and Question are set on NodeEnd when the node called Interrupt
	// (the err argument is nil then).
	Paused   bool
	Question string
}

// Observer receives node lifecycle events. Calls for one run are made in
// execution order, except parallel branches, which come from several
// goroutines.
type Observer interface {
	NodeStart(ctx context.Context, info StepInfo)
	NodeEnd(ctx context.Context, info StepInfo, err error)
}

type recorder struct {
	mu    sync.Mutex
	began time.Time
	steps []Step
	path  []string
}

func (r *recorder) add(s Step) {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
}

func (r *recorder) report() *Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := &Report{Steps: slices.Clone(r.steps), Duration: time.Since(r.began), Path: slices.Clone(r.path)}
	for _, s := range r.steps {
		rep.TotalCostUSD += s.CostUSD
	}
	return rep
}

type (
	scopeKey  struct{}
	answerKey struct{}
)

// scope is the per-node-execution cost accumulator carried in the context.
type scope struct {
	name string
	mu   sync.Mutex
	cost float64
}

func (s *scope) total() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost
}

// AddCost attributes usd to the node execution ctx belongs to. It is safe to
// call from any goroutine, and is a no-op outside a node.
func AddCost(ctx context.Context, usd float64) {
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		s.mu.Lock()
		s.cost += usd
		s.mu.Unlock()
	}
}

// NodeName returns the name of the node ctx belongs to ("" outside a node).
func NodeName(ctx context.Context) string {
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		return s.name
	}
	return ""
}
