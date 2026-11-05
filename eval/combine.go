package eval

import (
	"path/filepath"
	"sort"

	"github.com/tggo/claude-agent-go/recording"
)

// Combine merges several runs into one, in order — e.g. the per-node runs of
// a graph execution, evaluated as a whole. Tool calls are concatenated and
// metrics summed (durations too: nodes run sequentially). Response is the
// last non-empty response; SessionID is the last non-empty one.
func Combine(runs ...Run) Run {
	var out Run
	for _, r := range runs {
		out.ToolCalls = append(out.ToolCalls, r.ToolCalls...)
		if r.Response != "" {
			out.Response = r.Response
		}
		if r.SessionID != "" {
			out.SessionID = r.SessionID
		}
		m := &out.Metrics
		m.ToolCalls += r.Metrics.ToolCalls
		m.Inferences += r.Metrics.Inferences
		m.Turns += r.Metrics.Turns
		m.InputTokens += r.Metrics.InputTokens
		m.OutputTokens += r.Metrics.OutputTokens
		m.CacheReadTokens += r.Metrics.CacheReadTokens
		m.CostUSD += r.Metrics.CostUSD
		m.Duration += r.Metrics.Duration
		m.ModelTime += r.Metrics.ModelTime
		m.ToolTime += r.Metrics.ToolTime
	}
	return out
}

// FromRecordingDir reads every recording in dir (files that are not
// recordings are skipped), orders them by header start time, and Combines
// them. It is how a graph run recorded with recording.Dir is turned into one
// evaluable Run.
func FromRecordingDir(dir string) (Run, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return Run{}, err
	}
	var recs []*recording.Recording
	for _, f := range files {
		rec, err := recording.ReadFile(f)
		if err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		return recs[i].Header.StartedAt.Before(recs[j].Header.StartedAt)
	})
	runs := make([]Run, len(recs))
	for i, rec := range recs {
		runs[i] = FromRecording(rec)
	}
	return Combine(runs...), nil
}
