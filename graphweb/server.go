// Package graphweb is a local web UI to watch, debug and replay graph runs.
//
// A *Server is a graph.Observer (plus every optional observer extension): hand
// it to graph.WithObserver and every node, edge, state change and agent stream
// event of the run becomes an Event in an in-memory journal, optionally
// mirrored to a JSONL file. The journal is served over SSE to a single-page UI
// (no build step) that draws the graph, animates data along the edges, lets
// you set breakpoints, answer interrupts and scrub back through the run.
//
//	srv, _ := graphweb.New(graphweb.WithJournal("runs/journal.jsonl"))
//	url, _ := srv.Start(ctx, "127.0.0.1:8799")
//	run, _ := g.Compile(graph.WithObserver(srv))
//	final, rep, err := graphweb.RunInteractive(ctx, run, init, srv)
//
// A node can publish files (rendered images, a script, a JSON report) with
// graph.EmitArtifact; the UI shows images as thumbnails on the node and in a
// run-wide gallery, other files as links:
//
//	func render(ctx context.Context, s *State) error {
//	    path := renderPanel(s) // writes /abs/path/p03.png
//	    graph.EmitArtifact(ctx, graph.Artifact{Name: "p03", Path: path, Caption: "panel 3"})
//	    return nil
//	}
//
// A node can also pause for a human review of files: return
// graph.InterruptReview(question, items) and the UI shows a review page (every
// item, a comment box under each, "Approve all" / "Send comments"); the answer
// reaches the node as graph.ReviewAnswer(ctx).
//
// Only paths recorded in artifact events are ever served (GET
// /api/artifact/{seq}); they are read from disk at request time, so the files
// must outlive the run, also for NewReplay.
//
// A journal written this way is also the replay format: NewReplay (or
// `cag web --replay journal.jsonl`) serves the same UI read-only.
//
// Security: the server binds loopback by default, rejects requests whose Host
// header is not local (DNS rebinding), and requires a per-server random token
// on every POST (embedded in the served page, so only same-origin script has
// it).
package graphweb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/graph"
)

// SchemaVersion is the value of Event.V.
const SchemaVersion = 1

// Event types.
const (
	TypeGraphStart    = "graph_start"
	TypeNodeStart     = "node_start"
	TypeNodeEnd       = "node_end"
	TypeEdge          = "edge"
	TypeAgentEvent    = "agent_event"
	TypeAgentInput    = "agent_input"
	TypeInterrupt     = "interrupt"
	TypeResume        = "resume"
	TypeBreakpointHit = "breakpoint_hit"
	TypeArtifact      = "artifact"
	TypeGraphEnd      = "graph_end"
	TypeProgress      = "progress"
)

// maxDiffValue is the size beyond which a diff value is truncated.
const maxDiffValue = 4096

// Change is one changed top-level key of the state.
type Change struct {
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// Event is one journal entry. Only the fields that belong to Type are set.
type Event struct {
	V    int       `json:"v"`
	Run  string    `json:"run,omitempty"`
	Seq  int64     `json:"seq"`
	TS   time.Time `json:"ts"`
	Type string    `json:"type"`

	Topology *graph.Topology `json:"topology,omitempty"` // graph_start
	State    json.RawMessage `json:"state,omitempty"`    // graph_start, node_end (after)

	Node    string `json:"node,omitempty"`
	Step    int    `json:"step,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Attempt int    `json:"attempt,omitempty"`

	Diff       map[string]Change `json:"diff,omitempty"` // node_end
	DurationMS int64             `json:"duration_ms,omitempty"`
	CostUSD    float64           `json:"cost_usd,omitempty"`
	Err        string            `json:"err,omitempty"`
	Paused     bool              `json:"paused,omitempty"`

	From string `json:"from,omitempty"` // edge
	To   string `json:"to,omitempty"`
	Iter int    `json:"iter,omitempty"` // how many times this edge was taken
	Back bool   `json:"back,omitempty"` // loop back-edge

	Stream json.RawMessage `json:"stream,omitempty"` // agent_event
	Input  json.RawMessage `json:"input,omitempty"`  // agent_input: graph.AgentInfo (effective, secret-free)

	Artifact *graph.Artifact `json:"artifact,omitempty"` // artifact (served at /api/artifact/{seq})
	Progress *graph.Progress `json:"progress,omitempty"` // progress

	Question string             `json:"question,omitempty"` // interrupt
	Items    []graph.ReviewItem `json:"items,omitempty"`    // interrupt: files to review (served at /api/review/{seq}/{n})
	Answer   string             `json:"answer,omitempty"`   // resume

	Status       string   `json:"status,omitempty"` // graph_end: ok | error | cancelled | max_steps
	TotalCostUSD float64  `json:"total_cost_usd,omitempty"`
	Path         []string `json:"path,omitempty"`
}

type config struct {
	journal      string
	allowedHosts []string
}

// Option configures New.
type Option func(*config)

// WithJournal mirrors every event to a JSONL file (mode 0600). The file must
// not exist yet, so a journal never mixes two sessions.
func WithJournal(path string) Option { return func(c *config) { c.journal = path } }

// WithAllowedHosts accepts extra Host header names (without port), for when
// the server is deliberately exposed beyond loopback.
func WithAllowedHosts(hosts ...string) Option {
	return func(c *config) { c.allowedHosts = append(c.allowedHosts, hosts...) }
}

type stepKey struct {
	node string
	step int
}

type runState struct {
	back  map[[2]string]bool
	edges map[[2]string]int
	state map[stepKey][2]json.RawMessage
	topo  graph.Topology
}

// Server journals graph events and serves the UI. Create with New or
// NewReplay; it is safe for concurrent use.
type Server struct {
	token    string
	readOnly bool
	hosts    map[string]bool

	mu      sync.Mutex
	events  []Event
	raw     [][]byte
	changed chan struct{}
	runs    map[string]*runState
	latest  string
	journal *os.File
	jerr    error

	breakpoints map[string]bool
	pauseNext   bool
	release     chan struct{}

	answers chan string
	open    int
}

// New returns a live server. It fails only when the journal file cannot be
// created.
func New(opts ...Option) (*Server, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return nil, err
	}
	s := newServer(hex.EncodeToString(tok[:]), cfg.allowedHosts)
	if cfg.journal != "" {
		f, err := os.OpenFile(cfg.journal, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("graphweb: journal: %w", err)
		}
		s.journal = f
	}
	return s, nil
}

func newServer(token string, extraHosts []string) *Server {
	s := &Server{
		token:       token,
		hosts:       map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true},
		changed:     make(chan struct{}),
		runs:        map[string]*runState{},
		breakpoints: map[string]bool{},
		release:     make(chan struct{}),
		answers:     make(chan string, 16),
	}
	for _, h := range extraHosts {
		s.hosts[h] = true
	}
	return s
}

// Token is the secret the UI sends with every POST (header X-Graphweb-Token).
func (s *Server) Token() string { return s.token }

// Err reports the first error writing the journal file, if any. Journalling
// failures never fail a run.
func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jerr
}

// Events returns a copy of the journal so far.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Close flushes and closes the journal file. The server keeps serving its
// in-memory journal.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return nil
	}
	err := s.journal.Close()
	s.journal = nil
	return err
}

// emit stamps ev, appends it to the journal and wakes SSE readers.
func (s *Server) emit(ev Event) {
	ev.V = SchemaVersion
	ev.TS = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = int64(len(s.events)) + 1
	line, err := json.Marshal(ev)
	if err != nil {
		// Cannot happen for our own types unless a raw field is invalid JSON.
		ev.Stream, ev.State = nil, nil
		ev.Err = "graphweb: unmarshalable event: " + err.Error()
		line, _ = json.Marshal(ev)
	}
	s.events = append(s.events, ev)
	s.raw = append(s.raw, line)
	if s.journal != nil {
		if _, err := s.journal.Write(append(line, '\n')); err != nil && s.jerr == nil {
			s.jerr = err
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// ---- graph observer ----

// NodeStart implements graph.Observer.
func (s *Server) NodeStart(_ context.Context, i graph.StepInfo) {
	s.emit(Event{Run: i.RunID, Type: TypeNodeStart, Node: i.Node, Step: i.Step, Parent: i.Parent, Attempt: i.Attempt})
}

// NodeEnd implements graph.Observer.
func (s *Server) NodeEnd(_ context.Context, i graph.StepInfo, err error) {
	ev := Event{Run: i.RunID, Type: TypeNodeEnd, Node: i.Node, Step: i.Step, Parent: i.Parent, Attempt: i.Attempt,
		DurationMS: i.Duration.Milliseconds(), CostUSD: i.CostUSD, Paused: i.Paused}
	if err != nil {
		ev.Err = err.Error()
	}
	s.mu.Lock()
	if rs := s.runs[i.RunID]; rs != nil {
		k := stepKey{i.Node, i.Step}
		if st, ok := rs.state[k]; ok {
			delete(rs.state, k)
			ev.State, ev.Diff = st[1], diffState(st[0], st[1])
		}
	}
	s.mu.Unlock()
	s.emit(ev)
	if i.Paused {
		s.markOpen()
		s.emit(Event{Run: i.RunID, Type: TypeInterrupt, Node: i.Node, Step: i.Step, Question: i.Question, Items: i.Items})
	}
}

// RunStart implements graph.RunObserver.
func (s *Server) RunStart(_ context.Context, info graph.RunInfo) {
	s.mu.Lock()
	rs, known := s.runs[info.RunID]
	if !known {
		rs = &runState{topo: info.Topology, back: map[[2]string]bool{}, edges: map[[2]string]int{},
			state: map[stepKey][2]json.RawMessage{}}
		for _, e := range info.Topology.Edges {
			if e.Back {
				rs.back[[2]string{e.From, e.To}] = true
			}
		}
		s.runs[info.RunID] = rs
	}
	s.latest = info.RunID
	s.mu.Unlock()
	if known {
		return // a resumed segment continues the run the UI already shows
	}
	topo := info.Topology
	s.emit(Event{Run: info.RunID, Type: TypeGraphStart, Topology: &topo, State: info.State})
}

// RunEnd implements graph.RunObserver. A paused segment (interrupt) emits
// nothing: the interrupt event already told the UI.
func (s *Server) RunEnd(_ context.Context, info graph.RunInfo, rep *graph.Report, err error) {
	if errors.Is(err, graph.ErrInterrupt) {
		return
	}
	ev := Event{Run: info.RunID, Type: TypeGraphEnd, Status: "ok"}
	switch {
	case err == nil:
	case errors.Is(err, graph.ErrMaxSteps):
		ev.Status, ev.Err = "max_steps", err.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		ev.Status, ev.Err = "cancelled", err.Error()
	default:
		ev.Status, ev.Err = "error", err.Error()
	}
	if rep != nil {
		ev.TotalCostUSD, ev.DurationMS, ev.Path = rep.TotalCostUSD, rep.Duration.Milliseconds(), rep.Path
	}
	s.emit(ev)
}

// EdgeTaken implements graph.EdgeObserver.
func (s *Server) EdgeTaken(ctx context.Context, from, to string, step int) {
	run := graph.RunID(ctx)
	k := [2]string{from, to}
	s.mu.Lock()
	var iter int
	var back bool
	if rs := s.runs[run]; rs != nil {
		rs.edges[k]++
		iter, back = rs.edges[k], rs.back[k]
	}
	s.mu.Unlock()
	s.emit(Event{Run: run, Type: TypeEdge, From: from, To: to, Step: step, Iter: iter, Back: back})
}

// NodeState implements graph.StateObserver.
func (s *Server) NodeState(_ context.Context, i graph.StepInfo, before, after json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs := s.runs[i.RunID]; rs != nil {
		rs.state[stepKey{i.Node, i.Step}] = [2]json.RawMessage{before, after}
	}
}

// Resumed implements graph.ResumeObserver.
func (s *Server) Resumed(_ context.Context, i graph.StepInfo, answer string) {
	s.emit(Event{Run: i.RunID, Type: TypeResume, Node: i.Node, Step: i.Step, Answer: answer})
}

// AgentEvent implements graph.AgentObserver.
func (s *Server) AgentEvent(_ context.Context, i graph.StepInfo, ev claudecli.StreamEvent) {
	raw := json.RawMessage(ev.Raw)
	if len(raw) == 0 || !json.Valid(raw) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		raw = b
	}
	s.emit(Event{Run: i.RunID, Type: TypeAgentEvent, Node: i.Node, Step: i.Step, Parent: i.Parent, Stream: raw})
}

// AgentInput implements graph.AgentInputObserver: it journals what the agent
// run is about to be started with.
func (s *Server) AgentInput(_ context.Context, i graph.StepInfo, in graph.AgentInfo) {
	b, err := json.Marshal(in)
	if err != nil {
		return
	}
	s.emit(Event{Run: i.RunID, Type: TypeAgentInput, Node: i.Node, Step: i.Step, Parent: i.Parent, Input: b})
}

// Artifact implements graph.ArtifactObserver: it journals a file a node
// produced. The file itself is not copied; GET /api/artifact/{seq} streams it
// from disk when the UI asks. An empty Mime is filled in from the extension.
func (s *Server) Artifact(_ context.Context, i graph.StepInfo, a graph.Artifact) {
	if a.Mime == "" {
		a.Mime = guessMime(a.Path)
		if j := strings.IndexByte(a.Mime, ';'); j >= 0 {
			a.Mime = a.Mime[:j]
		}
	}
	s.emit(Event{Run: i.RunID, Type: TypeArtifact, Node: i.Node, Step: i.Step, Parent: i.Parent, Attempt: i.Attempt, Artifact: &a})
}

// Progress implements graph.ProgressObserver.
func (s *Server) Progress(_ context.Context, i graph.StepInfo, p graph.Progress) {
	s.emit(Event{Run: i.RunID, Type: TypeProgress, Node: i.Node, Step: i.Step, Parent: i.Parent, Attempt: i.Attempt, Progress: &p})
}

// Before implements graph.Gate: it blocks while a breakpoint on the node is
// set or a pause/step was requested, until the UI sends continue or step.
func (s *Server) Before(ctx context.Context, i graph.StepInfo) error {
	s.mu.Lock()
	if !s.breakpoints[i.Node] && !s.pauseNext {
		s.mu.Unlock()
		return nil
	}
	s.pauseNext = false
	ch := s.release
	s.mu.Unlock()
	s.emit(Event{Run: i.RunID, Type: TypeBreakpointHit, Node: i.Node, Step: i.Step})
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetBreakpoints replaces the set of nodes to stop before.
func (s *Server) SetBreakpoints(nodes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breakpoints = make(map[string]bool, len(nodes))
	for _, n := range nodes {
		s.breakpoints[n] = true
	}
}

// Breakpoints lists the nodes currently set.
func (s *Server) Breakpoints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.breakpoints))
	for n := range s.breakpoints {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Control applies "pause" (stop before the next node), "step" (run one node,
// then stop again) or "continue" (run until the next breakpoint).
func (s *Server) Control(action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch action {
	case "pause":
		s.pauseNext = true
		return nil
	case "step", "continue":
		s.pauseNext = action == "step"
		close(s.release)
		s.release = make(chan struct{})
		return nil
	}
	return fmt.Errorf("unknown action %q (want pause, step or continue)", action)
}

// ---- human answers ----

func (s *Server) markOpen() {
	s.mu.Lock()
	s.open++
	s.mu.Unlock()
}

// errNoQuestion means an answer arrived with no interrupt open.
var errNoQuestion = errors.New("graphweb: no question is waiting for an answer")

// Answer delivers a human answer to the pending interrupt (what POST
// /api/answer does).
func (s *Server) Answer(a string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == 0 {
		return errNoQuestion
	}
	s.open--
	s.answers <- a
	return nil
}

func (s *Server) waitAnswer(ctx context.Context) (string, error) {
	select {
	case a := <-s.answers:
		return a, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Ask shows question in the UI and blocks until the human answers (or ctx
// ends). Use it from a node that wants input without pausing the graph; for
// graph.Interrupt flows prefer RunInteractive.
func (s *Server) Ask(ctx context.Context, question string) (string, error) {
	run, node := graph.RunID(ctx), graph.NodeName(ctx)
	s.markOpen()
	s.emit(Event{Run: run, Type: TypeInterrupt, Node: node, Question: question})
	a, err := s.waitAnswer(ctx)
	if err != nil {
		return "", err
	}
	s.emit(Event{Run: run, Type: TypeResume, Node: node, Answer: a})
	return a, nil
}

// RunInteractive runs r and, whenever a node interrupts, waits for the answer
// from the UI and resumes, until the run finishes. r should have been
// compiled with graph.WithObserver(srv) so the UI sees the run; the question
// is surfaced even when it was not.
func RunInteractive[S any](ctx context.Context, r *graph.Runnable[S], init S, srv *Server) (S, *graph.Report, error) {
	st, rep, err := r.Run(ctx, init)
	for {
		intr, ok := graph.AsInterrupt[S](err)
		if !ok {
			return st, rep, err
		}
		srv.ensureOpen(intr.Checkpoint.RunID, intr.Node, intr.Question, intr.Items)
		answer, aerr := srv.waitAnswer(ctx)
		if aerr != nil {
			return st, rep, aerr
		}
		st, rep, err = r.Resume(ctx, intr.Checkpoint, answer)
	}
}

// ensureOpen opens the interrupt in the UI if the observer has not already.
func (s *Server) ensureOpen(run, node, question string, items []graph.ReviewItem) {
	s.mu.Lock()
	if s.open > 0 {
		s.mu.Unlock()
		return
	}
	s.open++
	s.mu.Unlock()
	s.emit(Event{Run: run, Type: TypeInterrupt, Node: node, Question: question, Items: items})
}

// ---- helpers ----

// diffState lists the top-level keys that differ between two JSON states.
func diffState(before, after json.RawMessage) map[string]Change {
	var b, a map[string]json.RawMessage
	if json.Unmarshal(before, &b) != nil || json.Unmarshal(after, &a) != nil {
		if bytes.Equal(before, after) {
			return nil
		}
		return map[string]Change{"$": {Before: truncate(before), After: truncate(after)}}
	}
	out := map[string]Change{}
	for k, av := range a {
		if bv, ok := b[k]; !ok {
			out[k] = Change{After: truncate(av)}
		} else if !bytes.Equal(av, bv) {
			out[k] = Change{Before: truncate(bv), After: truncate(av)}
		}
	}
	for k, bv := range b {
		if _, ok := a[k]; !ok {
			out[k] = Change{Before: truncate(bv)}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncate caps one diff value at maxDiffValue bytes. Strings stay strings;
// other JSON becomes a string holding its first bytes.
func truncate(raw json.RawMessage) json.RawMessage {
	if len(raw) <= maxDiffValue {
		return raw
	}
	var str string
	text := ""
	if json.Unmarshal(raw, &str) == nil {
		text = str
	} else {
		text = string(raw)
	}
	if len(text) > maxDiffValue {
		cut := maxDiffValue
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = fmt.Sprintf("%s…[truncated, %d bytes total]", text[:cut], len(text))
	}
	out, _ := json.Marshal(text)
	return out
}
