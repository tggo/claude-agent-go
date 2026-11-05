// Package temporal runs claude-agent-go graphs as durable Temporal workflows.
//
// Each graph node executes as a Temporal activity; the control loop of
// graph.Runnable.Run becomes a deterministic workflow that walks the graph one
// node at a time, so a worker crash, a deploy or a machine loss never loses a
// run: Temporal replays the workflow history, re-runs only the node that was
// in flight, and an agent node continues its Claude CLI session (--resume)
// instead of starting over.
//
//	r, _ := g.Compile()
//	w := worker.New(c, temporal.TaskQueue(r), worker.Options{})
//	temporal.Register(w, r)                  // workflow + node activity
//	run, _ := temporal.Start(ctx, c, r, init, client.StartWorkflowOptions{ID: "job-1"})
//	final, report, err := temporal.Wait[State](ctx, run)
//
// Nodes are Go closures and cannot travel through Temporal, so every worker
// that serves the graph must register the same compiled graph; the workflow
// only carries the state (JSON) and the route. The state type must therefore
// be JSON-serializable. A node that calls graph.Interrupt pauses the workflow
// until Answer delivers the human's answer as a signal.
//
// Guarantees and limits:
//   - At-least-once nodes. A retried or recovered node runs again from the top;
//     use graph.IdempotencyKey(ctx) (workflowID:node:step) for side effects.
//   - A failed attempt's cost is not in the Result (only completed executions
//     report their cost).
//   - Long loops grow the workflow history (about three events per node);
//     keep graphs within Temporal's history limits or use graph.WithMaxSteps.
package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/graph"
)

// Names of the signal and queries a graph workflow exposes.
const (
	// SignalAnswer carries the human's answer (a string) to a paused node.
	SignalAnswer = "answer"
	// QueryPending returns a Pending: the question the workflow waits on.
	QueryPending = "pending"
	// QueryState returns the current state as JSON (json.RawMessage).
	QueryState = "state"
	// QueryReport returns the Result so far.
	QueryReport = "report"
)

// Error types of the non-retryable application errors the package produces.
const (
	ErrTypeRoute    = "InvalidRoute"
	ErrTypeMaxSteps = "MaxSteps"
	ErrTypeNode     = "NodePanic"
	ErrTypeState    = "State"
)

// WorkflowName is the Temporal workflow type registered for r.
func WorkflowName[S any](r *graph.Runnable[S]) string { return "graph." + r.Topology().Name }

// ActivityName is the Temporal activity type that executes one node of r.
func ActivityName[S any](r *graph.Runnable[S]) string { return "graph." + r.Topology().Name + ".node" }

// TaskQueue is the default task queue of r, used by Start when the options
// name none. Run your worker on it.
func TaskQueue[S any](r *graph.Runnable[S]) string { return "graph-" + r.Topology().Name }

// Input is the workflow input.
type Input struct {
	State json.RawMessage `json:"state"`
}

// NodeInput is the activity input: one node execution.
type NodeInput struct {
	Node string `json:"node"`
	// Step is the 1-based execution number (an interrupted node re-run with
	// its answer keeps its number).
	Step int `json:"step"`
	// Visit counts executions of Node in the run (graph.StepInfo.Attempt).
	Visit int             `json:"visit"`
	State json.RawMessage `json:"state"`
	// Answer is set when the node is re-run after an interrupt.
	Answer *string `json:"answer,omitempty"`
	RunID  string  `json:"run_id"`
}

// NodeOutput is the activity result.
type NodeOutput struct {
	State    json.RawMessage    `json:"state"`
	Next     string             `json:"next,omitempty"`
	Cost     float64            `json:"cost_usd,omitempty"`
	Paused   bool               `json:"paused,omitempty"`
	Question string             `json:"question,omitempty"`
	Items    []graph.ReviewItem `json:"items,omitempty"`
	// SessionID is the last agent session the node used.
	SessionID string     `json:"session_id,omitempty"`
	Step      graph.Step `json:"step"`
}

// Heartbeat is the progress an activity records. A retried attempt reads the
// previous attempt's last one to resume its agent session.
type Heartbeat struct {
	Node    string `json:"node"`
	Session string `json:"session,omitempty"`
	// Events counts the agent stream events seen by the attempt.
	Events int `json:"events"`
}

// Pending describes the question a paused workflow waits on (QueryPending).
type Pending struct {
	Waiting  bool               `json:"waiting"`
	Node     string             `json:"node,omitempty"`
	Step     int                `json:"step,omitempty"`
	Question string             `json:"question,omitempty"`
	Items    []graph.ReviewItem `json:"items,omitempty"`
}

// Result is the workflow result: the final state and a report like
// graph.Report. It is also what QueryReport returns while the run is going.
type Result struct {
	RunID string `json:"run_id"`
	Graph string `json:"graph"`
	// State is the final state as JSON; see Decode.
	State        json.RawMessage `json:"state"`
	Steps        []graph.Step    `json:"steps"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	// Duration is the summed wall-clock time of the node executions.
	Duration time.Duration `json:"duration"`
	// Path is the top-level execution trace; an interrupted node appears once
	// per execution.
	Path []string `json:"path"`
}

// Report converts the result to a graph.Report.
func (r *Result) Report() *graph.Report {
	return &graph.Report{RunID: r.RunID, Steps: r.Steps, TotalCostUSD: r.TotalCostUSD, Duration: r.Duration, Path: r.Path}
}

// Decode unmarshals the final state.
func (r *Result) Decode(into any) error { return json.Unmarshal(r.State, into) }

type config struct {
	timeout      time.Duration
	nodeTimeouts map[string]time.Duration
	hbTimeout    time.Duration
	hbInterval   time.Duration
	retry        *sdktemporal.RetryPolicy
}

// Option configures Register.
type Option func(*config)

// WithActivityTimeout sets the StartToClose timeout of every node attempt
// (default 30 minutes).
func WithActivityTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// WithNodeTimeout overrides the timeout for one node (agent nodes usually
// need longer than plain ones).
func WithNodeTimeout(node string, d time.Duration) Option {
	return func(c *config) { c.nodeTimeouts[node] = d }
}

// WithHeartbeat sets the heartbeat timeout (default 30s: a worker silent for
// that long is declared dead and the node is retried) and how often the
// activity heartbeats (default timeout/3).
func WithHeartbeat(timeout, interval time.Duration) Option {
	return func(c *config) { c.hbTimeout, c.hbInterval = timeout, interval }
}

// WithRetryPolicy replaces the default retry policy (3 attempts, 1s initial
// interval, doubling). The package's non-retryable error types are added.
func WithRetryPolicy(p sdktemporal.RetryPolicy) Option { return func(c *config) { c.retry = &p } }

func newConfig(opts []Option) *config {
	c := &config{timeout: 30 * time.Minute, nodeTimeouts: map[string]time.Duration{}, hbTimeout: 30 * time.Second}
	for _, o := range opts {
		o(c)
	}
	if c.hbInterval <= 0 {
		c.hbInterval = c.hbTimeout / 3
	}
	if c.retry == nil {
		c.retry = &sdktemporal.RetryPolicy{MaximumAttempts: 3, InitialInterval: time.Second, BackoffCoefficient: 2}
	}
	rp := *c.retry
	rp.NonRetryableErrorTypes = append(append([]string(nil), rp.NonRetryableErrorTypes...), ErrTypeRoute, ErrTypeMaxSteps, ErrTypeNode, ErrTypeState)
	c.retry = &rp
	return c
}

func (c *config) nodeTimeout(node string) time.Duration {
	if d, ok := c.nodeTimeouts[node]; ok {
		return d
	}
	return c.timeout
}

type registration[S any] struct {
	r        *graph.Runnable[S]
	cfg      *config
	name     string
	maxSteps int
	actName  string
}

// Register registers the workflow WorkflowName(r) and the activity
// ActivityName(r) on w. Call it on every worker that serves the graph, with
// the same compiled graph.
func Register[S any](w worker.Registry, r *graph.Runnable[S], opts ...Option) {
	topo := r.Topology()
	rg := &registration[S]{r: r, cfg: newConfig(opts), name: topo.Name, maxSteps: topo.MaxSteps, actName: ActivityName(r)}
	w.RegisterWorkflowWithOptions(rg.workflow, workflow.RegisterOptions{Name: WorkflowName(r)})
	w.RegisterActivityWithOptions(rg.node, activity.RegisterOptions{Name: rg.actName})
}

func (rg *registration[S]) workflow(ctx workflow.Context, in Input) (*Result, error) {
	runID := workflow.GetInfo(ctx).WorkflowExecution.ID
	res := &Result{RunID: runID, Graph: rg.name, State: in.State}
	pending := Pending{}
	if err := workflow.SetQueryHandler(ctx, QueryPending, func() (Pending, error) { return pending, nil }); err != nil {
		return nil, err
	}
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (json.RawMessage, error) { return res.State, nil }); err != nil {
		return nil, err
	}
	if err := workflow.SetQueryHandler(ctx, QueryReport, func() (Result, error) { return *res, nil }); err != nil {
		return nil, err
	}
	answers := workflow.GetSignalChannel(ctx, SignalAnswer)

	run := func(node string, step, visit int, answer *string) (NodeOutput, error) {
		ao := workflow.ActivityOptions{
			StartToCloseTimeout: rg.cfg.nodeTimeout(node),
			HeartbeatTimeout:    rg.cfg.hbTimeout,
			RetryPolicy:         rg.cfg.retry,
			WaitForCancellation: true,
		}
		var out NodeOutput
		err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), rg.actName,
			NodeInput{Node: node, Step: step, Visit: visit, State: res.State, Answer: answer, RunID: runID}).Get(ctx, &out)
		return out, err
	}

	first, err := run(graph.Start, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	cur, step := first.Next, 0
	visits := map[string]int{}
	for cur != graph.End {
		if step >= rg.maxSteps {
			return nil, sdktemporal.NewNonRetryableApplicationError(
				fmt.Sprintf("graph %q: max steps (%d) exceeded before node %q, path %v", rg.name, rg.maxSteps, cur, res.Path),
				ErrTypeMaxSteps, graph.ErrMaxSteps)
		}
		step++
		var answer *string
		var out NodeOutput
		for {
			visits[cur]++
			res.Path = append(res.Path, cur)
			if out, err = run(cur, step, visits[cur], answer); err != nil {
				return nil, err
			}
			res.State = out.State
			res.record(out)
			if !out.Paused {
				break
			}
			// The node asked a question: wait for the human, then re-run it
			// (same step number) with the answer.
			pending = Pending{Waiting: true, Node: cur, Step: step, Question: out.Question, Items: out.Items}
			var a string
			answers.Receive(ctx, &a)
			pending = Pending{}
			answer = &a
		}
		cur = out.Next
	}
	return res, nil
}

func (r *Result) record(out NodeOutput) {
	r.Steps = append(r.Steps, out.Step)
	r.TotalCostUSD += out.Step.CostUSD
	r.Duration += out.Step.Duration
}

// node is the activity: one node execution through Runnable.Step, with a
// heartbeat that carries the agent session across attempts.
func (rg *registration[S]) node(ctx context.Context, in NodeInput) (NodeOutput, error) {
	var s S
	if len(in.State) > 0 {
		if err := json.Unmarshal(in.State, &s); err != nil {
			return NodeOutput{}, sdktemporal.NewNonRetryableApplicationError("decode state: "+err.Error(), ErrTypeState, err)
		}
	}
	info := activity.GetInfo(ctx)
	hb := Heartbeat{Node: in.Node}
	opt := graph.StepOptions{RunID: in.RunID, Step: in.Step, Attempt: in.Visit, Answer: in.Answer}
	if info.Attempt > 1 && activity.HasHeartbeatDetails(ctx) {
		var prev Heartbeat
		if err := activity.GetHeartbeatDetails(ctx, &prev); err == nil && prev.Node == in.Node {
			hb.Session, opt.Session = prev.Session, prev.Session
		}
	}

	var mu sync.Mutex
	beat := func() {
		mu.Lock()
		d := hb
		mu.Unlock()
		activity.RecordHeartbeat(ctx, d)
	}
	opt.Hooks = graph.StepHooks{
		OnSession: func(id string) {
			mu.Lock()
			hb.Session = id
			mu.Unlock()
			beat() // a crash right after the session starts must still find it
		},
		OnEvent: func(claudecli.StreamEvent, int) {
			mu.Lock()
			hb.Events++
			mu.Unlock()
		},
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(rg.cfg.hbInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				beat()
			}
		}
	}()
	// No beat before the node starts: the SDK batches heartbeats, and the first
	// one opens a window (0.8 x HeartbeatTimeout) in which later details are
	// only held back. Left free, the session id beat goes out at once.
	res, err := rg.r.Step(ctx, in.Node, s, opt)
	close(stop)
	<-done
	if err != nil {
		return NodeOutput{}, classify(err)
	}
	st, merr := json.Marshal(res.State)
	if merr != nil {
		return NodeOutput{}, sdktemporal.NewNonRetryableApplicationError("encode state: "+merr.Error(), ErrTypeState, merr)
	}
	mu.Lock()
	sess := hb.Session
	mu.Unlock()
	return NodeOutput{State: st, Next: res.Next, Cost: res.Step.CostUSD, Paused: res.Paused, Question: res.Question,
		Items: res.Items, SessionID: sess, Step: res.Step}, nil
}

// classify marks the errors that a retry cannot fix as non-retryable.
func classify(err error) error {
	var pe *graph.PanicError
	switch {
	case errors.Is(err, graph.ErrInvalidRoute):
		return sdktemporal.NewNonRetryableApplicationError(err.Error(), ErrTypeRoute, err)
	case errors.Is(err, graph.ErrInterruptInParallel), errors.As(err, &pe):
		return sdktemporal.NewNonRetryableApplicationError(err.Error(), ErrTypeNode, err)
	}
	return err
}

// StartOptions is client.StartWorkflowOptions; an empty TaskQueue defaults to
// TaskQueue(r).
type StartOptions = client.StartWorkflowOptions

// Start starts a workflow run of r from init and returns its handle. The ID in
// opts becomes graph.RunID and the first part of graph.IdempotencyKey.
func Start[S any](ctx context.Context, c client.Client, r *graph.Runnable[S], init S, opts StartOptions) (client.WorkflowRun, error) {
	st, err := json.Marshal(init)
	if err != nil {
		return nil, fmt.Errorf("temporal: encode initial state: %w", err)
	}
	if opts.TaskQueue == "" {
		opts.TaskQueue = TaskQueue(r)
	}
	return c.ExecuteWorkflow(ctx, opts, WorkflowName(r), Input{State: st})
}

// Wait blocks for the run and decodes the final state.
func Wait[S any](ctx context.Context, run client.WorkflowRun) (S, *Result, error) {
	var (
		zero S
		res  Result
	)
	if err := run.Get(ctx, &res); err != nil {
		return zero, nil, err
	}
	var s S
	if err := res.Decode(&s); err != nil {
		return zero, &res, fmt.Errorf("temporal: decode final state: %w", err)
	}
	return s, &res, nil
}

// Answer delivers a human's answer to the workflow paused on an Interrupt.
func Answer(ctx context.Context, c client.Client, workflowID, answer string) error {
	return c.SignalWorkflow(ctx, workflowID, "", SignalAnswer, answer)
}

// PendingQuestion asks the workflow what it is waiting for.
func PendingQuestion(ctx context.Context, c client.Client, workflowID string) (Pending, error) {
	var p Pending
	v, err := c.QueryWorkflow(ctx, workflowID, "", QueryPending)
	if err != nil {
		return p, err
	}
	return p, v.Get(&p)
}
