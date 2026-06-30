// Command tools demonstrates exposing a Go function to the agent as an
// in-process MCP tool. The real claude binary connects back and calls it.
//
//	go run ./examples/tools -prompt "What is the weather in Kyiv?"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tggo/claude-agent-go/mcp"
	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/tools"
)

func main() {
	prompt := flag.String("prompt", "Use the get_weather tool for Kyiv and report it.", "prompt")
	flag.Parse()

	srv, err := tools.Serve("demo", tools.Tool{
		Name:        "get_weather",
		Description: "Returns the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				City string `json:"city"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			// Your real logic goes here.
			return fmt.Sprintf("It is 21°C and sunny in %s.", in.City), nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	name, cfg := srv.Config()
	mcpPath := filepath.Join(os.TempDir(), "mcp.demo.json")
	if err := mcp.WriteConfig(mcpPath, map[string]mcp.ServerConfig{name: cfg}); err != nil {
		log.Fatal(err)
	}

	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(5),
		runner.WithTimeout(2*time.Minute),
		runner.WithAllowedTools("mcp__"+name+"__get_weather"),
	)
	res, err := r.RunJSON(context.Background(), runner.Input{Prompt: *prompt, MCPConfigPath: mcpPath})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.ResultText)
}
