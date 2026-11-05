//go:build integration

package graph

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

type codeword struct {
	Word  string
	Upper string
}

func clean(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), ".\"'`"))
}

// The second node can only answer correctly if it received the first node's
// output through the state.
func TestIntegrationTwoAgentNodes(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	r := runner.New(runner.WithModel("haiku"), runner.WithMaxTurns(2), runner.WithTimeout(2*time.Minute),
		runner.WithEntrypoint("claude-agent-go-itest"))

	g, err := New[codeword]("itest").
		AddNode("pick", Agent(r,
			func(*codeword) runner.Input {
				return runner.Input{Prompt: "Reply with one single random lowercase English word of 7 letters and nothing else: no punctuation, no explanation."}
			},
			func(s *codeword, res *runner.Result) error { s.Word = clean(res.ResultText); return nil })).
		AddNode("echo", Agent(r,
			func(s *codeword) runner.Input {
				return runner.Input{Prompt: "Write the word \"" + s.Word + "\" in UPPERCASE. Reply with only that uppercase word, nothing else."}
			},
			func(s *codeword, res *runner.Result) error { s.Upper = clean(res.ResultText); return nil })).
		AddEdge(Start, "pick").AddEdge("pick", "echo").AddEdge("echo", End).
		Compile()
	require.NoError(t, err)

	out, rep, err := g.Run(context.Background(), codeword{})
	require.NoError(t, err)
	require.NotEmpty(t, out.Word)

	assert.Equal(t, strings.ToLower(out.Word), out.Upper, "second node must echo the first node's word, word=%q", out.Word)
	assert.Equal(t, []string{"pick", "echo"}, rep.Path)
	assert.Positive(t, rep.TotalCostUSD)
	t.Logf("word=%q upper=%q cost=$%.4f duration=%s", out.Word, out.Upper, rep.TotalCostUSD, rep.Duration)
}
