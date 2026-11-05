package graph

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sentinel errors, matchable with errors.Is.
var (
	// ErrMaxSteps: the run executed more nodes than WithMaxSteps allows.
	ErrMaxSteps = errors.New("graph: max steps exceeded")
	// ErrInvalidRoute: a RouteFunc returned a node outside its declared targets.
	ErrInvalidRoute = errors.New("graph: invalid route")
	// ErrInterruptInParallel: a parallel branch or merge called Interrupt.
	ErrInterruptInParallel = errors.New("graph: interrupt is not supported inside a parallel node")
)

// NodeError wraps an error returned (or panicked) by a node.
type NodeError struct {
	Graph string
	Node  string
	Err   error
}

func (e *NodeError) Error() string {
	return fmt.Sprintf("graph %q: node %q: %v", e.Graph, e.Node, e.Err)
}
func (e *NodeError) Unwrap() error { return e.Err }

// PanicError is the error a recovered node panic becomes.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Runnable is a compiled, immutable graph. It is safe to call Run and Resume
// concurrently; each call has its own state.
type Runnable[S any] struct {
	name     string
	nodes    map[string]*node[S]
	order    []string
	static   map[string]string
	cond     map[string]*condEdge[S]
	branches map[string]bool
	cfg      options
	clone    func(S) S
}

// Run executes the graph from Start with init as the initial state. It returns
// the final state and a Report even on failure (the state is then however far
// the run got). A node that called Interrupt makes Run return a *Interrupted[S]
// error; pass its Checkpoint to Resume.
func (r *Runnable[S]) Run(ctx context.Context, init S) (S, *Report, error) {
	rec := &recorder{began: time.Now()}
	s := init
	first, err := r.next(Start, &s)
	if err != nil {
		return s, rec.report(), err
	}
	return r.loop(ctx, s, first, 0, nil, "", false, rec)
}

// Resume continues a run paused by Interrupt: it re-executes cp.Node with the
// answer available through Answer, then carries on. The node starts from the
// top, with cp.State as left by the interrupted attempt, so a node that can be
// interrupted should check Answer first and keep any partial work in the
// state. The step budget and Report.Path continue from the checkpoint; Steps
// and durations cover only this call.
func (r *Runnable[S]) Resume(ctx context.Context, cp Checkpoint[S], answer string) (S, *Report, error) {
	rec := &recorder{began: time.Now()}
	n, ok := r.nodes[cp.Node]
	if !ok || r.branches[cp.Node] {
		return cp.State, rec.report(), fmt.Errorf("graph %q: resume: checkpoint node %q is not a node of this graph", r.name, cp.Node)
	}
	if cp.Step < 0 {
		return cp.State, rec.report(), fmt.Errorf("graph %q: resume: negative step in checkpoint", r.name)
	}
	return r.loop(ctx, cp.State, n.name, cp.Step, slices.Clone(cp.Path), answer, true, rec)
}

func (r *Runnable[S]) loop(ctx context.Context, s S, cur string, step int, path []string, answer string, hasAnswer bool, rec *recorder) (S, *Report, error) {
	rec.path = path
	for cur != End {
		if err := ctx.Err(); err != nil {
			return s, rec.report(), fmt.Errorf("graph %q: cancelled before node %q: %w", r.name, cur, err)
		}
		if step >= r.cfg.maxSteps {
			return s, rec.report(), fmt.Errorf("%w (%d) in graph %q before node %q, path %v", ErrMaxSteps, r.cfg.maxSteps, r.name, cur, rec.path)
		}
		n := r.nodes[cur]
		step++
		rec.path = append(rec.path, cur)

		nctx := ctx
		if hasAnswer {
			nctx = context.WithValue(ctx, answerKey{}, answer)
			hasAnswer = false
		}
		var fn NodeFunc[S] = n.fn
		if n.par != nil {
			fn = r.parallelFn(n, step, rec)
		}
		err := r.exec(nctx, n.name, "", step, rec, fn, &s)
		if err != nil {
			var ie *interruptError
			if errors.As(err, &ie) {
				cp := Checkpoint[S]{State: s, Node: n.name, Step: step - 1, Path: slices.Clone(rec.path)}
				// Step counts executions completed before this one, so the
				// resumed attempt re-takes the same step number.
				return s, rec.report(), &Interrupted[S]{Question: ie.Question, Node: n.name, Checkpoint: cp}
			}
			return s, rec.report(), err
		}
		if cur, err = r.next(n.name, &s); err != nil {
			return s, rec.report(), err
		}
	}
	return s, rec.report(), nil
}

// next resolves the outgoing route of from.
func (r *Runnable[S]) next(from string, s *S) (to string, err error) {
	if t, ok := r.static[from]; ok {
		return t, nil
	}
	c := r.cond[from]
	defer func() {
		if p := recover(); p != nil {
			err = &NodeError{Graph: r.name, Node: from, Err: fmt.Errorf("route panicked: %v", p)}
		}
	}()
	t := c.route(s)
	if !slices.Contains(c.targets, t) {
		return "", fmt.Errorf("%w: graph %q: node %q routed to %q, allowed targets: %s", ErrInvalidRoute, r.name, from, t, quoteList(c.targets))
	}
	return t, nil
}

// exec runs one node function with scope, timing, observer, recover and
// recording. It is used for top-level nodes, parallel nodes and branches.
func (r *Runnable[S]) exec(ctx context.Context, name, parent string, step int, rec *recorder, fn NodeFunc[S], s *S) (err error) {
	sc := &scope{name: name}
	ctx = context.WithValue(ctx, scopeKey{}, sc)
	if r.cfg.nodeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.nodeTimeout)
		defer cancel()
	}
	info := StepInfo{Graph: r.name, Node: name, Step: step, Parent: parent}
	started := time.Now()
	if r.cfg.obs != nil {
		r.cfg.obs.NodeStart(ctx, info)
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = &PanicError{Value: p, Stack: debug.Stack()}
			}
		}()
		err = fn(ctx, s)
	}()
	// A pause is not a failure. Inside a parallel node an interrupt is
	// rejected later, so there it stays an error.
	var ie *interruptError
	paused := err != nil && parent == "" && errors.As(err, &ie)
	if err != nil && !paused {
		var ne *NodeError
		if !errors.As(err, &ne) {
			err = &NodeError{Graph: r.name, Node: name, Err: err}
		}
	}
	st := Step{Node: name, Parent: parent, Started: started, Duration: time.Since(started), CostUSD: sc.total()}
	obsErr := err
	if paused {
		st.Paused, st.Question, obsErr = true, ie.Question, nil
	} else {
		st.Err = err
	}
	rec.add(st)
	if r.cfg.obs != nil {
		info.Duration, info.CostUSD, info.Paused, info.Question = st.Duration, st.CostUSD, st.Paused, st.Question
		r.cfg.obs.NodeEnd(ctx, info, obsErr)
	}
	return err
}

func (r *Runnable[S]) cloneState(s S) S {
	if r.clone != nil {
		return r.clone(s)
	}
	return s
}

// parallelFn builds the NodeFunc that fans out to the branches of n.
func (r *Runnable[S]) parallelFn(n *node[S], step int, rec *recorder) NodeFunc[S] {
	return func(ctx context.Context, s *S) error {
		p := n.par
		bctx, cancel := context.WithCancel(ctx)
		defer cancel()

		results := make([]S, len(p.branches))
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			first error
		)
		for i, b := range p.branches {
			results[i] = r.cloneState(*s)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := r.exec(bctx, b, n.name, step, rec, r.nodes[b].fn, &results[i])
				var ie *interruptError
				if errors.As(err, &ie) {
					err = &NodeError{Graph: r.name, Node: b, Err: fmt.Errorf("%w (question %q)", ErrInterruptInParallel, ie.Question)}
				}
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if first != nil {
			return first
		}
		err := p.merge(s, results)
		var ie *interruptError
		if errors.As(err, &ie) {
			return fmt.Errorf("%w (question %q)", ErrInterruptInParallel, ie.Question)
		}
		return err
	}
}

// Func adapts a context-free step to a NodeFunc.
func Func[S any](fn func(s *S) error) NodeFunc[S] {
	return func(_ context.Context, s *S) error { return fn(s) }
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ", ")
}
