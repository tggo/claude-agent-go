package eval

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyAgent passes (calls Bash) on the runs flagged true, in call order.
func flakyAgent(pattern ...bool) AgentFunc {
	var n atomic.Int32
	return func(context.Context, Case) (Run, error) {
		i := int(n.Add(1)) - 1
		if pattern[i%len(pattern)] {
			return Run{ToolCalls: []ToolCall{{Name: "Bash"}}, Response: "ok"}, nil
		}
		return Run{Response: "ok"}, nil
	}
}

func flakySet() Set {
	return Set{ID: "f", Cases: []Case{{ID: "c", Prompt: "p", ExpectedTools: []ToolCall{{Name: "Bash"}}, Match: InOrder, IgnoreArgs: true}}}
}

func TestPassRateSemantics(t *testing.T) {
	crit := Criteria{TrajectoryThreshold: Threshold(1)}
	tests := []struct {
		name   string
		crit   Criteria
		opts   []EvalOption
		expect bool
	}{
		{"default mean threshold fails flaky", crit, nil, false},
		{"option 0.8 tolerates one flake", crit, []EvalOption{PassRate(0.8)}, true},
		{"option 0.9 does not", crit, []EvalOption{PassRate(0.9)}, false},
		{"criteria field", Criteria{TrajectoryThreshold: Threshold(1), MinPassRate: 0.8}, nil, true},
		{"option overrides criteria", Criteria{TrajectoryThreshold: Threshold(1), MinPassRate: 0.5}, []EvalOption{PassRate(1)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]EvalOption{WithRepeats(5)}, tt.opts...)
			rep, err := Evaluate(context.Background(), flakySet(), flakyAgent(true, true, false, true, true), tt.crit, opts...)
			require.NoError(t, err)
			cr := rep.Cases[0]
			assert.Equal(t, tt.expect, cr.Pass)
			assert.Equal(t, 4, cr.Passes)
			assert.Len(t, cr.Runs, 5)
			assert.InDelta(t, 0.8, cr.PassRate, 1e-9)
			assert.Equal(t, []bool{true, true, false, true, true}, cr.RunPass)
		})
	}
}

func TestPassAtK(t *testing.T) {
	cr := CaseResult{RunPass: make([]bool, 5), Passes: 1}
	assert.InDelta(t, 0.2, cr.PassAtK(1), 1e-9)
	assert.InDelta(t, 0.4, cr.PassAtK(2), 1e-9)
	assert.InDelta(t, 1.0, cr.PassAtK(5), 1e-9)
	assert.InDelta(t, 1.0, cr.PassAtK(99), 1e-9, "k clamps to n")
	cr.Passes = 4
	assert.InDelta(t, 1.0, cr.PassAtK(2), 1e-9)
	assert.Equal(t, 0.0, cr.PassAtK(0))
	assert.Equal(t, 0.0, CaseResult{}.PassAtK(1))
	assert.Equal(t, 0.0, (&Report{}).PassAtK(1))
	assert.Equal(t, 0.0, CaseResult{RunPass: []bool{true}, Passes: 1, Err: assert.AnError}.PassAtK(1))
}

func TestPassRateReport(t *testing.T) {
	rep, err := Evaluate(context.Background(), flakySet(), flakyAgent(true, true, false, true, true),
		Criteria{TrajectoryThreshold: Threshold(1)}, WithRepeats(5), PassRate(0.8))
	require.NoError(t, err)
	out := rep.String()
	assert.Contains(t, out, "4/5 (0.80, min 0.80) pass")
	assert.Contains(t, out, "pass@1=0.80 pass@5=1.00")

	// No pass-rate requirement: informational line only.
	rep, _ = Evaluate(context.Background(), flakySet(), flakyAgent(true, false), Criteria{TrajectoryThreshold: Threshold(1)}, WithRepeats(2))
	assert.True(t, strings.Contains(rep.String(), "1/2 (0.50)"))
}
