package eval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

// ADK #3593: a single LLM judge sample is flaky; JudgeSamples takes the median.
// The fake binary replies with the next score of a fixed sequence per call.
func sequenceJudgeBin(t *testing.T, replies ...string) (bin, counter string) {
	t.Helper()
	counter = filepath.Join(t.TempDir(), "n")
	var cases strings.Builder
	for i, r := range replies {
		out := fmt.Sprintf(`{"type":"result","result":%q,"total_cost_usd":0.002,"session_id":"j"}`, r)
		fmt.Fprintf(&cases, "%d) cat <<'EOF'\n%s\nEOF\n;;\n", i+1, out)
	}
	bin = fakeClaude(t, fmt.Sprintf(`cat >/dev/null
CF=%q
n=$(cat "$CF" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$CF"
case $n in
%s*) exit 1;;
esac`, counter, cases.String()))
	return bin, counter
}

func TestADK3593JudgeSamplesMedian(t *testing.T) {
	cases := []struct {
		name    string
		samples int
		replies []string
		score   float64
		pass    bool
		cost    float64
		wantErr bool
	}{
		{"outlier low", 3, []string{`{"score":0.1}`, `{"score":0.9}`, `{"score":0.8}`}, 0.8, true, 0.006, false},
		{"outlier high", 3, []string{`{"score":0.95}`, `{"score":0.2}`, `{"score":0.3}`}, 0.3, false, 0.006, false},
		{"unparseable sample skipped", 3, []string{`nonsense`, `{"score":0.6}`, `{"score":0.8}`}, 0.7, true, 0.006, false},
		{"failing process skipped", 3, []string{`{"score":0.9}`}, 0.9, true, 0.002, false},
		{"even count averages middle", 4, []string{`{"score":0.2}`, `{"score":0.4}`, `{"score":0.6}`, `{"score":1}`}, 0.5, false, 0.008, false},
		{"all fail", 2, []string{`x`, `y`}, 0, false, 0.004, true},
		{"one sample is the old behaviour", 1, []string{`{"score":0.4}`, `{"score":0.9}`}, 0.4, false, 0.002, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := sequenceJudgeBin(t, tc.replies...)
			j := ClaudeJudge(runner.New(runner.WithBinary(bin)), JudgeSamples(tc.samples))
			res, err := j.Score(context.Background(), JudgeInput{Prompt: "q", Response: "a"})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tc.score, res.Score, 1e-9)
			assert.Equal(t, tc.pass, res.Pass)
			if tc.name != "failing process skipped" { // failed runs report no cost
				assert.InDelta(t, tc.cost, res.CostUSD, 1e-9)
			}
		})
	}
}

func TestJudgeSamplesStopsOnCancel(t *testing.T) {
	bin, counter := sequenceJudgeBin(t, `{"score":0.5}`, `{"score":0.5}`, `{"score":0.5}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ClaudeJudge(runner.New(runner.WithBinary(bin)), JudgeSamples(3)).Score(ctx, JudgeInput{Prompt: "q", Response: "a"})
	require.ErrorIs(t, err, context.Canceled)
	assert.NoFileExists(t, counter)
}

// Deterministic metrics never reach the judge, even when one is configured.
func TestADK3593DeterministicMetricsSkipJudge(t *testing.T) {
	judge := JudgeFunc(func(context.Context, JudgeInput) (JudgeResult, error) {
		t.Error("judge called for a deterministic-only case")
		return JudgeResult{}, nil
	})
	set := Set{ID: "s", Cases: []Case{{
		ID: "det", Prompt: "p",
		ExpectedTools:    []ToolCall{{Name: "Bash", Args: []byte(`{"command":"echo hi"}`)}},
		Match:            Exact,
		ResponseContains: []string{"ans"}, ResponseRegex: "a.s",
	}}}
	rep, err := Evaluate(context.Background(), set, staticAgent(bashRun("ans")),
		Criteria{Judge: judge, TrajectoryThreshold: Threshold(1)})
	require.NoError(t, err)
	assert.True(t, rep.Cases[0].Pass)
	assert.Zero(t, rep.Cases[0].JudgeCostUSD)
}

// ADK #3984: sub-agent tool calls are kept, tagged, and do not become the answer.
func TestADK3984SubagentToolCallsTagged(t *testing.T) {
	run := FromStream(lines(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Task","input":{}},{"type":"text","text":"main text"}]}}
{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"Read","input":{}},{"type":"text","text":"sub text"}]}}`))
	require.Len(t, run.ToolCalls, 2)
	assert.Empty(t, run.ToolCalls[0].ParentToolUseID)
	assert.Equal(t, "t1", run.ToolCalls[1].ParentToolUseID)
	assert.Equal(t, 2, run.Metrics.ToolCalls)
	assert.Equal(t, "main text", run.Response)
}
