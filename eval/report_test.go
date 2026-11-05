package eval

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportText(t *testing.T) {
	set := Set{ID: "smoke", Name: "Smoke", Cases: []Case{
		{ID: "good", Prompt: "run   echo\nhi", ExpectedTools: []ToolCall{{Name: "Bash", Args: []byte(`{ "command": "echo hi" }`)}},
			Match: InOrder, ExpectedResponse: "the output is hi"},
		{ID: "bad", Prompt: "read it", ExpectedTools: []ToolCall{{Name: "Read"}}, IgnoreArgs: true},
		{ID: "boom", Prompt: "x"},
	}}
	agent := func(_ context.Context, c Case) (Run, error) {
		if c.ID == "boom" {
			return Run{}, errors.New("agent exploded")
		}
		r := bashRun("The output is hi.")
		r.Metrics.ModelTime, r.Metrics.ToolTime = 800*time.Millisecond, 200*time.Millisecond
		return r, nil
	}
	rep, err := Evaluate(context.Background(), set, agent, Criteria{TrajectoryThreshold: Threshold(1), ResponseMatch: Threshold(0.8), MaxToolCalls: 3})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, rep.WriteText(&buf))
	out := buf.String()
	assert.Equal(t, out, rep.String())

	for _, want := range []string{
		"Eval set: smoke (Smoke), 3 case(s), 1 run(s) each",
		"PASS  good",
		"prompt:   run echo hi",
		`expected tools (in_order): Bash({"command":"echo hi"})`,
		`actual tools: Bash({"command":"echo hi"})`,
		"expected response: the output is hi",
		"actual response:   The output is hi.",
		"trajectory         1.00 (threshold 1.00) pass",
		"response_match     1.00 (threshold 0.80) pass",
		"max_tool_calls     1.00 (threshold 1.00) pass  1 of 3",
		"metrics: tool_calls=1 inferences=2 turns=2 tokens(in/out/cache)=10/5/0 cost=$0.0100",
		"duration=1s model_time=800ms tool_time=200ms",
		"FAIL  bad",
		"expected tools (exact, ignore args): Read()",
		"trajectory         0.00 (threshold 1.00) FAIL  match=exact",
		"ERROR  boom",
		"error:    run 1/1: agent exploded",
		"Summary: 1 passed, 1 failed, 1 errored",
	} {
		assert.Contains(t, out, want)
	}
	assert.Contains(t, rep.Cases[1].Detail(), "FAIL  bad")
}

func TestReportMultiRunAndFormatting(t *testing.T) {
	cr := CaseResult{
		Case: Case{ID: "c", Prompt: "p", ExpectedTools: []ToolCall{}},
		Runs: []Run{bashRun("one"), {Response: "two"}},
	}
	out := cr.Detail()
	assert.Contains(t, out, "expected tools (exact): (none)")
	assert.Contains(t, out, "actual tools [run 1]: Bash(")
	assert.Contains(t, out, "actual tools [run 2]: (none)")
	assert.Contains(t, out, "actual response:   two", "last run's response")

	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	assert.Len(t, []rune(oneLine(string(long), 50)), 53)
	assert.Equal(t, "Tool(not json)", formatCalls([]ToolCall{{Name: "Tool", Args: []byte("not json")}}))
	assert.Equal(t, "A() -> B()", formatCalls([]ToolCall{{Name: "A", Args: []byte("null")}, {Name: "B"}}))
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n--; f.n < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestWriteTextPropagatesWriteError(t *testing.T) {
	rep := &Report{SetID: "s", Cases: []CaseResult{{Case: Case{ID: "c"}, Pass: true}}}
	assert.ErrorContains(t, rep.WriteText(&failWriter{n: 1}), "disk full")
	assert.ErrorContains(t, rep.WriteText(&failWriter{n: 0}), "disk full")
}
