package graph

import (
	"context"
	"encoding/json"
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
	labels   map[[2]string]string
}

// Run executes the graph from Start with init as the initial state. It returns
// the final state and a Report even on failure (the state is then however far
// the run got). A node that called Interrupt makes Run return a *Interrupted[S]
// error; pass its Checkpoint to Resume.
func (r *Runnable[S]) Run(ctx context.Context, init S) (S, *Report, error) {
	rec := &recorder{began: time.Now(), runID: newRunID()}
	rec.ck = r.writer(rec.runID, 0)
	ctx = context.WithValue(ctx, runIDKey{}, rec.runID)
	s := init
	info := r.runInfo(rec, &s, false)
	if ro, ok := r.cfg.obs.(RunObserver); ok {
		ro.RunStart(ctx, info)
	}
	first, err := r.next(Start, &s)
	if err == nil && rec.ck != nil {
		err = r.logStart(ctx, rec, &s, first, 0)
	}
	if err != nil {
		rep := rec.report()
		err = r.finish(ctx, info, rec, s, rep, err)
		return s, rep, err
	}
	r.edge(ctx, Start, first, 0)
	s, rep, err := r.loop(ctx, s, first, 0, nil, "", false, rec)
	err = r.finish(ctx, info, rec, s, rep, err)
	return s, rep, err
}

func (r *Runnable[S]) runInfo(rec *recorder, s *S, resumed bool) RunInfo {
	if _, ok := r.cfg.obs.(RunObserver); !ok {
		return RunInfo{}
	}
	return RunInfo{RunID: rec.runID, Graph: r.name, Topology: r.Topology(), State: snapshot(s), Resumed: resumed}
}

func (r *Runnable[S]) runEnd(ctx context.Context, info RunInfo, rep *Report, err error) {
	if ro, ok := r.cfg.obs.(RunObserver); ok {
		ro.RunEnd(ctx, info, rep, err)
	}
}

func (r *Runnable[S]) edge(ctx context.Context, from, to string, step int) {
	if eo, ok := r.cfg.obs.(EdgeObserver); ok {
		eo.EdgeTaken(ctx, from, to, step)
	}
}

// Resume continues a run paused by Interrupt: it re-executes cp.Node with the
// answer available through Answer, then carries on. The node starts from the
// top, with cp.State as left by the interrupted attempt, so a node that can be
// interrupted should check Answer first and keep any partial work in the
// state. The step budget continues from the checkpoint, and the returned
// Report is cumulative: Steps, TotalCostUSD, Duration and Path include
// everything recorded in cp.Prior.
func (r *Runnable[S]) Resume(ctx context.Context, cp Checkpoint[S], answer string) (S, *Report, error) {
	rec := &recorder{began: time.Now(), runID: cp.RunID}
	if rec.runID == "" {
		rec.runID = newRunID()
	}
	n, ok := r.nodes[cp.Node]
	if !ok || r.branches[cp.Node] {
		return cp.State, rec.report(), fmt.Errorf("graph %q: resume: checkpoint node %q is not a node of this graph", r.name, cp.Node)
	}
	if cp.Step < 0 {
		return cp.State, rec.report(), fmt.Errorf("graph %q: resume: negative step in checkpoint", r.name)
	}
	rec.prior = Prior{Steps: slices.Clone(cp.Prior.Steps), Duration: cp.Prior.Duration}
	ctx = context.WithValue(ctx, runIDKey{}, rec.runID)
	st := cp.State
	if r.cfg.store != nil {
		if err := r.logResume(ctx, rec, &st, cp, answer); err != nil {
			return st, rec.report(), err
		}
	}
	info := r.runInfo(rec, &st, true)
	if ro, ok := r.cfg.obs.(RunObserver); ok {
		ro.RunStart(ctx, info)
	}
	st, rep, err := r.loop(ctx, st, n.name, cp.Step, slices.Clone(cp.Path), answer, true, rec)
	err = r.finish(ctx, info, rec, st, rep, err)
	return st, rep, err
}

func (r *Runnable[S]) loop(ctx context.Context, s S, cur string, step int, path []string, answer string, hasAnswer bool, rec *recorder) (S, *Report, error) {
	rec.path = path
	rec.seedVisits(path)
	for cur != End {
		if err := ctx.Err(); err != nil {
			return s, rec.report(), fmt.Errorf("graph %q: cancelled before node %q: %w", r.name, cur, err)
		}
		if step >= r.cfg.maxSteps {
			return s, rec.report(), fmt.Errorf("%w (%d) in graph %q before node %q, path %v", ErrMaxSteps, r.cfg.maxSteps, r.name, cur, rec.path)
		}
		n := r.nodes[cur]
		if !hasAnswer {
			if g, ok := r.cfg.obs.(Gate); ok {
				if err := g.Before(ctx, StepInfo{RunID: rec.runID, Graph: r.name, Node: cur, Step: step + 1}); err != nil {
					return s, rec.report(), err
				}
			}
		}
		step++
		rec.path = append(rec.path, cur)

		nctx := ctx
		if hasAnswer {
			nctx = context.WithValue(ctx, answerKey{}, answer)
			hasAnswer = false
			if ro, ok := r.cfg.obs.(ResumeObserver); ok {
				ro.Resumed(ctx, StepInfo{RunID: rec.runID, Graph: r.name, Node: cur, Step: step}, answer)
			}
		}
		if rec.recSession != "" && cur == rec.recNode {
			nctx = context.WithValue(nctx, recoveredKey{}, rec.recSession)
			rec.recSession = ""
		}
		var fn NodeFunc[S] = n.fn
		if n.par != nil {
			fn = r.parallelFn(n, step, rec)
		}
		if rec.ck != nil {
			if err := rec.ck.append(ctx, Record{Kind: KindNodeStart, Node: n.name, Step: step}); err != nil {
				return s, rec.report(), err
			}
		}
		err := r.exec(nctx, n.name, "", step, rec, fn, &s)
		if err != nil {
			var ie *interruptError
			if errors.As(err, &ie) {
				if rec.ck != nil {
					last := rec.last()
					if cerr := rec.ck.append(ctx, Record{Kind: KindInterrupt, Node: n.name, Step: step, Question: ie.Question,
						State: snapshot(&s), CostUSD: last.CostUSD, DurationMS: last.Duration.Milliseconds()}); cerr != nil {
						return s, rec.report(), cerr
					}
				}
				rep := rec.report()
				cp := Checkpoint[S]{RunID: rec.runID, State: s, Node: n.name, Step: step - 1, Path: slices.Clone(rec.path),
					Prior: Prior{Steps: rep.Steps, Duration: rep.Duration}}
				// Step counts executions completed before this one, so the
				// resumed attempt re-takes the same step number.
				return s, rep, &Interrupted[S]{Question: ie.Question, Node: n.name, Checkpoint: cp, Items: ie.Items}
			}
			return s, rec.report(), err
		}
		if cur, err = r.next(n.name, &s); err != nil {
			return s, rec.report(), err
		}
		if rec.ck != nil {
			if err := r.logCommit(ctx, rec, n, step, &s, cur); err != nil {
				return s, rec.report(), err
			}
		}
		r.edge(ctx, n.name, cur, step)
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
	info := StepInfo{RunID: rec.runID, Graph: r.name, Node: name, Step: step, Parent: parent, Attempt: rec.visit(name)}
	sc := &scope{name: name, info: info, obs: r.cfg.obs}
	if parent == "" {
		sc.ck = rec.ck // branches are not individually recoverable: no session records
	}
	ctx = context.WithValue(ctx, scopeKey{}, sc)
	if r.cfg.nodeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.nodeTimeout)
		defer cancel()
	}
	started := time.Now()
	so, _ := r.cfg.obs.(StateObserver)
	var before json.RawMessage
	if so != nil {
		before = snapshot(s)
	}
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
	if so != nil {
		if after := snapshot(s); before != nil && after != nil {
			so.NodeState(ctx, info, before, after)
		}
	}
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
		info.Items = ie.Items
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
			r.edge(ctx, n.name, b, step)
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
