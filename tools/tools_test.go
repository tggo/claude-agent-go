package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServeValidation(t *testing.T) {
	ok := Tool{Name: "x", Description: "d", Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }}

	if _, err := Serve(""); err == nil {
		t.Error("empty server name should error")
	}
	if _, err := Serve("s"); err == nil {
		t.Error("no tools should error")
	}
	if _, err := Serve("s", Tool{Description: "d", Handler: ok.Handler}); err == nil {
		t.Error("empty tool name should error")
	}
	if _, err := Serve("s", Tool{Name: "x"}); err == nil {
		t.Error("nil handler should error")
	}
	s, err := Serve("s", ok)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()

	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL = %q", s.URL())
	}
	if s.Name() != "s" {
		t.Errorf("Name = %q", s.Name())
	}
	name, cfg := s.Config()
	if name != "s" || cfg.Type != "http" || cfg.URL != s.URL() {
		t.Errorf("Config = %q %+v", name, cfg)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Errorf("second Close: %v", err)
	}
}

// TestServeRoundTrip exercises the full handler path by connecting a real MCP
// client to the in-process HTTP server — no claude binary required.
func TestServeRoundTrip(t *testing.T) {
	echo := Tool{
		Name:        "echo",
		Description: "echo back the message field",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			return "echo: " + in.Message, nil
		},
	}
	boom := Tool{
		Name:        "boom",
		Description: "always fails",
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "", errors.New("kaboom") },
	}

	s, err := Serve("test-tools", echo, boom)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	sess, err := cli.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: s.URL()}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	// list tools
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(lt.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(lt.Tools))
	}

	// successful call
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hi"},
	})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if res.IsError {
		t.Fatalf("echo unexpectedly errored: %+v", res)
	}
	if got := textOf(res); got != "echo: hi" {
		t.Errorf("echo text = %q", got)
	}

	// error path
	res, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "boom", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool boom transport err: %v", err)
	}
	if !res.IsError {
		t.Error("boom should be IsError")
	}
	if got := textOf(res); !strings.Contains(got, "kaboom") {
		t.Errorf("boom text = %q", got)
	}
}

func textOf(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
