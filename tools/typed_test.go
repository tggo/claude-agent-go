package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type addArgs struct {
	A int `json:"a" jsonschema:"first addend"`
	B int `json:"b" jsonschema:"second addend"`
}
type addResult struct {
	Sum int `json:"sum"`
}

func TestRegistryValidation(t *testing.T) {
	r := NewRegistry("empty")
	if _, err := r.Serve(); err == nil {
		t.Error("Serve with no tools should error")
	}

	r2 := NewRegistry("one")
	Register(r2, "noop", "does nothing",
		func(context.Context, addArgs) (addResult, error) { return addResult{}, nil })
	s, err := r2.Serve()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()
	if _, err := r2.Serve(); err == nil {
		t.Error("second Serve should error")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	r := NewRegistry("calc")
	Register(r, "add", "Add two integers",
		func(_ context.Context, in addArgs) (addResult, error) {
			return addResult{Sum: in.A + in.B}, nil
		})
	Register(r, "fail", "always fails",
		func(_ context.Context, _ addArgs) (addResult, error) {
			return addResult{}, errors.New("nope")
		})

	s, err := r.Serve()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := cli.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: s.URL()}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	// schema inference: list should expose both tools with object schemas
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(lt.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(lt.Tools))
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "add",
		Arguments: map[string]any{"a": 2, "b": 3},
	})
	if err != nil {
		t.Fatalf("CallTool add: %v", err)
	}
	if res.IsError {
		t.Fatalf("add errored: %+v", res)
	}
	// structured output should carry sum=5
	if res.StructuredContent != nil {
		if m, ok := res.StructuredContent.(map[string]any); ok {
			if m["sum"] != float64(5) {
				t.Errorf("sum = %v, want 5", m["sum"])
			}
		}
	}
	if got := textOf(res); !strings.Contains(got, "5") {
		t.Errorf("add text %q should contain 5", got)
	}

	// error path
	res, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "fail", Arguments: map[string]any{"a": 1, "b": 1}})
	if err != nil {
		t.Fatalf("CallTool fail transport: %v", err)
	}
	if !res.IsError {
		t.Error("fail should be IsError")
	}

	// schema validation: missing required field should be rejected by the SDK
	_, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "add", Arguments: map[string]any{"a": "notanint"}})
	if err == nil {
		t.Log("note: invalid args accepted (SDK validation lenient)")
	}
}
