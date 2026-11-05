package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

// judgeBin fakes `claude --output-format json` replying with reply as the
// result text; it also records its stdin so tests can inspect the prompt.
func judgeBin(t *testing.T, reply string) (bin, stdin string) {
	t.Helper()
	out, _ := json.Marshal(map[string]any{"type": "result", "result": reply, "total_cost_usd": 0.002, "session_id": "j"})
	stdin = t.TempDir() + "/stdin.txt"
	return fakeClaude(t, "cat > '"+stdin+"'\ncat <<'EOF'\n"+string(out)+"\nEOF"), stdin
}

func TestClaudeJudge(t *testing.T) {
	tests := []struct {
		name    string
		reply   string
		score   float64
		pass    bool
		wantErr string
	}{
		{"plain", `{"score": 0.9, "reason": "good"}`, 0.9, true, ""},
		{"fenced", "Sure!\n```json\n{\"score\": 0.5, \"reason\": \"meh\"}\n```\nDone.", 0.5, false, ""},
		{"clamped high", `{"score": 7, "reason": "x"}`, 1, true, ""},
		{"clamped low", `{"score": -3, "reason": "x"}`, 0, false, ""},
		{"exact threshold", `{"score": 0.7, "reason": "x"}`, 0.7, true, ""},
		{"garbage", "I cannot grade this", 0, false, "not JSON"},
		{"no score", `{"reason": "forgot"}`, 0, false, "no numeric"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, _ := judgeBin(t, tt.reply)
			j := ClaudeJudge(runner.New(runner.WithBinary(bin)))
			res, err := j.Score(context.Background(), JudgeInput{Prompt: "q", Response: "a"})
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tt.score, res.Score, 1e-9)
			assert.Equal(t, tt.pass, res.Pass)
			assert.InDelta(t, 0.002, res.CostUSD, 1e-9)
		})
	}
}

func TestClaudeJudgeOptionsAndPrompt(t *testing.T) {
	bin, stdin := judgeBin(t, `{"score":0.6,"reason":"ok"}`)
	j := ClaudeJudge(runner.New(runner.WithBinary(bin)), JudgeModel("sonnet"), JudgeThreshold(0.5))
	res, err := j.Score(context.Background(), JudgeInput{Prompt: "PROMPT-X", Response: "RESP-Y", Reference: "REF-Z", Rubric: "RUBRIC-W"})
	require.NoError(t, err)
	assert.True(t, res.Pass, "0.6 >= custom threshold 0.5")
	assert.Equal(t, "ok", res.Reason)

	got := readFile(t, stdin)
	for _, want := range []string{"PROMPT-X", "RESP-Y", "REF-Z", "RUBRIC-W", `"score"`, "<rubric_", "<reference_answer_"} {
		assert.Contains(t, got, want)
	}

	bare := judgePrompt(JudgeInput{Prompt: "p", Response: "r"})
	assert.NotContains(t, bare, "<rubric_")
	assert.NotContains(t, bare, "<reference_answer>")
}

func TestClaudeJudgeRunnerError(t *testing.T) {
	j := ClaudeJudge(runner.New(runner.WithBinary("/nonexistent/claude")))
	_, err := j.Score(context.Background(), JudgeInput{Prompt: "q", Response: "a"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "judge run"))
}

func TestJudgeFunc(t *testing.T) {
	var j Judge = JudgeFunc(func(_ context.Context, in JudgeInput) (JudgeResult, error) {
		if in.Response == "" {
			return JudgeResult{}, errors.New("empty")
		}
		return JudgeResult{Score: 1, Pass: true}, nil
	})
	r, err := j.Score(context.Background(), JudgeInput{Response: "x"})
	require.NoError(t, err)
	assert.Equal(t, 1.0, r.Score)
	_, err = j.Score(context.Background(), JudgeInput{})
	assert.Error(t, err)
}
