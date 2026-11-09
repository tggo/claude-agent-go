package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Durable runs: with WithCheckpointStore every run appends its events to a
// CheckpointStore, and Recover rebuilds a run from that log after a crash.
//
// The log is the source of truth. A node is committed (state after, next node)
// before the run moves on, so after a crash the run restarts at the node that
// was in flight, never earlier. A node that was in flight is run again from
// its top: nodes with side effects must be idempotent (see IdempotencyKey).
//
// Limits: the state S must survive a JSON round trip (exported fields; Recover
// unmarshals it). A parallel node is committed as a whole with its merged
// state, so its branches are not individually recoverable: a crash inside a
// fan-out re-runs every branch.

// ErrCheckpoint wraps a failure of the CheckpointStore. The run stops: a run
// that cannot log must not go on, or it would not be recoverable.
var ErrCheckpoint = errors.New("graph: checkpoint store failed")

// ErrRunNotFound: Recover or Fork found no records for the run id.
var ErrRunNotFound = errors.New("graph: run not found in the checkpoint store")

// ErrTopologyChanged: the graph differs from the one the run was started on
// (see AllowTopologyChange).
var ErrTopologyChanged = errors.New("graph: the graph changed since the run started")

// DefaultRecoverPrompt is what a recovered agent session is told when neither
// WithRecoverPrompt nor RetryPolicy.ResumePrompt sets one.
const DefaultRecoverPrompt = "The process running this task was interrupted and restarted. Continue where you left off and finish the task."

// WithCheckpointStore makes every Run, Resume and Recover append to store.
// State must be JSON-serializable. Appending fails the run with ErrCheckpoint.
func WithCheckpointStore(store CheckpointStore) CompileOption {
	return func(o *options) { o.store = store }
}

// AllowTopologyChange lets Recover and Fork continue a run whose graph was
// edited since it started. Without it they fail with ErrTopologyChanged. The
// next node must still exist.
func AllowTopologyChange() CompileOption { return func(o *options) { o.allowTopo = true } }

type recoveredKey struct{}

// RecoveredSession returns the agent session id the crashed node had logged
// when the process died, and true, inside the node re-run by Recover. A
// hand-written node can pass it to the runner as Input.Resume. Agent and
// RunAgent do this by themselves. It is false in every other execution.
func RecoveredSession(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(recoveredKey{}).(string)
	return s, ok && s != ""
}

// IdempotencyKey identifies the current logical step: runID:node:step. It is
// the same on the first attempt and on the re-run after a crash (Recover keeps
// the step number), and different for every iteration of a loop. Pass it to
// the external system a node writes to so that a repeated call returns the
// first result instead of acting twice. It is "" outside a node.
func IdempotencyKey(ctx context.Context) string {
	sc, ok := ctx.Value(scopeKey{}).(*scope)
	if !ok {
		return ""
	}
	return RunID(ctx) + ":" + sc.name + ":" + strconv.Itoa(sc.info.Step)
}

// ckWriter appends records for one run, numbering them.
type ckWriter struct {
	store   CheckpointStore
	runID   string
	mu      sync.Mutex
	seq     int
	err     error // first failure, sticky
	started bool
}

func (w *ckWriter) append(ctx context.Context, rec Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.seq++
	rec.Seq, rec.V = w.seq, RecordVersion
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	// A cancelled run must still be able to log what it already did.
	if err := w.store.Append(context.WithoutCancel(ctx), w.runID, rec); err != nil {
		w.err = fmt.Errorf("%w: %s record %d: %w", ErrCheckpoint, rec.Kind, rec.Seq, err)
	}
	return w.err
}

func (w *ckWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// writer returns the log writer of a run, nil without a store. last is the
// seq of the last record already in the log.
func (r *Runnable[S]) writer(runID string, last int) *ckWriter {
	if r.cfg.store == nil {
		return nil
	}
	return &ckWriter{store: r.cfg.store, runID: runID, seq: last}
}

func (r *Runnable[S]) topoHash() string {
	t := r.Topology()
	type node struct {
		ID, Kind, Parent string
		Branches         []string
	}
	type edge struct {
		From, To string
		Cond     bool
	}
	shape := struct {
		Name  string
		Nodes []node
		Edges []edge
	}{Name: t.Name}
	for _, n := range t.Nodes {
		shape.Nodes = append(shape.Nodes, node{n.ID, n.Kind, n.Parent, n.Branches})
	}
	for _, e := range t.Edges {
		if e.Watch {
			continue // presentation only
		}
		shape.Edges = append(shape.Edges, edge{e.From, e.To, e.Conditional})
	}
	b, _ := json.Marshal(shape)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// logStart writes run_start. first is the node the run begins at; step and
// the state are those the segment starts from.
func (r *Runnable[S]) logStart(ctx context.Context, rec *recorder, s *S, first string, step int) error {
	st := snapshot(s)
	if st == nil {
		return fmt.Errorf("%w: the state does not marshal to JSON", ErrCheckpoint)
	}
	topo, _ := json.Marshal(r.Topology())
	if err := rec.ck.append(ctx, Record{Kind: KindRunStart, Graph: r.name, Topology: topo, TopoHash: r.topoHash(),
		State: st, Next: first, Step: step}); err != nil {
		return err
	}
	rec.ck.started = true
	return nil
}

// logCommit writes node_commit and the edge after it. It is called after the
// route was resolved and before the run moves on.
func (r *Runnable[S]) logCommit(ctx context.Context, rec *recorder, n *node[S], step int, s *S, next string) error {
	if err := rec.ck.failure(); err != nil {
		return err // a session record failed earlier
	}
	st := snapshot(s)
	if st == nil {
		return fmt.Errorf("%w: the state after node %q does not marshal to JSON", ErrCheckpoint, n.name)
	}
	last := rec.last()
	if err := rec.ck.append(ctx, Record{Kind: KindNodeCommit, Node: n.name, Step: step, Attempt: rec.peek(n.name),
		State: st, Next: next, CostUSD: last.CostUSD, DurationMS: last.Duration.Milliseconds()}); err != nil {
		return err
	}
	return rec.ck.append(ctx, Record{Kind: KindEdge, From: n.name, Node: next, Step: step})
}

// finish ends a segment: it writes run_end (not for a pause: the interrupt
// record already tells the story) and calls the RunObserver. A failure to
// write run_end fails an otherwise successful run.
func (r *Runnable[S]) finish(ctx context.Context, info RunInfo, rec *recorder, s S, rep *Report, err error) error {
	if ck := rec.ck; ck != nil && ck.started && !errors.Is(err, ErrInterrupt) {
		end := Record{Kind: KindRunEnd, State: snapshot(&s)}
		if err != nil {
			end.Err = err.Error()
		}
		if cerr := ck.append(ctx, end); cerr != nil && err == nil {
			err = cerr
		}
	}
	r.runEnd(ctx, info, rep, err)
	return err
}

// ---- replay ----

type replay struct {
	start    *Record
	state    json.RawMessage
	next     string
	step     int // executions completed
	steps    []Step
	path     []string
	last     int // seq of the last record
	ended    bool
	endErr   string
	pending  *Record // unanswered interrupt
	answer   *Record // resume following pending
	sessNode string
	sessStep int
	sessID   string
}

func replayLog(recs []Record) (*replay, error) {
	p := &replay{}
	for i := range recs {
		r := &recs[i]
		p.last = r.Seq
		if r.Kind != KindRunEnd {
			p.ended = false
		}
		switch r.Kind {
		case KindRunStart:
			if p.start == nil {
				p.start = r
			}
			p.state, p.next, p.step = r.State, r.Next, r.Step
		case KindSession:
			p.sessNode, p.sessStep, p.sessID = r.Node, r.Step, r.SessionID
		case KindNodeCommit:
			p.state, p.next, p.step = r.State, r.Next, r.Step
			p.steps = append(p.steps, Step{Node: r.Node, Started: r.Time.Add(-time.Duration(r.DurationMS) * time.Millisecond),
				Duration: time.Duration(r.DurationMS) * time.Millisecond, CostUSD: r.CostUSD})
			p.path = append(p.path, r.Node)
			p.pending, p.answer, p.sessID = nil, nil, ""
		case KindFork:
			p.state, p.next, p.step = r.State, r.Next, r.Step
			p.pending, p.answer, p.sessID = nil, nil, ""
		case KindInterrupt:
			p.pending, p.answer, p.sessID = r, nil, ""
			p.state = r.State
			p.steps = append(p.steps, Step{Node: r.Node, Started: r.Time.Add(-time.Duration(r.DurationMS) * time.Millisecond),
				Duration: time.Duration(r.DurationMS) * time.Millisecond, CostUSD: r.CostUSD, Paused: true, Question: r.Question})
			p.path = append(p.path, r.Node)
		case KindResume:
			if p.pending != nil {
				p.answer = r
			}
		case KindRunEnd:
			p.ended, p.endErr = true, r.Err
			if r.State != nil && r.Err == "" { // a failed run is retried from its last commit
				p.state = r.State
			}
		}
	}
	if p.start == nil {
		return nil, errors.New("graph: log has no run_start record")
	}
	return p, nil
}

func (p *replay) prior() Prior {
	var d time.Duration
	for _, s := range p.steps {
		d += s.Duration
	}
	return Prior{Steps: slices.Clone(p.steps), Duration: d}
}

func decodeState[S any](raw json.RawMessage) (S, error) {
	var s S
	if len(raw) == 0 {
		return s, errors.New("graph: log record has no state")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("graph: decode logged state: %w", err)
	}
	return s, nil
}

// Recover continues the run runID from its log in the CheckpointStore: it
// rebuilds the state from the last node_commit and carries on from the node
// that was in flight, which runs again from its top (an agent node resumes its
// CLI session if one was logged). The returned Report is cumulative across the
// crash, restored from the log (parallel branch steps, and the cost of the
// crashed attempt itself, are not in it).
//
// What the log says decides what happens:
//   - finished (run_end without error): the final state, no work;
//   - paused on an Interrupt: the same *Interrupted[S] Run returned, so the
//     caller can Resume it with the answer;
//   - failed (run_end with an error) or crashed: the run is retried from the
//     last commit under the same run id and step numbers;
//   - unknown run: ErrRunNotFound.
//
// A graph that differs from the logged one is refused with ErrTopologyChanged
// unless compiled with AllowTopologyChange.
func (r *Runnable[S]) Recover(ctx context.Context, runID string) (S, *Report, error) {
	var zero S
	if r.cfg.store == nil {
		return zero, nil, errors.New("graph: Recover needs WithCheckpointStore")
	}
	recs, err := r.cfg.store.Load(ctx, runID)
	if err != nil {
		return zero, nil, fmt.Errorf("%w: load %q: %w", ErrCheckpoint, runID, err)
	}
	if len(recs) == 0 {
		return zero, nil, fmt.Errorf("%w: %q", ErrRunNotFound, runID)
	}
	p, err := replayLog(recs)
	if err != nil {
		return zero, nil, fmt.Errorf("graph %q: recover %q: %w", r.name, runID, err)
	}
	if h := p.start.TopoHash; h != r.topoHash() && !r.cfg.allowTopo {
		return zero, nil, fmt.Errorf("%w: run %q started on topology %s, now %s (compile with AllowTopologyChange to continue anyway)",
			ErrTopologyChanged, runID, h, r.topoHash())
	}
	s, err := decodeState[S](p.state)
	if err != nil {
		return zero, nil, fmt.Errorf("graph %q: recover %q: %w", r.name, runID, err)
	}

	rec := &recorder{began: time.Now(), runID: runID, prior: p.prior()}
	rec.ck = r.writer(runID, p.last)
	rec.ck.started = true
	rec.path = slices.Clone(p.path)
	ctx = context.WithValue(ctx, runIDKey{}, runID)
	info := r.runInfo(rec, &s, true)

	// Nothing to do: finished.
	if p.ended && p.endErr == "" {
		rep := rec.report()
		if ro, ok := r.cfg.obs.(RunObserver); ok {
			ro.RunStart(ctx, info)
			ro.RunEnd(ctx, info, rep, nil)
		}
		return s, rep, nil
	}
	// Paused and not answered yet: hand back the question.
	if p.pending != nil && p.answer == nil {
		rep := rec.report()
		cp := Checkpoint[S]{RunID: runID, State: s, Node: p.pending.Node, Step: p.pending.Step - 1,
			Path: slices.Clone(p.path), Prior: Prior{Steps: rep.Steps, Duration: rep.Duration}}
		return s, rep, &Interrupted[S]{Question: p.pending.Question, Node: p.pending.Node, Checkpoint: cp}
	}

	cur, step, answer, hasAnswer := p.next, p.step, "", false
	if p.answer != nil { // answered, then crashed: re-run the node with the answer
		cur, step, answer, hasAnswer = p.pending.Node, p.pending.Step-1, p.answer.Answer, true
	}
	if cur != End {
		if _, ok := r.nodes[cur]; !ok || r.branches[cur] {
			return s, nil, fmt.Errorf("%w: run %q continues at node %q, which this graph does not have", ErrTopologyChanged, runID, cur)
		}
	}
	if p.sessNode == cur && p.sessStep == step+1 && p.sessID != "" {
		rec.recNode, rec.recSession = cur, p.sessID
	}
	if err := rec.ck.append(ctx, Record{Kind: KindRecover, Node: cur, Step: step + 1, Err: p.endErr, SessionID: rec.recSession}); err != nil {
		return s, rec.report(), err
	}
	if ro, ok := r.cfg.obs.(RunObserver); ok {
		ro.RunStart(ctx, info)
	}
	s, rep, err := r.loop(ctx, s, cur, step, slices.Clone(p.path), answer, hasAnswer, rec)
	err = r.finish(ctx, info, rec, s, rep, err)
	return s, rep, err
}

// Fork starts newRunID as a copy of fromRunID's history up to the node_commit
// (or the run_start) with sequence number atSeq, optionally edits the state
// there, and continues from the node that commit routed to. Use the Seq of a
// node_commit record from the log (cag runs <dir> <run> prints them).
// Rollback is a Fork at an earlier commit into a new id: history is never
// rewritten, and the original run stays as it was. The new run's log is the
// copied prefix, then a fork record carrying the edited state.
func (r *Runnable[S]) Fork(ctx context.Context, fromRunID string, atSeq int, newRunID string, edit func(*S)) (S, *Report, error) {
	var zero S
	if r.cfg.store == nil {
		return zero, nil, errors.New("graph: Fork needs WithCheckpointStore")
	}
	if fromRunID == newRunID {
		return zero, nil, errors.New("graph: Fork: the new run id must differ from the source")
	}
	recs, err := r.cfg.store.Load(ctx, fromRunID)
	if err != nil {
		return zero, nil, fmt.Errorf("%w: load %q: %w", ErrCheckpoint, fromRunID, err)
	}
	if len(recs) == 0 {
		return zero, nil, fmt.Errorf("%w: %q", ErrRunNotFound, fromRunID)
	}
	at := -1
	for i, rc := range recs {
		if rc.Seq == atSeq && (rc.Kind == KindNodeCommit || rc.Kind == KindRunStart || rc.Kind == KindFork) {
			at = i
		}
	}
	if at < 0 {
		return zero, nil, fmt.Errorf("graph: Fork: seq %d of run %q is not a node_commit, run_start or fork record", atSeq, fromRunID)
	}
	if existing, err := r.cfg.store.Load(ctx, newRunID); err != nil || len(existing) > 0 {
		if err != nil {
			return zero, nil, fmt.Errorf("%w: load %q: %w", ErrCheckpoint, newRunID, err)
		}
		return zero, nil, fmt.Errorf("graph: Fork: run %q already exists", newRunID)
	}
	prefix := recs[:at+1]
	p, err := replayLog(prefix)
	if err != nil {
		return zero, nil, fmt.Errorf("graph %q: fork %q: %w", r.name, fromRunID, err)
	}
	if h := p.start.TopoHash; h != r.topoHash() && !r.cfg.allowTopo {
		return zero, nil, fmt.Errorf("%w: run %q started on topology %s, now %s", ErrTopologyChanged, fromRunID, h, r.topoHash())
	}
	s, err := decodeState[S](p.state)
	if err != nil {
		return zero, nil, fmt.Errorf("graph %q: fork %q: %w", r.name, fromRunID, err)
	}
	if edit != nil {
		edit(&s)
	}
	st := snapshot(&s)
	if st == nil {
		return zero, nil, fmt.Errorf("%w: the edited state does not marshal to JSON", ErrCheckpoint)
	}
	for _, rc := range prefix {
		if err := r.cfg.store.Append(ctx, newRunID, rc); err != nil {
			return zero, nil, fmt.Errorf("%w: copy to %q: %w", ErrCheckpoint, newRunID, err)
		}
	}
	w := &ckWriter{store: r.cfg.store, runID: newRunID, seq: p.last}
	if err := w.append(ctx, Record{Kind: KindFork, From: fromRunID, FromSeq: atSeq, State: st, Next: p.next, Step: p.step}); err != nil {
		return zero, nil, err
	}
	return r.Recover(ctx, newRunID)
}

// logResume writes the resume record of a Resume call. A checkpoint that
// never went through this store (a log that does not exist) first gets a
// run_start and an interrupt record, so the log stays self-contained.
func (r *Runnable[S]) logResume(ctx context.Context, rec *recorder, st *S, cp Checkpoint[S], answer string) error {
	recs, err := r.cfg.store.Load(ctx, rec.runID)
	if err != nil {
		return fmt.Errorf("%w: load %q: %w", ErrCheckpoint, rec.runID, err)
	}
	last := 0
	if len(recs) > 0 {
		last = recs[len(recs)-1].Seq
	}
	rec.ck = r.writer(rec.runID, last)
	if len(recs) == 0 {
		if err := r.logStart(ctx, rec, st, cp.Node, cp.Step); err != nil {
			return err
		}
		if err := rec.ck.append(ctx, Record{Kind: KindInterrupt, Node: cp.Node, Step: cp.Step + 1,
			Question: "(checkpoint imported)", State: snapshot(st)}); err != nil {
			return err
		}
	}
	rec.ck.started = true
	return rec.ck.append(ctx, Record{Kind: KindResume, Node: cp.Node, Step: cp.Step + 1, Answer: answer})
}

// noteSession logs the agent session id of the node once per distinct id.
func (s *scope) noteSession(ctx context.Context, id string) {
	if id == "" || (s.ck == nil && s.hook == nil) {
		return
	}
	s.mu.Lock()
	if id == s.sess {
		s.mu.Unlock()
		return
	}
	s.sess = id
	s.mu.Unlock()
	if s.hook != nil && s.hook.OnSession != nil {
		s.hook.OnSession(id)
	}
	if s.ck == nil {
		return
	}
	_ = s.ck.append(ctx, Record{Kind: KindSession, Node: s.name, Step: s.info.Step, SessionID: id}) // failure is sticky; the commit reports it
}
