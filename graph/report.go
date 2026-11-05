package graph

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Step is the record of one node execution.
type Step struct {
	Node     string        `json:"node"`
	Parent   string        `json:"parent,omitempty"` // the parallel node, for a branch; empty otherwise
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	// Err is the node's error. It is not serialized; ErrText carries its
	// message, which is all that survives a Checkpoint round trip.
	Err     error   `json:"-"`
	ErrText string  `json:"err,omitempty"`
	CostUSD float64 `json:"cost_usd"`
	// Paused is true when the node called Interrupt. Err is nil then; the
	// question is in Question.
	Paused   bool   `json:"paused,omitempty"`
	Question string `json:"question,omitempty"`
}

// Report summarises a run. After Resume it is cumulative: it includes the
// steps, cost and duration of every earlier segment recorded in the
// Checkpoint (Checkpoint.Prior), so the final Report covers the whole run.
type Report struct {
	// RunID is the id of the run (see RunID(ctx)), for Recover and Fork.
	RunID string
	// Steps lists executions in completion order. Branches of a parallel node
	// appear before the parallel node's own step.
	Steps []Step
	// TotalCostUSD sums the cost of every step. Branch costs are attributed to
	// the branch steps only, so nothing is counted twice.
	TotalCostUSD float64
	// Duration is the wall-clock time of the calls that make up the run
	// (time spent paused, waiting for the human, is not counted).
	Duration time.Duration
	// Path is the top-level execution trace (parallel branches excluded). After
	// Resume it continues the checkpoint's path, and an interrupted node
	// appears once per attempt.
	Path []string
}

// StepInfo identifies a node execution to an Observer.
type StepInfo struct {
	// RunID is shared by every segment (Run, Resume) of one logical run.
	RunID  string
	Graph  string
	Node   string
	Step   int    // 1-based execution number; branches share their parallel node's
	Parent string // the parallel node, for a branch
	// Attempt counts executions of this node in the run, 1-based (a node in a
	// loop, or re-run by Resume, has Attempt > 1).
	Attempt int
	// Duration and CostUSD are set on NodeEnd only.
	Duration time.Duration
	CostUSD  float64
	// Paused and Question are set on NodeEnd when the node called Interrupt
	// (the err argument is nil then).
	Paused   bool
	Question string
	// Items is set with Paused when the node called InterruptReview.
	Items []ReviewItem
}

// Observer receives node lifecycle events. Calls for one run are made in
// execution order, except parallel branches, which come from several
// goroutines.
type Observer interface {
	NodeStart(ctx context.Context, info StepInfo)
	NodeEnd(ctx context.Context, info StepInfo, err error)
}

type recorder struct {
	mu     sync.Mutex
	began  time.Time
	steps  []Step
	path   []string
	prior  Prior
	runID  string
	visits map[string]int

	ck         *ckWriter // nil unless WithCheckpointStore
	recNode    string    // Recover: the node whose logged session is resumed
	recSession string
}

// last is the most recently completed step (the node that just ran).
func (r *recorder) last() Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.steps) == 0 {
		return Step{}
	}
	return r.steps[len(r.steps)-1]
}

// peek returns how often name has run so far.
func (r *recorder) peek(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.visits[name]
}

// seedVisits counts the executions already in path (a resumed run).
func (r *recorder) seedVisits(path []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visits = map[string]int{}
	for _, n := range path {
		r.visits[n]++
	}
}

// visit returns the 1-based execution count of name including this one,
// continuing from what seedVisits counted from the checkpoint path.
func (r *recorder) visit(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.visits == nil {
		r.visits = map[string]int{}
	}
	r.visits[name]++
	return r.visits[name]
}

func (r *recorder) add(s Step) {
	if s.Err != nil {
		s.ErrText = s.Err.Error()
	}
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
}

func (r *recorder) report() *Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := &Report{
		RunID:    r.runID,
		Steps:    append(slices.Clone(r.prior.Steps), r.steps...),
		Duration: r.prior.Duration + time.Since(r.began),
		Path:     slices.Clone(r.path),
	}
	for _, s := range rep.Steps {
		rep.TotalCostUSD += s.CostUSD
	}
	return rep
}

type (
	runIDKey  struct{}
	scopeKey  struct{}
	answerKey struct{}
)

// scope is the per-node-execution cost accumulator carried in the context.
type scope struct {
	name string
	info StepInfo
	obs  Observer
	ck   *ckWriter  // log of the run; nil without a store or inside a parallel branch
	hook *StepHooks // set by Runnable.Step
	mu   sync.Mutex
	cost float64
	sess string // last agent session id logged
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

// RunID returns the id of the run ctx belongs to: the context handed to nodes
// and to every Observer call carries it ("" outside a run).
func RunID(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
}
