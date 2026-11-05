package graphweb

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/clitest"
	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/runner"
)

// A real agent node through a fake CLI: the journal carries the effective
// input, stream events and static runner config, and no secret reaches it.
func TestAgentInputJournaledWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(mcp, []byte(`{"mcpServers":{"s":{"type":"http","url":"https://h.example/mcp?token=sekret-q","headers":{"Authorization":"Bearer sekret-h"},"env":{"K":"sekret-e"}}}}`), 0o600))
	bin := clitest.FakeCLI(t, clitest.Init("s1"), clitest.Text("hello"), clitest.Result("done", 0.02))
	r := runner.New(runner.WithBinary(bin), runner.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		runner.WithModel("haiku"), runner.WithMaxTurns(6), runner.WithEnv("API_KEY=sekret-env"))
	jpath := filepath.Join(dir, "j.jsonl")
	s := newSrv(t, WithJournal(jpath))
	g, err := graph.New[st]("a").
		AgentRunner("coder", r).
		AddAgentNode("coder", graph.Agent(r, func(*st) runner.Input {
			return runner.Input{Prompt: "write the thing", MCPConfigPath: mcp, Env: []string{"TOKEN=sekret-in"},
				ExtraArgs: []string{"--api-key", "sekret-arg"}}
		}, nil, graph.WithRetry(runner.RetryPolicy{MaxAttempts: 2, Resume: true}))).
		AddEdge(graph.Start, "coder").AddEdge("coder", graph.End).Compile(graph.WithObserver(s))
	require.NoError(t, err)
	_, _, err = g.Run(context.Background(), st{})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	var in Event
	var agents int
	var top *graph.Topology
	for _, e := range s.Events() {
		switch e.Type {
		case TypeAgentInput:
			in = e
		case TypeAgentEvent:
			agents++
		case TypeGraphStart:
			top = e.Topology
		}
	}
	require.NotEmpty(t, in.Input)
	var ai graph.AgentInfo
	require.NoError(t, json.Unmarshal(in.Input, &ai))
	assert.Equal(t, "write the thing", ai.Input.Prompt)
	assert.Equal(t, "haiku", ai.Input.Model)
	assert.Equal(t, 6, ai.Input.MaxTurns)
	assert.Equal(t, "coder", ai.Input.Label)
	assert.Equal(t, []string{"API_KEY", "TOKEN"}, ai.Input.EnvNames)
	require.Len(t, ai.Input.MCPServers, 1)
	assert.Equal(t, "https://h.example/mcp", ai.Input.MCPServers[0].URL)
	assert.Equal(t, []string{"--api-key", "***"}, ai.Input.ExtraArgs)
	assert.Equal(t, &graph.RetryInfo{MaxAttempts: 2, Resume: true}, ai.Retry)
	assert.Equal(t, 3, agents)
	require.NotNil(t, top)
	assert.Equal(t, "haiku", top.Nodes[1].Runner.Model)

	raw, err := os.ReadFile(jpath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "sekret", "no secret may reach the journal")
}
