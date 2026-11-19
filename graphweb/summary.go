package graphweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Run and node statuses in a RunSummary.
const (
	StatusRunning   = "running"   // started, no end yet: still going, or crashed (see LastEvent)
	StatusWaiting   = "waiting"   // an interrupt is open: the run waits for an answer
	StatusPaused    = "paused"    // a node (not the run) ended with a pause request
	StatusDone      = "done"      // finished ok
	StatusFailed    = "failed"    // finished with an error
	StatusCancelled = "cancelled" // the context ended
	StatusMaxSteps  = "max_steps" // the step limit was hit
	StatusUnknown   = "unknown"   // the journal has no run start (empty or cut at the front)
)

// RunSummary is a run in one flat, JSON-friendly struct, for a host that
// lists many runs without loading the UI. Summarize builds it.
type RunSummary struct {
	Run        string        `json:"run,omitempty"`
	Graph      string        `json:"graph"`
	Status     string        `json:"status"`
	Started    time.Time     `json:"started,omitzero"`
	Ended      time.Time     `json:"ended,omitzero"` // zero until the run ends
	LastEvent  time.Time     `json:"last_event,omitzero"`
	DurationMS int64         `json:"duration_ms"` // start to end; start to last event while the run is open
	CostUSD    float64       `json:"cost_usd"`
	Tokens     int64         `json:"tokens,omitempty"` // input + output + cache, from the agents' result events; 0 when unknown
	Error      string        `json:"error,omitempty"`
	Nodes      []NodeSummary `json:"nodes"`
}

// NodeSummary is one node of a RunSummary, in order of first start. A node
// that ran several times (a loop) or was retried adds up: durations and costs
// are sums, Attempts is how often the node ran (the graph's Attempt counter), Status and Error are the last try's.
type NodeSummary struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"` // running, waiting, paused, done or failed
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Tokens     int64   `json:"tokens,omitempty"`
	Attempts   int     `json:"attempts"`
	Error      string  `json:"error,omitempty"`
}

// Summarize reads a journal (a JSONL file written by WithJournal, or a
// directory holding journal.jsonl) and summarizes its last run. It does not
// start a server. A journal of a run that is still going or that crashed is
// not an error: the status is running, or waiting or paused when the run is
// parked, and a half-written last line is ignored. An empty journal gives
// status unknown. A missing file, or a line that is not a graphweb event
// before the last one, is an error.
func Summarize(path string) (RunSummary, error) {
	file, err := JournalFile(path)
	if err != nil {
		return RunSummary{}, err
	}
	evs, err := readJournalLenient(file)
	if err != nil {
		return RunSummary{}, err
	}
	return summarizeEvents(evs), nil
}

// readJournalLenient is readJournal that accepts an empty file and a last
// line cut by a crash.
func readJournalLenient(file string) ([]Event, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var evs []Event
	rd := bufio.NewReaderSize(f, 1<<20)
	for n := 1; ; n++ {
		line, rerr := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var ev Event
			jerr := json.Unmarshal(t, &ev)
			if jerr == nil && ev.Type == "" {
				jerr = errors.New("not a graphweb event (no type)")
			}
			switch {
			case jerr == nil:
				evs = append(evs, ev)
			case errors.Is(rerr, io.EOF) && line[len(line)-1] != '\n':
				// the last line is unfinished: the writer is mid-line or died
			default:
				return nil, fmt.Errorf("graphweb: %s line %d: %w", file, n, jerr)
			}
		}
		if errors.Is(rerr, io.EOF) {
			return evs, nil
		}
		if rerr != nil {
			return nil, rerr
		}
	}
}

func summarizeEvents(all []Event) RunSummary {
	sum := RunSummary{Status: StatusUnknown, Nodes: []NodeSummary{}}
	run := ""
	for _, ev := range all {
		if ev.Type == TypeGraphStart {
			run = ev.Run // the last run of the journal
		}
	}
	if run == "" {
		return sum
	}
	var (
		ended bool
		open  = ""
	)
	// nodes are found by index: the slice grows, so a pointer kept across appends would go stale
	idx := map[string]int{}
	get := func(name string) *NodeSummary {
		i, ok := idx[name]
		if !ok {
			sum.Nodes = append(sum.Nodes, NodeSummary{Name: name})
			i = len(sum.Nodes) - 1
			idx[name] = i
		}
		return &sum.Nodes[i]
	}
	for _, ev := range all {
		if ev.Run != run {
			continue
		}
		if !ev.TS.IsZero() {
			sum.LastEvent = ev.TS
		}
		switch ev.Type {
		case TypeGraphStart:
			sum.Graph = ""
			if ev.Topology != nil {
				sum.Graph = ev.Topology.Name
			}
			if sum.Started.IsZero() {
				sum.Started = ev.TS
			}
			sum.Status = StatusRunning
		case TypeNodeStart:
			n := get(ev.Node)
			n.Status = StatusRunning
			n.Error = ""
			n.Attempts = max(n.Attempts+1, ev.Attempt)
			open = ""
		case TypeNodeEnd:
			n := get(ev.Node)
			n.DurationMS += ev.DurationMS
			n.CostUSD += ev.CostUSD
			n.Attempts = max(n.Attempts, ev.Attempt)
			switch {
			case ev.Err != "":
				n.Status, n.Error = StatusFailed, ev.Err
			case ev.Paused:
				n.Status = StatusPaused
			default:
				n.Status = StatusDone
			}
		case TypeInterrupt:
			open = ev.Node
			get(ev.Node).Status = StatusWaiting
		case TypeResume:
			open = ""
		case TypeAgentEvent:
			if t := resultTokens(ev.Stream); t > 0 {
				get(ev.Node).Tokens += t
			}
		case TypeGraphEnd:
			ended = true
			sum.Ended = ev.TS
			sum.Error = ev.Err
			sum.DurationMS = ev.DurationMS
			sum.CostUSD = ev.TotalCostUSD
			switch ev.Status {
			case "ok":
				sum.Status = StatusDone
			case "error":
				sum.Status = StatusFailed
			case "cancelled":
				sum.Status = StatusCancelled
			case "max_steps":
				sum.Status = StatusMaxSteps
			default:
				sum.Status = StatusUnknown
			}
		}
		sum.Run = run
	}
	var cost float64
	for _, n := range sum.Nodes {
		cost += n.CostUSD
		sum.Tokens += n.Tokens
	}
	if !ended {
		sum.CostUSD = cost
		if open != "" {
			sum.Status = StatusWaiting
		}
	}
	if ended && sum.CostUSD == 0 {
		sum.CostUSD = cost
	}
	if sum.DurationMS == 0 {
		end := sum.Ended
		if end.IsZero() {
			end = sum.LastEvent
		}
		if !sum.Started.IsZero() && end.After(sum.Started) {
			sum.DurationMS = end.Sub(sum.Started).Milliseconds()
		}
	}
	return sum
}

// resultTokens reads the run totals of an agent's final "result" stream event.
func resultTokens(stream json.RawMessage) int64 {
	if len(stream) == 0 || !bytes.Contains(stream, []byte(`"result"`)) {
		return 0
	}
	var raw struct {
		Type  string           `json:"type"`
		Usage map[string]int64 `json:"usage"`
	}
	if json.Unmarshal(stream, &raw) != nil || raw.Type != "result" {
		return 0
	}
	u := raw.Usage
	return u["input_tokens"] + u["output_tokens"] + u["cache_read_input_tokens"] + u["cache_creation_input_tokens"]
}
