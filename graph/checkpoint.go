package graph

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// RecordVersion is the value of Record.V. Readers must ignore unknown fields
// and unknown kinds, so the format can grow without a bump.
const RecordVersion = 1

// Record kinds. A run's log is the ordered list of its records; the state of
// the run is a pure function of that list (see Runnable.Recover).
const (
	// KindRunStart opens the log: the initial state, the topology and its
	// hash, and the first node.
	KindRunStart = "run_start"
	// KindNodeStart is written before a top-level node runs.
	KindNodeStart = "node_start"
	// KindSession records the agent session id of a running agent node, so a
	// crashed node can be resumed instead of restarted.
	KindSession = "session"
	// KindNodeCommit is written after a node succeeded, BEFORE the run moves
	// on: the post-node state and the routed next node. It is the recovery
	// point.
	KindNodeCommit = "node_commit"
	// KindEdge is the routing decision after a commit (informational; the
	// commit already carries Next).
	KindEdge = "edge"
	// KindInterrupt: a node asked a human question; State is the state the
	// interrupted attempt left.
	KindInterrupt = "interrupt"
	// KindResume: the human's answer to the last interrupt.
	KindResume = "resume"
	// KindRecover marks a Recover call continuing the run.
	KindRecover = "recover"
	// KindFork is the first record of a forked run after its copied prefix:
	// the (possibly edited) state to continue from.
	KindFork = "fork"
	// KindRunEnd closes the log. Err == "" means the run finished; a non-empty
	// Err is a failed run, which Recover retries from the last commit.
	KindRunEnd = "run_end"
)

// Record is one entry of a run's durable log. Only the fields that belong to
// Kind are set.
type Record struct {
	V    int       `json:"v"`
	Seq  int       `json:"seq"` // 1-based, dense within a run
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`

	Graph    string          `json:"graph,omitempty"`     // run_start
	Topology json.RawMessage `json:"topology,omitempty"`  // run_start: graph.Topology
	TopoHash string          `json:"topo_hash,omitempty"` // run_start

	Node    string `json:"node,omitempty"`
	Step    int    `json:"step,omitempty"`    // execution number of Node (1-based); run_start/fork: steps done
	Attempt int    `json:"attempt,omitempty"` // node_commit: how often Node ran in this run

	// State is the JSON state: initial (run_start), after the node
	// (node_commit), as left by the interrupted attempt (interrupt), edited
	// (fork), final (run_end).
	State json.RawMessage `json:"state,omitempty"`
	// Next is the node routed to (run_start, node_commit, fork).
	Next string `json:"next,omitempty"`

	SessionID  string  `json:"session_id,omitempty"` // session
	CostUSD    float64 `json:"cost_usd,omitempty"`   // node_commit, interrupt
	DurationMS int64   `json:"duration_ms,omitempty"`

	Question string `json:"question,omitempty"` // interrupt
	Answer   string `json:"answer,omitempty"`   // resume
	From     string `json:"from,omitempty"`     // edge: source node; fork: the run forked from
	FromSeq  int    `json:"from_seq,omitempty"` // fork: seq of the commit forked at
	Err      string `json:"err,omitempty"`      // run_end, recover
}

// CheckpointStore is the durable, append-only log of runs. Records are JSON
// bytes, so a store needs no knowledge of the state type. An implementation
// must make Append durable before it returns for the records that matter to
// recovery (FileStore fsyncs run_start, node_commit and the other
// state-bearing kinds), and Load must return records in append order.
//
// Postgres, Redis or S3 stores are a table or a key per run away: Append is an
// INSERT (run_id, seq, rec jsonb), Load is SELECT ... ORDER BY seq, and List
// is a GROUP BY that feeds Summarize. examples/pgstore is a Postgres store
// over database/sql (no driver in the core).
type CheckpointStore interface {
	// Append adds rec to the end of runID's log.
	Append(ctx context.Context, runID string, rec Record) error
	// Load returns runID's records in order, or none (and no error) when the
	// run does not exist.
	Load(ctx context.Context, runID string) ([]Record, error)
	// List summarises every run, most recently updated first.
	List(ctx context.Context) ([]RunSummary, error)
}

// Run statuses reported by RunSummary.
const (
	StatusRunning = "running" // no run_end, last record younger than StaleAfter
	StatusCrashed = "crashed" // no run_end, nothing written for StaleAfter: the process died
	StatusPaused  = "paused"  // waiting for an interrupt answer
	StatusDone    = "done"
	StatusFailed  = "failed" // ended with an error; Recover retries it
)

// StaleAfter is how long a log without a run_end may stay silent before
// Summarize calls it crashed rather than running. It is a heuristic: a node
// that works longer than this without any record (a long non-agent step)
// looks crashed while it still runs.
const StaleAfter = 2 * time.Minute

// RunSummary is one line of CheckpointStore.List.
type RunSummary struct {
	RunID    string    `json:"run_id"`
	Graph    string    `json:"graph,omitempty"`
	Status   string    `json:"status"`
	LastNode string    `json:"last_node,omitempty"` // last committed node
	Next     string    `json:"next,omitempty"`      // where the run continues
	Commits  int       `json:"commits"`
	Records  int       `json:"records"`
	CostUSD  float64   `json:"cost_usd"`
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`
	Question string    `json:"question,omitempty"` // when paused
	Err      string    `json:"err,omitempty"`
}

// Summarize reduces a run's records to a RunSummary; now decides running vs
// crashed (see StaleAfter).
func Summarize(runID string, recs []Record, now time.Time) RunSummary {
	s := RunSummary{RunID: runID, Records: len(recs)}
	if len(recs) == 0 {
		s.Status = StatusCrashed
		return s
	}
	s.Started, s.Updated = recs[0].Time, recs[len(recs)-1].Time
	for _, r := range recs {
		switch r.Kind {
		case KindRunStart:
			s.Graph, s.Next = r.Graph, r.Next
		case KindNodeCommit:
			s.Commits++
			s.LastNode, s.Next = r.Node, r.Next
			s.CostUSD += r.CostUSD
		case KindFork:
			s.Next = r.Next
		case KindInterrupt:
			s.CostUSD += r.CostUSD
		}
	}
	last := recs[len(recs)-1]
	switch {
	case last.Kind == KindRunEnd && last.Err == "":
		s.Status = StatusDone
	case last.Kind == KindRunEnd:
		s.Status, s.Err = StatusFailed, last.Err
	case last.Kind == KindInterrupt:
		s.Status, s.Question = StatusPaused, last.Question
	case now.Sub(last.Time) > StaleAfter:
		s.Status = StatusCrashed
	default:
		s.Status = StatusRunning
	}
	return s
}

// ---- MemStore ----

type memStore struct {
	mu   sync.Mutex
	runs map[string][]Record
}

// MemStore returns an in-memory CheckpointStore, for tests and single-process
// use. Nothing survives the process.
func MemStore() CheckpointStore { return &memStore{runs: map[string][]Record{}} }

func (m *memStore) Append(_ context.Context, runID string, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[runID] = append(m.runs[runID], rec)
	return nil
}

func (m *memStore) Load(_ context.Context, runID string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.runs[runID]), nil
}

func (m *memStore) List(context.Context) ([]RunSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []RunSummary
	for id, recs := range m.runs {
		out = append(out, Summarize(id, recs, now))
	}
	sortSummaries(out)
	return out, nil
}

func sortSummaries(s []RunSummary) {
	slices.SortFunc(s, func(a, b RunSummary) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.RunID, b.RunID)
	})
}

// ---- FileStore ----

const fileExt = ".jsonl"

var validRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type fileStore struct {
	dir string
	mu  sync.Mutex // serializes appends within the process
}

// FileStore returns a CheckpointStore keeping one JSONL file per run in dir
// (created 0700; files 0600). Appends use O_APPEND and fsync every record that
// recovery depends on before returning, so a SIGKILL or power loss after
// Append returns never loses it. A torn last line (a crash mid-write) is
// ignored by Load and cut off by the next Append. Run ids must match
// [A-Za-z0-9][A-Za-z0-9._-]*, since they become file names.
func FileStore(dir string) (CheckpointStore, error) {
	if dir == "" {
		return nil, errors.New("graph: FileStore: empty dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("graph: FileStore: %w", err)
	}
	return &fileStore{dir: dir}, nil
}

func (f *fileStore) path(runID string) (string, error) {
	if !validRunID.MatchString(runID) {
		return "", fmt.Errorf("graph: invalid run id %q", runID)
	}
	return filepath.Join(f.dir, runID+fileExt), nil
}

// durable reports whether kind must be fsynced before Append returns.
func durable(kind string) bool { return kind != KindNodeStart && kind != KindEdge }

func (f *fileStore) Append(_ context.Context, runID string, rec Record) error {
	p, err := f.path(runID)
	if err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("graph: encode record: %w", err)
	}
	line = append(line, '\n')

	f.mu.Lock()
	defer f.mu.Unlock()
	_, statErr := os.Stat(p)
	fresh := errors.Is(statErr, os.ErrNotExist)
	fh, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := cutTornTail(fh); err != nil {
		return err
	}
	if _, err := fh.Write(line); err != nil {
		return err
	}
	if durable(rec.Kind) {
		if err := fh.Sync(); err != nil {
			return err
		}
	}
	if fresh {
		syncDir(f.dir)
	}
	return nil
}

// cutTornTail truncates a trailing partial line (no final newline) left by a
// crash, so the next record does not get glued to it.
func cutTornTail(fh *os.File) error {
	st, err := fh.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := fh.ReadAt(last[:], st.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	keep := int64(0)
	buf := make([]byte, 64<<10)
	for end := st.Size(); end > 0; {
		start := max(end-int64(len(buf)), 0)
		n, err := fh.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			keep = start + int64(i) + 1
			break
		}
		end = start
	}
	// A tail that is a whole record missing only its newline was already
	// visible to readers (and maybe to Recover/Fork): keep it, terminate it.
	tail := make([]byte, st.Size()-keep)
	if _, err := fh.ReadAt(tail, keep); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if json.Valid(tail) {
		_, err := fh.Write([]byte{'\n'}) // O_APPEND: lands at the end
		return err
	}
	return fh.Truncate(keep)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func (f *fileStore) Load(_ context.Context, runID string) ([]Record, error) {
	p, err := f.path(runID)
	if err != nil {
		return nil, err
	}
	return readRecords(p)
}

// readRecords parses a log file. An unparsable last line is a torn write and
// is dropped; an unparsable line anywhere else is corruption and an error.
func readRecords(path string) ([]Record, error) {
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var (
		recs []Record
		rd   = bufio.NewReaderSize(fh, 1<<20)
	)
	for n := 1; ; n++ {
		line, rerr := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var r Record
			if err := json.Unmarshal(t, &r); err != nil || r.Kind == "" {
				if errors.Is(rerr, io.EOF) {
					break // torn tail
				}
				return nil, fmt.Errorf("graph: %s line %d: corrupt record", path, n)
			}
			recs = append(recs, r)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}
	return recs, nil
}

func (f *fileStore) List(ctx context.Context) ([]RunSummary, error) {
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []RunSummary
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), fileExt)
		if !ok || e.IsDir() || !validRunID.MatchString(id) {
			continue
		}
		recs, err := f.Load(ctx, id)
		if err != nil {
			out = append(out, RunSummary{RunID: id, Status: "corrupt", Err: err.Error()})
			continue
		}
		out = append(out, Summarize(id, recs, now))
	}
	sortSummaries(out)
	return out, nil
}
