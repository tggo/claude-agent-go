// Package graph wires Claude Code runs and plain Go steps into an explicit
// execution graph: sequences, conditional branches, loops, parallel fan-out and
// human-in-the-loop pauses.
//
// Control flow is always explicit. Every node has exactly one outgoing route,
// either a static edge (AddEdge) or a conditional edge (AddConditionalEdge)
// whose route function must return one of the declared targets. Compile checks
// the wiring up front and Run re-checks every routing decision, so a flow
// never "falls through" or stops implicitly: it ends only when a route reaches
// End, a node fails, the context is cancelled, or the step budget is exhausted.
//
// State is a caller-defined type S that flows through the nodes by pointer.
// Nodes mutate it in place. Parallel branches each get their own copy (see
// WithClone) and a merge function folds the copies back.
//
//	g := graph.New[State]("ship").
//		AddNode("plan", plan).
//		AddNode("code", code).
//		AddEdge(graph.Start, "plan").
//		AddEdge("plan", "code").
//		AddEdge("code", graph.End)
//	run, err := g.Compile()
//	final, report, err := run.Run(ctx, State{})
package graph

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/tggo/claude-agent-go/runner"
)

// Reserved node names for the entry and exit pseudo-nodes.
const (
	// Start is the entry pseudo-node. Give it exactly one outgoing edge.
	Start = "__start__"
	// End is the exit pseudo-node. Routing to it finishes the run.
	End = "__end__"
)

// NodeFunc is one step of a graph. It reads and mutates *s. Returning an error
// aborts the run (or, for Interrupt, pauses it).
type NodeFunc[S any] func(ctx context.Context, s *S) error

// RouteFunc picks the next node from the state: a node name or End. The name
// must be one of the targets declared with AddConditionalEdge.
type RouteFunc[S any] func(s *S) string

// MergeFunc folds the states produced by parallel branches into dst. results
// is in the order the branches were declared. dst is the state the parallel
// node started from (not a branch copy).
type MergeFunc[S any] func(dst *S, results []S) error

type node[S any] struct {
	name   string
	fn     NodeFunc[S]
	par    *parallelSpec[S] // non-nil for AddParallel nodes
	agent  bool             // tagged by AddAgentNode / MarkAgent (topology only)
	runner *runner.Runner   // set by AgentRunner (topology only)
	group  *groupDef        // set by Compile from Group (presentation only)
}

type parallelSpec[S any] struct {
	branches []string
	merge    MergeFunc[S]
}

type condEdge[S any] struct {
	route   RouteFunc[S]
	targets []string
}

// Graph is a builder. It is not safe for concurrent mutation; Compile it and
// share the resulting Runnable instead.
type Graph[S any] struct {
	name    string
	nodes   map[string]*node[S]
	order   []string
	static  map[string]string
	cond    map[string]*condEdge[S]
	clone   func(S) S
	labels  map[[2]string]string
	agents  map[string]bool
	runners map[string]*runner.Runner
	watches [][2]string
	groups  []*groupDef
	errs    []error
}

// New returns an empty graph. The name appears in errors and observer events.
func New[S any](name string) *Graph[S] {
	return &Graph[S]{
		name:    name,
		nodes:   map[string]*node[S]{},
		static:  map[string]string{},
		cond:    map[string]*condEdge[S]{},
		labels:  map[[2]string]string{},
		agents:  map[string]bool{},
		runners: map[string]*runner.Runner{},
	}
}

func (g *Graph[S]) fail(format string, a ...any) {
	g.errs = append(g.errs, fmt.Errorf(format, a...))
}

func (g *Graph[S]) addNode(name string, n *node[S]) {
	switch {
	case name == "":
		g.fail("node with empty name")
	case name == Start || name == End:
		g.fail("node %q: name is reserved", name)
	default:
		if _, dup := g.nodes[name]; dup {
			g.fail("node %q: duplicate name", name)
			return
		}
		g.nodes[name] = n
		g.order = append(g.order, name)
	}
}

// AddNode registers a step under a unique name.
func (g *Graph[S]) AddNode(name string, fn NodeFunc[S]) *Graph[S] {
	if fn == nil {
		g.fail("node %q: nil function", name)
	}
	g.addNode(name, &node[S]{name: name, fn: fn})
	return g
}

// AddAgentNode is AddNode for a node that runs a Claude Code agent (for
// example one built with Agent). It behaves identically; the tag only shows
// up in Topology, so viewers can draw agent nodes differently.
func (g *Graph[S]) AddAgentNode(name string, fn NodeFunc[S]) *Graph[S] {
	g.agents[name] = true
	return g.AddNode(name, fn)
}

// MarkAgent tags already registered nodes as agent nodes (see AddAgentNode).
// Unknown names are ignored.
func (g *Graph[S]) MarkAgent(names ...string) *Graph[S] {
	for _, n := range names {
		g.agents[n] = true
	}
	return g
}

// Watch declares that node observer watches the given target nodes (a budget
// guard, a reviewer, an inspector). It changes nothing about execution: the
// pairs show up in Topology as edges with Watch set, which viewers draw as a
// dashed side link instead of a step of the flow. Watch edges do not count
// towards the durable topology hash. Unknown names are ignored by Topology.
func (g *Graph[S]) Watch(observer string, targets ...string) *Graph[S] {
	for _, t := range targets {
		g.watches = append(g.watches, [2]string{observer, t})
	}
	return g
}

// AgentRunner tags node name as an agent node and records the Runner it uses,
// so Topology can show the runner's effective defaults (model, tools, limits)
// before the node has run. It changes nothing about execution.
func (g *Graph[S]) AgentRunner(name string, r *runner.Runner) *Graph[S] {
	g.agents[name] = true
	g.runners[name] = r
	return g
}

// LabelEdge attaches a human-readable label to the edge from -> to, shown by
// Topology consumers (for a conditional edge, say what the route means, e.g.
// "approved"). It does not create the edge.
func (g *Graph[S]) LabelEdge(from, to, label string) *Graph[S] {
	g.labels[[2]string{from, to}] = label
	return g
}

// AddEdge adds the static edge from -> to. A node has at most one outgoing
// route: a second AddEdge, or mixing with AddConditionalEdge, is a compile
// error. For fan-out use AddParallel.
func (g *Graph[S]) AddEdge(from, to string) *Graph[S] {
	if _, dup := g.static[from]; dup {
		g.fail("node %q: more than one static edge (use AddParallel for fan-out)", from)
		return g
	}
	g.static[from] = to
	return g
}

// AddConditionalEdge routes from `from` by calling route after the node runs.
// targets lists every allowed destination (End is allowed); Compile validates
// them and Run rejects any other return value with ErrInvalidRoute.
func (g *Graph[S]) AddConditionalEdge(from string, route RouteFunc[S], targets ...string) *Graph[S] {
	if _, dup := g.cond[from]; dup {
		g.fail("node %q: more than one conditional edge", from)
		return g
	}
	g.cond[from] = &condEdge[S]{route: route, targets: slices.Clone(targets)}
	return g
}

// WithClone sets how a parallel branch's private state copy is made. The
// default is a plain value copy, which shares any maps, slices or pointers in
// S; supply a deep copy when branches mutate those.
func (g *Graph[S]) WithClone(fn func(S) S) *Graph[S] {
	g.clone = fn
	return g
}

// AddParallel adds a node that runs the named branch nodes concurrently, each
// on its own copy of the state, then calls merge. The branch nodes are
// registered with AddNode but must have no edges of their own and cannot be
// routed to directly. The parallel node itself needs an outgoing route like
// any other node.
//
// The default state copy is a shallow value copy: if S holds maps, slices or
// pointers that branches mutate, WithClone is REQUIRED, otherwise branches
// share and race on that data.
//
// If any branch fails, the others are cancelled and the first error is
// returned; merge is not called. A branch (or merge) that calls Interrupt is
// rejected with ErrInterruptInParallel: pausing a fan-out would mean
// checkpointing partially finished branches, so put the question in a node
// before or after the parallel node instead.
func (g *Graph[S]) AddParallel(name string, branches []string, merge MergeFunc[S]) *Graph[S] {
	if merge == nil {
		g.fail("parallel node %q: nil merge function", name)
	}
	g.addNode(name, &node[S]{name: name, par: &parallelSpec[S]{branches: slices.Clone(branches), merge: merge}})
	return g
}

// ErrInvalidGraph is wrapped by every Compile error (errors.Is).
var ErrInvalidGraph = errors.New("graph: invalid graph")

// Compile validates the wiring and returns a Runnable. All problems are
// reported together (errors.Join); each names the offending node.
func (g *Graph[S]) Compile(opts ...CompileOption) (*Runnable[S], error) {
	cfg := options{maxSteps: 100}
	for _, o := range opts {
		o(&cfg)
	}
	errs := slices.Clone(g.errs)
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if cfg.badMaxSteps {
		bad("WithMaxSteps: must be positive")
	}
	if cfg.nodeTimeout < 0 {
		bad("WithNodeTimeout: must not be negative")
	}

	errs = append(errs, g.bindGroups()...)

	exists := func(n string) bool { _, ok := g.nodes[n]; return ok }
	branchSet := map[string]bool{}
	for _, name := range g.order {
		if p := g.nodes[name].par; p != nil {
			if len(p.branches) == 0 {
				bad("parallel node %q: no branches", name)
			}
			seen := map[string]bool{}
			for _, b := range p.branches {
				switch {
				case !exists(b):
					bad("parallel node %q: unknown branch %q", name, b)
				case g.nodes[b].par != nil:
					bad("parallel node %q: branch %q is itself a parallel node (nesting is not supported)", name, b)
				case seen[b]:
					bad("parallel node %q: duplicate branch %q", name, b)
				default:
					branchSet[b] = true
				}
				seen[b] = true
			}
		}
	}

	// Edge endpoints.
	checkFrom := func(from string) bool {
		if from == Start || exists(from) {
			return true
		}
		if from == End {
			bad("edge from %q: End has no outgoing edges", from)
		} else {
			bad("edge from unknown node %q", from)
		}
		return false
	}
	checkTo := func(from, to string) {
		switch {
		case to == End || exists(to):
			if branchSet[to] {
				bad("edge %q -> %q: %q is a parallel branch and cannot be routed to directly", from, to, to)
			}
		case to == Start:
			bad("edge %q -> %q: cannot route to Start", from, to)
		default:
			bad("edge %q -> %q: unknown node %q", from, to, to)
		}
	}
	for from, to := range g.static {
		if checkFrom(from) {
			checkTo(from, to)
		}
	}
	for from, c := range g.cond {
		if !checkFrom(from) {
			continue
		}
		if c.route == nil {
			bad("node %q: nil route function", from)
		}
		if len(c.targets) == 0 {
			bad("node %q: conditional edge declares no targets", from)
		}
		for _, to := range c.targets {
			checkTo(from, to)
		}
		if _, both := g.static[from]; both {
			bad("node %q: has both a static and a conditional edge", from)
		}
	}

	// Every node has a route; branches have none.
	_, hasStart := g.static[Start]
	_, hasStartCond := g.cond[Start]
	if !hasStart && !hasStartCond {
		bad("Start has no outgoing edge")
	}
	for _, name := range g.order {
		_, s := g.static[name]
		_, c := g.cond[name]
		if branchSet[name] {
			if s || c {
				bad("node %q: is a parallel branch and must not have outgoing edges", name)
			}
			continue
		}
		if !s && !c {
			bad("node %q: no outgoing edge", name)
		}
	}

	// Reachability from Start (only meaningful once edges are sane).
	if len(errs) == 0 {
		reach := map[string]bool{}
		var visit func(string)
		visit = func(n string) {
			if reach[n] {
				return
			}
			reach[n] = true
			if t, ok := g.static[n]; ok {
				visit(t)
			}
			if c, ok := g.cond[n]; ok {
				for _, t := range c.targets {
					visit(t)
				}
			}
			if nd := g.nodes[n]; nd != nil && nd.par != nil {
				for _, b := range nd.par.branches {
					visit(b)
				}
			}
		}
		visit(Start)
		for _, name := range g.order {
			if !reach[name] {
				bad("node %q: unreachable from Start", name)
			}
		}
		if !reach[End] {
			bad("End is not reachable from Start")
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidGraph, g.name, errors.Join(errs...))
	}

	r := &Runnable[S]{
		name:     g.name,
		nodes:    make(map[string]*node[S], len(g.nodes)),
		order:    slices.Clone(g.order),
		static:   make(map[string]string, len(g.static)),
		cond:     make(map[string]*condEdge[S], len(g.cond)),
		branches: branchSet,
		cfg:      cfg,
		clone:    g.clone,
		labels:   maps.Clone(g.labels),
		watches:  slices.Clone(g.watches),
	}
	for k, v := range g.nodes {
		v.agent = v.agent || g.agents[k]
		v.runner = g.runners[k]
		r.nodes[k] = v
	}
	for k, v := range g.static {
		r.static[k] = v
	}
	for k, v := range g.cond {
		r.cond[k] = v
	}
	return r, nil
}

// CompileOption configures Compile.
type CompileOption func(*options)

type options struct {
	maxSteps    int
	badMaxSteps bool
	obs         Observer
	nodeTimeout time.Duration
	store       CheckpointStore
	allowTopo   bool
}

// WithMaxSteps bounds the number of node executions in one run (default 100).
// Exceeding it fails the run with ErrMaxSteps. Resumed runs continue counting
// from the checkpoint.
func WithMaxSteps(n int) CompileOption {
	return func(o *options) {
		if n <= 0 {
			o.badMaxSteps = true
			return
		}
		o.maxSteps = n
	}
}

// WithObserver attaches an Observer. For parallel branches its methods are
// called from several goroutines, so it must be goroutine-safe.
func WithObserver(obs Observer) CompileOption { return func(o *options) { o.obs = obs } }

// WithNodeTimeout bounds every single node execution (each parallel branch
// separately). Zero, the default, means no per-node limit.
func WithNodeTimeout(d time.Duration) CompileOption { return func(o *options) { o.nodeTimeout = d } }
