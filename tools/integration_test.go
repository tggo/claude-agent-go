//go:build integration

// Integration test proving the real `claude` binary can call an in-process Go
// tool over the local HTTP MCP server. Run with:
//
//	go test -tags integration ./tools/...
//
// Requires `claude` on PATH and valid credentials.
package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/mcp"
	"github.com/tggo/claude-agent-go/runner"
)

func TestIntegrationRealClaudeCallsGoTool(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	var called atomic.Int32
	const secret = "42-zebra-9931"

	srv, err := Serve("agentgo",
		Tool{
			Name:        "get_secret_code",
			Description: "Returns the secret code the user is asking for. Call this whenever the user asks for the secret code.",
			Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
				called.Add(1)
				return "The secret code is " + secret, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer srv.Close()

	// Write an MCP config pointing the CLI at our in-process server.
	name, cfg := srv.Config()
	mcpPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := mcp.WriteConfig(mcpPath, map[string]mcp.ServerConfig{name: cfg}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// The CLI exposes MCP tools as mcp__<server>__<tool>.
	toolName := "mcp__" + name + "__get_secret_code"

	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(5),
		runner.WithTimeout(3*time.Minute),
		runner.WithAllowedTools(toolName),
		runner.WithEntrypoint("claude-agent-go-tool-itest"),
	)

	res, err := r.RunJSON(context.Background(), runner.Input{
		Prompt:        "Use the get_secret_code tool to retrieve the secret code, then reply with exactly the code value and nothing else.",
		MCPConfigPath: mcpPath,
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}

	if called.Load() == 0 {
		t.Fatalf("the Go tool was never called by claude; result was: %q", res.ResultText)
	}
	if !strings.Contains(res.ResultText, secret) {
		t.Errorf("result %q does not contain the secret %q", res.ResultText, secret)
	}
	t.Logf("tool called %d time(s); result=%q cost=$%.5f", called.Load(), res.ResultText, res.TotalCostUSD)
}

type addIn struct {
	A int `json:"a" jsonschema:"first number"`
	B int `json:"b" jsonschema:"second number"`
}
type addOut struct {
	Sum int `json:"sum"`
}

// TestIntegrationTypedToolCalledByClaude proves the generic Register path —
// schema inferred from a Go struct — works end-to-end with the real binary.
func TestIntegrationTypedToolCalledByClaude(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	var called atomic.Int32
	reg := NewRegistry("calc")
	Register(reg, "add_numbers", "Adds two integers a and b and returns their sum.",
		func(_ context.Context, in addIn) (addOut, error) {
			called.Add(1)
			return addOut{Sum: in.A + in.B}, nil
		})
	srv, err := reg.Serve()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer srv.Close()

	name, cfg := srv.Config()
	mcpPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := mcp.WriteConfig(mcpPath, map[string]mcp.ServerConfig{name: cfg}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(5),
		runner.WithTimeout(3*time.Minute),
		runner.WithAllowedTools("mcp__"+name+"__add_numbers"),
	)
	res, err := r.RunJSON(context.Background(), runner.Input{
		Prompt:        "Use the add_numbers tool to add 137 and 245, then reply with exactly the resulting number.",
		MCPConfigPath: mcpPath,
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if called.Load() == 0 {
		t.Fatalf("typed tool was never called; result=%q", res.ResultText)
	}
	if !strings.Contains(res.ResultText, "382") {
		t.Errorf("result %q should contain 382 (137+245)", res.ResultText)
	}
	t.Logf("typed tool called %d time(s); result=%q", called.Load(), res.ResultText)
}
