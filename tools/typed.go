package tools

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registry builds an MCP tool server from strongly-typed Go handlers. Unlike
// the untyped Serve path (raw json.RawMessage in, string out), Register infers
// the JSON Schema from the In struct's fields (and `jsonschema` struct tags),
// validates arguments before your handler runs, and marshals the Out value back
// — the Go-native ergonomics analogue of the Python SDK's @tool decorator, with
// compile-time types instead of hand-written schemas.
//
//	reg := tools.NewRegistry("myapp")
//	tools.Register(reg, "add", "Add two numbers",
//	    func(ctx context.Context, in AddArgs) (AddResult, error) { ... })
//	srv, _ := reg.Serve()
//	defer srv.Close()
type Registry struct {
	name   string
	srv    *mcpsdk.Server
	count  int
	served bool
}

// NewRegistry creates a typed-tool registry named serverName.
func NewRegistry(serverName string) *Registry {
	return &Registry{
		name: serverName,
		srv:  mcpsdk.NewServer(&mcpsdk.Implementation{Name: serverName, Version: "0.1.0"}, nil),
	}
}

// Register adds a typed tool. In and Out must be structs (or maps) so their
// schemas have object type, per the MCP spec. The handler receives a decoded,
// validated In and returns an Out that is marshalled to the tool result.
func Register[In, Out any](r *Registry, name, description string, h func(context.Context, In) (Out, error)) {
	mcpsdk.AddTool(r.srv, &mcpsdk.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, Out, error) {
			out, err := h(ctx, in)
			if err != nil {
				var zero Out
				return &mcpsdk.CallToolResult{
					IsError: true,
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
				}, zero, nil
			}
			// Returning a nil result lets the SDK populate structured + text
			// content from the typed Out value automatically.
			return nil, out, nil
		})
	r.count++
}

// Serve starts the HTTP MCP endpoint for the registered tools. It errors if no
// tools were registered or if called twice.
func (r *Registry) Serve() (*Server, error) {
	if r.served {
		return nil, fmt.Errorf("registry already served")
	}
	if r.count == 0 {
		return nil, fmt.Errorf("no tools registered")
	}
	r.served = true
	return startServer(r.name, r.srv)
}
