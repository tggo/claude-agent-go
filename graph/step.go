package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tggo/claude-agent-go/runner"
)

// StepHooks are callbacks a driver of single steps (the temporal module) gets
// while an agent node runs. Both may be nil and are called from the node's
// goroutines.
type StepHooks struct {
	// OnSession is called once per distinct agent session id seen in the
	// node's stream, before anything else reacts to the event.
	OnSession func(id string)
	// OnEvent is called for every stream event of the agent runs the node
	// makes; n is the event count of that run.
	OnEvent runner.ProgressFunc
}

// StepOptions describes the execution a Step call stands for. The zero value
// is the first execution of a node in a fresh run.
type StepOptions struct {
	// RunID is the run id nodes see through RunID(ctx) and IdempotencyKey. An
	// external engine passes its own id (a Temporal workflow id).
	RunID string
	// Step is the 1-based execution number of node in the run. It feeds
	// IdempotencyKey and StepInfo.Step; the driver owns the counter.
	Step int
	// Attempt is the 1-based count of executions of this node in the run
	// (StepInfo.Attempt). 0 means 1.
	Attempt int
	// Answer, when non-nil, is the human's answer: the node sees it through
	// Answer(ctx), exactly like the re-run done by Resume.
	Answer *string
	// Session is an agent session id to continue, as RecoveredSession(ctx)
	// returns it after a crash: Agent and RunAgent then run with --resume.
	Session string
	Hooks   StepHooks
}

// StepResult is the outcome of Runnable.Step.
type StepResult[S any] struct {
	// State is the state after the node. When Paused it is the state the
	// interrupted attempt left, to be passed back with the answer.
	State S
	// Next is the node the graph routes to (End to finish); "" when Paused.
	Next string
	// Paused is true when the node called Interrupt/InterruptReview.
	Paused   bool
	Question string
	Items    []ReviewItem
	// Step is the report entry of this execution (cost, duration, error).
	Step Step
}

// Step executes exactly one node of the graph and resolves its outgoing
// route, for engines that own the control loop (see the temporal module): it
// is Run's loop body for a single iteration, with the same observer calls,
// panic recovery, node timeout, cost accounting and routing checks.
//
// node == Start runs nothing and returns the first node in Next. Parallel
// nodes run their branches inside the call. Step does not write to a
// CheckpointStore (the driver is the durable log) and does not enforce
// WithMaxSteps (the driver counts steps). A node error comes back as the
// *NodeError, with the StepResult still carrying the state and Step record.
// Branch nodes cannot be stepped on their own.
func (r *Runnable[S]) Step(ctx context.Context, node string, s S, opt StepOptions) (StepResult[S], error) {
	if opt.RunID == "" {
		opt.RunID = newRunID()
	}
	res := StepResult[S]{State: s}
	ctx = context.WithValue(ctx, runIDKey{}, opt.RunID)
	if node == Start {
		next, err := r.next(Start, &res.State)
		res.Next = next
		return res, err
	}
	n, ok := r.nodes[node]
	if !ok || r.branches[node] {
		return res, fmt.Errorf("graph %q: step: %q is not a node of this graph", r.name, node)
	}
	if opt.Step < 1 {
		opt.Step = 1
	}
	rec := &recorder{began: time.Now(), runID: opt.RunID}
	rec.seedVisits(nil)
	if opt.Attempt > 1 {
		rec.visits[node] = opt.Attempt - 1
	}
	if opt.Answer != nil {
		ctx = context.WithValue(ctx, answerKey{}, *opt.Answer)
		if ro, ok := r.cfg.obs.(ResumeObserver); ok {
			ro.Resumed(ctx, StepInfo{RunID: rec.runID, Graph: r.name, Node: node, Step: opt.Step}, *opt.Answer)
		}
	} else if g, ok := r.cfg.obs.(Gate); ok {
		if err := g.Before(ctx, StepInfo{RunID: rec.runID, Graph: r.name, Node: node, Step: opt.Step}); err != nil {
			return res, err
		}
	}
	if opt.Session != "" {
		ctx = context.WithValue(ctx, recoveredKey{}, opt.Session)
	}
	fn := n.fn
	if n.par != nil {
		fn = r.parallelFn(n, opt.Step, rec)
	}
	hooks := opt.Hooks
	ctx = context.WithValue(ctx, stepHookKey{}, &hooks)
	err := r.exec(ctx, node, "", opt.Step, rec, fn, &res.State)
	if last := rec.last(); last.Node == node {
		res.Step = last
	}
	if err != nil {
		var ie *interruptError
		if errors.As(err, &ie) {
			res.Paused, res.Question, res.Items = true, ie.Question, slices.Clone(ie.Items)
			return res, nil
		}
		return res, err
	}
	res.Next, err = r.next(node, &res.State)
	return res, err
}

type stepHookKey struct{}
