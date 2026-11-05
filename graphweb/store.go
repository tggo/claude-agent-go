package graphweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tggo/claude-agent-go/graph"
)

// NewReplayStore serves one run of a graph.CheckpointStore (the durable log
// written by graph.WithCheckpointStore, for example a graph.FileStore dir)
// read-only, in the same UI as a graphweb journal. An empty runID picks the
// most recently updated run.
//
// The durable log has no agent stream events, so node bodies show their state
// diffs, costs and routing but not the model's messages. A run that crashed
// shows its last node as started and never ended.
func NewReplayStore(ctx context.Context, store graph.CheckpointStore, runID string) (*Server, error) {
	if runID == "" {
		runs, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return nil, errors.New("graphweb: the checkpoint store has no runs")
		}
		runID = runs[0].RunID
	}
	recs, err := store.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("graphweb: run %q not found in the checkpoint store", runID)
	}
	evs, err := EventsFromRecords(runID, recs)
	if err != nil {
		return nil, err
	}
	s := newServer("", nil)
	s.readOnly = true
	for i, ev := range evs {
		ev.Seq = int64(i + 1)
		line, _ := json.Marshal(ev)
		s.events = append(s.events, ev)
		s.raw = append(s.raw, line)
	}
	s.latest = runID
	return s, nil
}

// EventsFromRecords translates a durable run log into journal events: what
// graphweb would have recorded had it watched the run live, minus agent
// stream events. The first record must be a run_start.
func EventsFromRecords(runID string, recs []graph.Record) ([]Event, error) {
	if len(recs) == 0 || recs[0].Kind != graph.KindRunStart {
		return nil, errors.New("graphweb: the log does not start with a run_start record")
	}
	var (
		evs   []Event
		topo  graph.Topology
		back  = map[[2]string]bool{}
		iters = map[[2]string]int{}
		prev  json.RawMessage
		cost  float64
		path  []string
	)
	edge := func(r graph.Record, from, to string, step int) {
		k := [2]string{from, to}
		iters[k]++
		evs = append(evs, Event{V: SchemaVersion, Run: runID, TS: r.Time, Type: TypeEdge, From: from, To: to, Step: step, Iter: iters[k], Back: back[k]})
	}
	for _, r := range recs {
		ev := Event{V: SchemaVersion, Run: runID, TS: r.Time}
		switch r.Kind {
		case graph.KindRunStart:
			if len(r.Topology) > 0 {
				_ = json.Unmarshal(r.Topology, &topo)
			}
			for _, e := range topo.Edges {
				if e.Back {
					back[[2]string{e.From, e.To}] = true
				}
			}
			ev.Type, ev.State = TypeGraphStart, r.State
			if len(r.Topology) > 0 {
				ev.Topology = &topo
			}
			prev = r.State
			evs = append(evs, ev)
			edge(r, graph.Start, r.Next, 0)
		case graph.KindNodeStart:
			ev.Type, ev.Node, ev.Step = TypeNodeStart, r.Node, r.Step
			evs = append(evs, ev)
		case graph.KindNodeCommit:
			cost += r.CostUSD
			path = append(path, r.Node)
			ev.Type, ev.Node, ev.Step, ev.Attempt = TypeNodeEnd, r.Node, r.Step, r.Attempt
			ev.DurationMS, ev.CostUSD, ev.State, ev.Diff = r.DurationMS, r.CostUSD, r.State, diffState(prev, r.State)
			prev = r.State
			evs = append(evs, ev)
			edge(r, r.Node, r.Next, r.Step)
		case graph.KindInterrupt:
			cost += r.CostUSD
			path = append(path, r.Node)
			ev.Type, ev.Node, ev.Step, ev.Paused = TypeNodeEnd, r.Node, r.Step, true
			ev.DurationMS, ev.CostUSD, ev.State, ev.Diff = r.DurationMS, r.CostUSD, r.State, diffState(prev, r.State)
			prev = r.State
			evs = append(evs, ev)
			evs = append(evs, Event{V: SchemaVersion, Run: runID, TS: r.Time, Type: TypeInterrupt, Node: r.Node, Step: r.Step, Question: r.Question})
		case graph.KindResume:
			ev.Type, ev.Node, ev.Step, ev.Answer = TypeResume, r.Node, r.Step, r.Answer
			evs = append(evs, ev)
		case graph.KindFork:
			prev = r.State
		case graph.KindRunEnd:
			ev.Type, ev.Status, ev.TotalCostUSD, ev.Path = TypeGraphEnd, "ok", cost, path
			if r.Err != "" {
				ev.Status, ev.Err = "error", r.Err
			}
			evs = append(evs, ev)
		}
	}
	return evs, nil
}
