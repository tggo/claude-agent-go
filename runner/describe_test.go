package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveMergesAndRedacts(t *testing.T) {
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(mcp, []byte(`{"mcpServers":{
	  "b":{"type":"http","url":"https://user:pw@host.example/mcp?token=sekret-q","headers":{"Authorization":"Bearer sekret-h"}},
	  "a":{"command":"/usr/bin/tool","args":["--key","sekret-a"],"env":{"K":"sekret-e"}}}}`), 0o600))
	r := New(WithModel("haiku"), WithMaxTurns(7), WithMaxBudgetUSD("0.50"), WithTimeout(3*time.Minute),
		WithAllowedTools("Read", "Bash"), WithEnv("API_KEY=sekret-env", "PLAIN=1"))
	e := r.Effective(Input{Prompt: "do it", Label: "n", WorkDir: "/w", PermissionMode: "plan", Resume: "s1", MCPConfigPath: mcp,
		Env:       []string{"X_TOKEN=sekret-in", "PLAIN=2"},
		ExtraArgs: []string{"--tools", "", "--api-key", "sekret-x", "--token=sekret-y", "--url", "https://h/p?k=sekret-z", "FOO_SECRET=sekret-w", "--keep", "v"}})
	assert.Equal(t, "haiku", e.Model)
	assert.Equal(t, 7, e.MaxTurns)
	assert.Equal(t, "0.50", e.MaxBudgetUSD)
	assert.Equal(t, int64(180000), e.TimeoutMS)
	assert.Equal(t, []string{"Read", "Bash"}, e.AllowedTools)
	assert.Equal(t, "do it", e.Prompt)
	assert.Equal(t, []string{"API_KEY", "PLAIN", "X_TOKEN"}, e.EnvNames, "names only, deduplicated")
	assert.Equal(t, []MCPServer{{Name: "a", Command: "tool"}, {Name: "b", Type: "http", URL: "https://host.example/mcp"}}, e.MCPServers)
	assert.Equal(t, []string{"--tools", "", "--api-key", "***", "--token=***", "--url", "https://h/p", "FOO_SECRET=***", "--keep", "v"}, e.ExtraArgs)
	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "sekret")

	// Input overrides the default model; defaults describe a zero Input; bad MCP files are ignored.
	assert.Equal(t, "opus", r.Effective(Input{Model: "opus"}).Model)
	assert.Empty(t, r.Effective(Input{}).MCPServers)
	assert.Empty(t, r.Effective(Input{MCPConfigPath: "/nonexistent"}).MCPServers)
	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
	assert.Empty(t, r.Effective(Input{MCPConfigPath: bad}).MCPServers)
	assert.Nil(t, redactArgs(nil))
	assert.Equal(t, "(redacted)", stripURL("weird@thing"))
	assert.Equal(t, "plain", stripURL("plain"))
}
