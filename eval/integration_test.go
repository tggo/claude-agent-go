//go:build integration

package eval

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

// Runs a real haiku agent on a tiny case and checks the trajectory plus the
// ClaudeJudge. Costs a couple of cents.
func TestIntegrationEvalBash(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(4),
		runner.WithTimeout(3*time.Minute),
		runner.WithSkipPermissions(true),
		runner.WithEntrypoint("claude-agent-go-itest"),
	)
	set := Set{ID: "itest", Cases: []Case{{
		ID:               "echo-hi",
		Prompt:           "Use the Bash tool to run `echo hi` and tell me the output",
		ExpectedTools:    []ToolCall{{Name: "Bash"}},
		Match:            InOrder,
		IgnoreArgs:       true,
		ExpectedResponse: "The command printed hi.",
		Rubric:           "The answer states that the command's output was the word hi.",
	}}}
	agent := RunnerAgent(r, func(c Case) runner.Input {
		return runner.Input{Prompt: c.Prompt, Label: c.ID}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rep, err := Evaluate(ctx, set, agent, Criteria{
		TrajectoryThreshold: Threshold(1),
		Judge:               ClaudeJudge(r),
		JudgeThreshold:      Threshold(0.7),
		MaxToolCalls:        5,
	})
	require.NoError(t, err)
	t.Log("\n" + rep.String())
	require.True(t, rep.OK(), rep.String())

	run := rep.Cases[0].Runs[0]
	assert.Equal(t, "Bash", run.ToolCalls[0].Name)
	assert.Contains(t, run.ToolCalls[0].Result, "hi")
	assert.Greater(t, run.Metrics.CostUSD, 0.0)
	assert.Greater(t, run.Metrics.ToolTime, time.Duration(0))
	assert.Greater(t, run.Metrics.ModelTime, time.Duration(0))
	var judge *Score
	for i := range rep.Cases[0].Scores {
		if rep.Cases[0].Scores[i].Name == MetricJudge {
			judge = &rep.Cases[0].Scores[i]
		}
	}
	require.NotNil(t, judge)
	assert.GreaterOrEqual(t, judge.Value, 0.7)
}
