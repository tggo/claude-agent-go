package eval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/runner"
)

// Metric names used in Score.Name.
const (
	MetricTrajectory       = "trajectory"
	MetricResponseMatch    = "response_match"
	MetricResponseContains = "response_contains"
	MetricResponseRegex    = "response_regex"
	MetricJudge            = "judge"
	MetricMaxCost          = "max_cost_usd"
	MetricMaxToolCalls     = "max_tool_calls"
	MetricMaxDuration      = "max_duration"
)

// AgentFunc runs the agent under test on one case and returns what it did.
type AgentFunc func(ctx context.Context, c Case) (Run, error)

// Threshold returns a pointer to v, for the optional Criteria thresholds.
func Threshold(v float64) *float64 { return &v }

// Criteria selects which metrics are scored and their pass thresholds. A
// metric is only scored when it is enabled here and the case supplies the
// data for it (an expected trajectory, an expected response, ...).
type Criteria struct {
	// TrajectoryThreshold enables trajectory scoring (1 on match, else 0 per
	// run; the mean over repeats must reach it). nil skips it. Cases without
	// ExpectedTools are skipped.
	TrajectoryThreshold *float64
	// MatchOptions are passed to MatchTrajectory (ArgsSubset, MCPNames).
	MatchOptions []MatchOption

	// ResponseMatch enables ROUGE-L scoring against Case.ExpectedResponse
	// with this minimum F-measure. nil skips it.
	ResponseMatch *float64

	// Judge enables LLM-as-judge scoring for cases that have a Rubric or an
	// ExpectedResponse.
	//
	// Threshold precedence: when JudgeThreshold is non-nil the mean judge
	// score over runs must reach it (0 is expressible and always passes).
	// When nil, the judge's own verdict decides (JudgeResult.Pass, e.g. the
	// ClaudeJudge JudgeThreshold option, default 0.7) and the case passes only
	// if every run was judged Pass.
	Judge          Judge
	JudgeThreshold *float64

	// Budgets, enforced on every run (zero disables): a case fails when any
	// run exceeds one. MaxCostUSD budgets the agent only; judge cost is
	// reported separately in CaseResult.JudgeCostUSD and never counted.
	MaxCostUSD   float64
	MaxToolCalls int
	MaxDuration  time.Duration
}

// Score is one metric's aggregated outcome for a case.
type Score struct {
	Name string
	// Value is the mean over runs.
	Value     float64
	Threshold float64
	Pass      bool
	// ByVerdict marks a score whose pass/fail comes from the judge's own
	// verdict rather than Threshold (which is then unused).
	ByVerdict bool
	// Detail explains the score (judge reason, matched/missing, budget).
	Detail string
}

// CaseResult is the outcome of one case.
type CaseResult struct {
	Case Case
	// Runs holds every repeat; Metrics is their mean.
	Runs    []Run
	Metrics Metrics
	Scores  []Score
	// Err is set when the agent or a judge failed; the case then fails.
	Err  error
	Pass bool
	// JudgeCostUSD is the total judge cost over all runs (not part of Metrics
	// or the MaxCostUSD budget).
	JudgeCostUSD float64
}

// Report is the outcome of Evaluate.
type Report struct {
	SetID, SetName string
	Repeats        int
	Cases          []CaseResult
}

// Passed, Failed and Errored count cases. Errored cases are not in Failed.
func (r *Report) Passed() int {
	n := 0
	for _, c := range r.Cases {
		if c.Pass {
			n++
		}
	}
	return n
}

// Errored counts cases whose agent or judge returned an error.
func (r *Report) Errored() int {
	n := 0
	for _, c := range r.Cases {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// Failed counts cases that ran but missed a threshold.
func (r *Report) Failed() int { return len(r.Cases) - r.Passed() - r.Errored() }

// OK reports whether every case passed.
func (r *Report) OK() bool { return r.Passed() == len(r.Cases) }

// Option configures Evaluate and RunT.
type Option func(*evalConfig)

type evalConfig struct {
	parallel int
	repeats  int
}

// WithParallelism runs up to n cases concurrently (default 1). The AgentFunc
// must then be safe for concurrent use.
func WithParallelism(n int) Option { return func(c *evalConfig) { c.parallel = n } }

// WithRepeats runs each case n times (ADK's num_runs, default 1); each metric
// is the mean over runs and passes when the mean meets its threshold.
func WithRepeats(n int) Option { return func(c *evalConfig) { c.repeats = n } }

func newConfig(opts []Option) evalConfig {
	cfg := evalConfig{parallel: 1, repeats: 1}
	for _, o := range opts {
		o(&cfg)
	}
	cfg.parallel = max(cfg.parallel, 1)
	cfg.repeats = max(cfg.repeats, 1)
	return cfg
}

// Evaluate runs every case of set through agent and scores it against c. It
// returns an error only for an invalid set or a cancelled context (the report
// then holds the cases finished so far); per-case failures are in the report.
func Evaluate(ctx context.Context, set Set, agent AgentFunc, c Criteria, opts ...Option) (*Report, error) {
	if err := set.Validate(); err != nil {
		return nil, err
	}
	cfg := newConfig(opts)
	rep := &Report{SetID: set.ID, SetName: set.Name, Repeats: cfg.repeats, Cases: make([]CaseResult, len(set.Cases))}
	done := make([]bool, len(set.Cases))

	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.parallel)
loop:
	for i := range set.Cases {
		select {
		case <-ctx.Done():
			break loop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rep.Cases[i] = evalCase(ctx, set.Cases[i], agent, c, cfg.repeats)
			done[i] = true
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		finished := rep.Cases[:0]
		for i, cr := range rep.Cases {
			if done[i] {
				finished = append(finished, cr)
			}
		}
		rep.Cases = finished
		return rep, err
	}
	return rep, nil
}

func evalCase(ctx context.Context, cs Case, agent AgentFunc, crit Criteria, repeats int) CaseResult {
	res := CaseResult{Case: cs}
	var perRun [][]Score
	for n := 0; n < repeats; n++ {
		run, err := agent(ctx, cs)
		if err != nil {
			res.Err = fmt.Errorf("run %d/%d: %w", n+1, repeats, err)
			res.Runs = append(res.Runs, run)
			break
		}
		res.Runs = append(res.Runs, run)
		scores, jc, err := scoreRun(ctx, cs, run, crit)
		res.JudgeCostUSD += jc
		if err != nil {
			res.Err = fmt.Errorf("run %d/%d: %w", n+1, repeats, err)
			break
		}
		perRun = append(perRun, scores)
	}
	res.Metrics = meanMetrics(res.Runs)
	if res.Err != nil {
		return res
	}
	res.Scores = aggregate(perRun)
	res.Pass = true
	for _, s := range res.Scores {
		res.Pass = res.Pass && s.Pass
	}
	return res
}

// scoreRun scores one run; every Score has Value in 0..1 and its Threshold.
func scoreRun(ctx context.Context, cs Case, run Run, c Criteria) ([]Score, float64, error) {
	var judgeCost float64
	var out []Score
	add := func(name string, v, th float64, detail string) {
		out = append(out, Score{Name: name, Value: v, Threshold: th, Pass: v >= th, Detail: detail})
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	if c.TrajectoryThreshold != nil && cs.ExpectedTools != nil {
		ok := MatchTrajectory(run.ToolCalls, cs.ExpectedTools, cs.Match, cs.IgnoreArgs, c.MatchOptions...)
		add(MetricTrajectory, b2f(ok), *c.TrajectoryThreshold, "match="+cs.Match.String())
	}
	if c.ResponseMatch != nil && cs.ExpectedResponse != "" {
		v := RougeL(run.Response, cs.ExpectedResponse)
		add(MetricResponseMatch, v, *c.ResponseMatch, "rougeL")
	}
	if len(cs.ResponseContains) > 0 {
		var missing []string
		for _, s := range cs.ResponseContains {
			if !Contains(run.Response, s) {
				missing = append(missing, fmt.Sprintf("%q", s))
			}
		}
		d := "all present"
		if len(missing) > 0 {
			d = fmt.Sprintf("missing %v", missing)
		}
		add(MetricResponseContains, b2f(len(missing) == 0), 1, d)
	}
	if cs.ResponseRegex != "" {
		ok, err := Matches(run.Response, cs.ResponseRegex)
		if err != nil {
			return nil, 0, err
		}
		add(MetricResponseRegex, b2f(ok), 1, "pattern "+cs.ResponseRegex)
	}
	if c.Judge != nil && (cs.Rubric != "" || cs.ExpectedResponse != "") {
		jr, err := c.Judge.Score(ctx, JudgeInput{Prompt: cs.Prompt, Response: run.Response, Reference: cs.ExpectedResponse, Rubric: cs.Rubric})
		if err != nil {
			return nil, 0, fmt.Errorf("judge: %w", err)
		}
		judgeCost = jr.CostUSD
		if c.JudgeThreshold != nil {
			add(MetricJudge, jr.Score, *c.JudgeThreshold, jr.Reason)
		} else {
			out = append(out, Score{Name: MetricJudge, Value: jr.Score, Pass: jr.Pass, ByVerdict: true, Detail: jr.Reason})
		}
	}
	m := run.Metrics
	if c.MaxCostUSD > 0 {
		add(MetricMaxCost, b2f(m.CostUSD <= c.MaxCostUSD), 1, fmt.Sprintf("$%.4f of $%.4f", m.CostUSD, c.MaxCostUSD))
	}
	if c.MaxToolCalls > 0 {
		add(MetricMaxToolCalls, b2f(len(run.ToolCalls) <= c.MaxToolCalls), 1, fmt.Sprintf("%d of %d", len(run.ToolCalls), c.MaxToolCalls))
	}
	if c.MaxDuration > 0 {
		add(MetricMaxDuration, b2f(m.Duration <= c.MaxDuration), 1, fmt.Sprintf("%s of %s", m.Duration.Round(time.Millisecond), c.MaxDuration))
	}
	return out, judgeCost, nil
}

// aggregate averages each metric over runs. Detail comes from the last run.
func aggregate(perRun [][]Score) []Score {
	var order []string
	sum := map[string]Score{}
	count := map[string]int{}
	verdictFail := map[string]bool{}
	for _, scores := range perRun {
		for _, s := range scores {
			if _, ok := sum[s.Name]; !ok {
				order = append(order, s.Name)
			}
			agg := sum[s.Name]
			agg.Name, agg.Threshold, agg.Detail = s.Name, s.Threshold, s.Detail
			agg.Value += s.Value
			if s.ByVerdict {
				agg.ByVerdict = true
				if _, seen := verdictFail[s.Name]; !seen {
					verdictFail[s.Name] = false
				}
				verdictFail[s.Name] = verdictFail[s.Name] || !s.Pass
			}
			sum[s.Name] = agg
			count[s.Name]++
		}
	}
	out := make([]Score, 0, len(order))
	for _, name := range order {
		s := sum[name]
		s.Value /= float64(count[name])
		s.Pass = s.Value >= s.Threshold
		if s.ByVerdict {
			s.Pass = !verdictFail[name]
		}
		out = append(out, s)
	}
	return out
}

func meanMetrics(runs []Run) Metrics {
	var m Metrics
	if len(runs) == 0 {
		return m
	}
	n := len(runs)
	for _, r := range runs {
		x := r.Metrics
		m.ToolCalls += x.ToolCalls
		m.Inferences += x.Inferences
		m.Turns += x.Turns
		m.InputTokens += x.InputTokens
		m.OutputTokens += x.OutputTokens
		m.CacheReadTokens += x.CacheReadTokens
		m.CostUSD += x.CostUSD
		m.Duration += x.Duration
		m.ModelTime += x.ModelTime
		m.ToolTime += x.ToolTime
	}
	d := time.Duration(n)
	m.ToolCalls /= n
	m.Inferences /= n
	m.Turns /= n
	m.InputTokens /= n
	m.OutputTokens /= n
	m.CacheReadTokens /= n
	m.CostUSD /= float64(n)
	m.Duration /= d
	m.ModelTime /= d
	m.ToolTime /= d
	return m
}

// RunnerAgent returns an AgentFunc that runs each case through r with
// RunStream, building the runner.Input from the case, and observes the run
// with a Collector. A result-level CLI error is returned as an error together
// with the partial Run.
func RunnerAgent(r *runner.Runner, build func(Case) runner.Input) AgentFunc {
	return func(ctx context.Context, c Case) (Run, error) {
		col := NewCollector()
		res, err := r.RunStream(ctx, build(c), col.Progress)
		run := col.Run()
		if err != nil {
			return run, err
		}
		if res != nil {
			if run.Response == "" {
				run.Response = res.ResultText
			}
			if run.SessionID == "" {
				run.SessionID = res.SessionID
			}
			if run.Metrics.CostUSD == 0 {
				run.Metrics.CostUSD = res.TotalCostUSD
			}
			if run.Metrics.Duration == 0 {
				run.Metrics.Duration = res.Duration
			}
			if res.IsError {
				return run, fmt.Errorf("eval: agent run reported an error: %s", res.ResultText)
			}
		}
		return run, nil
	}
}
