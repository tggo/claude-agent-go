package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

func bashRun(resp string) Run {
	return Run{
		ToolCalls: []ToolCall{{Name: "Bash", Args: []byte(`{"command":"echo hi"}`)}},
		Response:  resp,
		Metrics:   Metrics{ToolCalls: 1, Inferences: 2, Turns: 2, InputTokens: 10, OutputTokens: 5, CostUSD: 0.01, Duration: time.Second},
	}
}

func evalSet() Set {
	return Set{ID: "s", Name: "S", Cases: []Case{
		{ID: "good", Prompt: "p1", ExpectedTools: []ToolCall{{Name: "Bash"}}, Match: InOrder, IgnoreArgs: true, ExpectedResponse: "the output is hi"},
		{ID: "bad-tools", Prompt: "p2", ExpectedTools: []ToolCall{{Name: "Read"}}, Match: Exact},
	}}
}

func staticAgent(r Run) AgentFunc {
	return func(context.Context, Case) (Run, error) { return r, nil }
}

func TestEvaluateScoresAndVerdicts(t *testing.T) {
	crit := Criteria{TrajectoryThreshold: Threshold(1), ResponseMatch: Threshold(0.8)}
	rep, err := Evaluate(context.Background(), evalSet(), staticAgent(bashRun("The output is hi.")), crit)
	require.NoError(t, err)
	require.Len(t, rep.Cases, 2)

	good := rep.Cases[0]
	assert.True(t, good.Pass)
	assert.Equal(t, "PASS", good.Verdict())
	require.Len(t, good.Scores, 2)
	assert.Equal(t, MetricTrajectory, good.Scores[0].Name)
	assert.Equal(t, 1.0, good.Scores[0].Value)
	assert.Equal(t, MetricResponseMatch, good.Scores[1].Name)
	assert.InDelta(t, 1.0, good.Scores[1].Value, 1e-9)

	bad := rep.Cases[1]
	assert.False(t, bad.Pass)
	assert.Equal(t, "FAIL", bad.Verdict())
	require.Len(t, bad.Scores, 1, "no expected response -> response_match skipped")
	assert.Equal(t, 0.0, bad.Scores[0].Value)

	assert.Equal(t, 1, rep.Passed())
	assert.Equal(t, 1, rep.Failed())
	assert.Equal(t, 0, rep.Errored())
	assert.False(t, rep.OK())
}

func TestEvaluateSkipsDisabledMetrics(t *testing.T) {
	rep, err := Evaluate(context.Background(), evalSet(), staticAgent(bashRun("x")), Criteria{})
	require.NoError(t, err)
	assert.True(t, rep.OK())
	assert.Empty(t, rep.Cases[0].Scores)
}

func TestEvaluateResponseChecks(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{
		{ID: "ok", Prompt: "p", ResponseContains: []string{"HI"}, ResponseRegex: `^The`},
		{ID: "miss", Prompt: "p", ResponseContains: []string{"hi", "bye"}, ResponseRegex: `^Nope`},
	}}
	rep, err := Evaluate(context.Background(), set, staticAgent(bashRun("The output is hi.")), Criteria{})
	require.NoError(t, err)
	assert.True(t, rep.Cases[0].Pass)
	assert.False(t, rep.Cases[1].Pass)
	assert.Contains(t, rep.Cases[1].Scores[0].Detail, `"bye"`)
	assert.False(t, rep.Cases[1].Scores[1].Pass)
}

func TestEvaluateBudgets(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{{ID: "c", Prompt: "p"}}}
	run := bashRun("x")
	run.ToolCalls = append(run.ToolCalls, run.ToolCalls[0])

	over := Criteria{MaxCostUSD: 0.005, MaxToolCalls: 1, MaxDuration: time.Millisecond}
	rep, _ := Evaluate(context.Background(), set, staticAgent(run), over)
	require.Len(t, rep.Cases[0].Scores, 3)
	for _, s := range rep.Cases[0].Scores {
		assert.False(t, s.Pass, s.Name)
	}

	within := Criteria{MaxCostUSD: 1, MaxToolCalls: 5, MaxDuration: time.Minute}
	rep, _ = Evaluate(context.Background(), set, staticAgent(run), within)
	assert.True(t, rep.OK())
}

func TestEvaluateJudge(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{
		{ID: "rubric", Prompt: "p", Rubric: "r"},
		{ID: "ref", Prompt: "p", ExpectedResponse: "e"},
		{ID: "none", Prompt: "p"},
	}}
	var seen []JudgeInput
	var mu sync.Mutex
	judge := JudgeFunc(func(_ context.Context, in JudgeInput) (JudgeResult, error) {
		mu.Lock()
		seen = append(seen, in)
		mu.Unlock()
		return JudgeResult{Score: 0.75, Pass: true, Reason: "fine"}, nil
	})
	rep, err := Evaluate(context.Background(), set, staticAgent(bashRun("ans")), Criteria{Judge: judge})
	require.NoError(t, err)
	assert.Len(t, seen, 2, "case without rubric/reference is not judged")
	assert.True(t, rep.Cases[0].Pass, "judge verdict Pass")
	assert.Equal(t, "fine", rep.Cases[0].Scores[0].Detail)
	assert.Empty(t, rep.Cases[2].Scores)

	rep, _ = Evaluate(context.Background(), set, staticAgent(bashRun("ans")), Criteria{Judge: judge, JudgeThreshold: Threshold(0.9)})
	assert.False(t, rep.Cases[0].Pass)

	failing := JudgeFunc(func(context.Context, JudgeInput) (JudgeResult, error) { return JudgeResult{}, errors.New("no judge") })
	rep, err = Evaluate(context.Background(), set, staticAgent(bashRun("ans")), Criteria{Judge: failing})
	require.NoError(t, err)
	assert.Equal(t, "ERROR", rep.Cases[0].Verdict())
	assert.ErrorContains(t, rep.Cases[0].Err, "no judge")
	assert.Equal(t, 2, rep.Errored())
	assert.Equal(t, 0, rep.Failed())
}

func TestEvaluateBadRegexIsError(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{{ID: "c", Prompt: "p"}}}
	set.Cases[0].ResponseRegex = "("
	_, err := Evaluate(context.Background(), set, staticAgent(Run{}), Criteria{})
	assert.Error(t, err, "Validate rejects it before running")

	// Bypass validation to exercise the scoring-time guard.
	set.Cases[0].ResponseRegex = ""
	_, _, err = scoreRun(context.Background(), Case{ResponseRegex: "("}, Run{}, Criteria{})
	assert.Error(t, err)
}

func TestEvaluateInvalidSet(t *testing.T) {
	rep, err := Evaluate(context.Background(), Set{Cases: []Case{{ID: "", Prompt: "p"}}}, staticAgent(Run{}), Criteria{})
	assert.Error(t, err)
	assert.Nil(t, rep)
}

func TestEvaluateRepeatsMeanScore(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{{ID: "flaky", Prompt: "p", ExpectedTools: []ToolCall{{Name: "Bash"}}, IgnoreArgs: true, Match: Exact}}}
	var n int32
	agent := func(context.Context, Case) (Run, error) {
		i := atomic.AddInt32(&n, 1)
		r := bashRun("x")
		r.Metrics.CostUSD = float64(i) // 1,2,3,4
		if i%2 == 0 {
			r.ToolCalls = nil // runs 2 and 4 miss the trajectory
		}
		return r, nil
	}
	rep, err := Evaluate(context.Background(), set, agent, Criteria{TrajectoryThreshold: Threshold(0.5)}, WithRepeats(4))
	require.NoError(t, err)
	c := rep.Cases[0]
	assert.Len(t, c.Runs, 4)
	assert.Equal(t, 4, rep.Repeats)
	assert.InDelta(t, 0.5, c.Scores[0].Value, 1e-9)
	assert.True(t, c.Pass, "mean 0.5 meets threshold 0.5")
	assert.InDelta(t, 2.5, c.Metrics.CostUSD, 1e-9, "metrics are the mean over runs")

	atomic.StoreInt32(&n, 0)
	rep, _ = Evaluate(context.Background(), set, agent, Criteria{TrajectoryThreshold: Threshold(0.75)}, WithRepeats(4))
	assert.False(t, rep.Cases[0].Pass)
}

func TestEvaluateAgentErrorStopsRepeats(t *testing.T) {
	var n int32
	agent := func(context.Context, Case) (Run, error) {
		if atomic.AddInt32(&n, 1) == 2 {
			return Run{}, errors.New("crashed")
		}
		return bashRun("x"), nil
	}
	rep, err := Evaluate(context.Background(), Set{ID: "s", Cases: []Case{{ID: "c", Prompt: "p"}}}, agent, Criteria{}, WithRepeats(5))
	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&n))
	assert.ErrorContains(t, rep.Cases[0].Err, "run 2/5: crashed")
	assert.False(t, rep.Cases[0].Pass)
	assert.Empty(t, rep.Cases[0].Scores)
}

func TestEvaluateParallelism(t *testing.T) {
	set := Set{ID: "s"}
	for i := 0; i < 8; i++ {
		set.Cases = append(set.Cases, Case{ID: fmt.Sprintf("c%d", i), Prompt: "p"})
	}
	var cur, peak int32
	agent := func(_ context.Context, c Case) (Run, error) {
		v := atomic.AddInt32(&cur, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if v <= p || atomic.CompareAndSwapInt32(&peak, p, v) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return Run{Response: c.ID}, nil
	}
	rep, err := Evaluate(context.Background(), set, agent, Criteria{}, WithParallelism(4))
	require.NoError(t, err)
	assert.Greater(t, atomic.LoadInt32(&peak), int32(1))
	assert.LessOrEqual(t, atomic.LoadInt32(&peak), int32(4))
	for i, cr := range rep.Cases {
		assert.Equal(t, fmt.Sprintf("c%d", i), cr.Runs[0].Response, "results keep set order")
	}

	atomic.StoreInt32(&peak, 0)
	_, err = Evaluate(context.Background(), set, agent, Criteria{}, WithParallelism(0))
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&peak), "default is sequential")
}

func TestEvaluateCancel(t *testing.T) {
	set := Set{ID: "s"}
	for i := 0; i < 6; i++ {
		set.Cases = append(set.Cases, Case{ID: fmt.Sprintf("c%d", i), Prompt: "p"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	var n int32
	agent := func(ctx context.Context, _ Case) (Run, error) {
		if atomic.AddInt32(&n, 1) == 2 {
			cancel()
		}
		return Run{}, nil
	}
	rep, err := Evaluate(ctx, set, agent, Criteria{})
	assert.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, rep)
	assert.Less(t, len(rep.Cases), 6)
	assert.GreaterOrEqual(t, len(rep.Cases), 2)
	for _, cr := range rep.Cases {
		assert.NotEmpty(t, cr.Case.ID, "only finished cases are reported")
	}
}

func TestMeanMetricsEmpty(t *testing.T) {
	assert.Equal(t, Metrics{}, meanMetrics(nil))
}

func TestRunnerAgent(t *testing.T) {
	script := `printf '%s\n' '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}'
printf '%s\n' '{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"hi"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"sx","result":"The output is hi.","total_cost_usd":0.02,"num_turns":2}'`
	r := runner.New(runner.WithBinary(fakeClaude(t, script)))
	var gotPrompt string
	agent := RunnerAgent(r, func(c Case) runner.Input {
		gotPrompt = c.Prompt
		return runner.Input{Prompt: c.Prompt, Label: c.ID}
	})
	set := Set{ID: "s", Cases: []Case{{
		ID: "echo", Prompt: "run echo hi",
		ExpectedTools: []ToolCall{{Name: "Bash", Args: []byte(`{"command":"echo hi"}`)}}, Match: InOrder,
		ExpectedResponse: "The output is hi",
	}}}
	rep, err := Evaluate(context.Background(), set, agent, Criteria{TrajectoryThreshold: Threshold(1), ResponseMatch: Threshold(0.9)})
	require.NoError(t, err)
	assert.Equal(t, "run echo hi", gotPrompt)
	assert.True(t, rep.OK(), rep.String())
	run := rep.Cases[0].Runs[0]
	assert.Equal(t, "sx", run.SessionID)
	assert.InDelta(t, 0.02, run.Metrics.CostUSD, 1e-9)
	assert.Equal(t, "hi", run.ToolCalls[0].Result)
}

func TestRunnerAgentErrors(t *testing.T) {
	in := func(c Case) runner.Input { return runner.Input{Prompt: c.Prompt} }

	// Result-level CLI error: partial run plus an error.
	errBin := catClaude(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"e","result":"it broke","total_cost_usd":0.01}`)
	run, err := RunnerAgent(runner.New(runner.WithBinary(errBin)), in)(context.Background(), Case{Prompt: "p"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "it broke")
	assert.Equal(t, "it broke", run.Response)

	// Process failure.
	_, err = RunnerAgent(runner.New(runner.WithBinary("/nonexistent/claude")), in)(context.Background(), Case{Prompt: "p"})
	assert.Error(t, err)

	// Result-only stream: response, cost and duration come from the Result.
	okBin := catClaude(t, `{"type":"result","subtype":"success","session_id":"o","result":"fine","total_cost_usd":0.03}`)
	run, err = RunnerAgent(runner.New(runner.WithBinary(okBin)), in)(context.Background(), Case{Prompt: "p"})
	require.NoError(t, err)
	assert.Equal(t, "fine", run.Response)
	assert.True(t, strings.HasPrefix(run.SessionID, "o"))
	assert.InDelta(t, 0.03, run.Metrics.CostUSD, 1e-9)
}

func TestEvaluateJudgeThresholdPrecedence(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{{ID: "c", Prompt: "p", Rubric: "r"}}}
	var n int32
	// Verdicts alternate: pass, fail, pass, fail; score 0.5 always; cost 0.01.
	judge := JudgeFunc(func(context.Context, JudgeInput) (JudgeResult, error) {
		i := atomic.AddInt32(&n, 1)
		return JudgeResult{Score: 0.5, Pass: i%2 == 1, Reason: "r", CostUSD: 0.01}, nil
	})
	agent := staticAgent(bashRun("x"))

	// nil threshold: the judge's verdict decides; one run passes.
	rep, _ := Evaluate(context.Background(), set, agent, Criteria{Judge: judge})
	assert.True(t, rep.Cases[0].Pass)
	assert.True(t, rep.Cases[0].Scores[0].ByVerdict)
	assert.Contains(t, rep.String(), "(judge verdict) pass")

	// nil threshold with repeats: every run must pass.
	atomic.StoreInt32(&n, 0)
	rep, _ = Evaluate(context.Background(), set, agent, Criteria{Judge: judge}, WithRepeats(2))
	assert.False(t, rep.Cases[0].Pass)
	assert.InDelta(t, 0.02, rep.Cases[0].JudgeCostUSD, 1e-9)

	// Explicit zero threshold is expressible and overrides the verdict.
	atomic.StoreInt32(&n, 1) // next verdict is "fail"
	rep, _ = Evaluate(context.Background(), set, agent, Criteria{Judge: judge, JudgeThreshold: Threshold(0)})
	assert.True(t, rep.Cases[0].Pass)
	assert.False(t, rep.Cases[0].Scores[0].ByVerdict)
}

func TestJudgeCostReportedNotBudgeted(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{{ID: "c", Prompt: "p", Rubric: "r"}}}
	judge := JudgeFunc(func(context.Context, JudgeInput) (JudgeResult, error) {
		return JudgeResult{Score: 1, Pass: true, CostUSD: 5}, nil
	})
	rep, _ := Evaluate(context.Background(), set, staticAgent(bashRun("x")), Criteria{Judge: judge, MaxCostUSD: 0.05})
	cr := rep.Cases[0]
	assert.True(t, cr.Pass, "judge cost is outside the agent budget")
	assert.Equal(t, 5.0, cr.JudgeCostUSD)
	assert.InDelta(t, 0.01, cr.Metrics.CostUSD, 1e-9)
	assert.Contains(t, rep.String(), "judge_cost=$5.0000 (not in cost above)")
}
