// Package tools lets a Go program expose its own functions to the Claude agent
// as MCP tools, in-process. It stands up a local HTTP MCP server (via the
// official github.com/modelcontextprotocol/go-sdk) bound to 127.0.0.1 on a
// random port, and hands back an mcp.ServerConfig the runner can pass to the
// CLI via --mcp-config. The real `claude` binary connects back over HTTP and
// can call your handlers — no separate process, no reimplemented agent loop.
//
// This is the genuinely-working analogue of the Python SDK's
// create_sdk_mcp_server / @tool. (The Python SDK tunnels tools over the CLI
// control protocol; an in-process HTTP server is a simpler transport the CLI
// supports natively and which we can test against the real binary.)
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tggo/claude-agent-go/mcp"
)

// Handler implements a tool. args is the raw JSON arguments object sent by the
// agent; return the textual result (or an error, surfaced to the agent as a
// tool error).
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool describes one callable tool.
type Tool struct {
	// Name is the tool identifier the agent calls. Required.
	Name string
	// Description tells the model when/how to use the tool. Required.
	Description string
	// InputSchema is a JSON Schema object for the arguments. If nil, an
	// permissive empty object schema ({"type":"object"}) is used.
	InputSchema json.RawMessage
	// Handler runs the tool. Required.
	Handler Handler
}

// Server is a running in-process MCP tool server.
type Server struct {
	name    string
	httpSrv *http.Server
	lis     net.Listener
	url     string

	mu     sync.Mutex
	closed bool
}

// emptyObjectSchema is the permissive default for tools that validate their own
// arguments.
var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

// Serve builds an MCP server named serverName exposing the given tools and
// starts listening on 127.0.0.1:<random>. Call Config() to wire it into a run
// and Close() when done. Returns an error if no tools are given, a tool is
// malformed, or the listener cannot be opened.
func Serve(serverName string, tools ...Tool) (*Server, error) {
	if serverName == "" {
		return nil, fmt.Errorf("server name is required")
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("at least one tool is required")
	}

	impl := &mcpsdk.Implementation{Name: serverName, Version: "0.1.0"}
	mcpSrv := mcpsdk.NewServer(impl, nil)

	for _, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		if t.Handler == nil {
			return nil, fmt.Errorf("tool %q has nil handler", t.Name)
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = emptyObjectSchema
		}
		h := t.Handler // capture
		mcpSrv.AddTool(
			&mcpsdk.Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			},
			func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				text, err := h(ctx, req.Params.Arguments)
				if err != nil {
					return &mcpsdk.CallToolResult{
						IsError: true,
						Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
					}, nil
				}
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
				}, nil
			},
		)
	}

	return startServer(serverName, mcpSrv)
}

// startServer binds a local HTTP MCP endpoint for an already-built mcp server
// and starts serving. Shared by Serve (untyped tools) and Registry (typed).
func startServer(serverName string, mcpSrv *mcpsdk.Server) (*Server, error) {
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return mcpSrv },
		nil,
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	s := &Server{
		name:    serverName,
		lis:     lis,
		httpSrv: &http.Server{Handler: handler},
		url:     fmt.Sprintf("http://%s/", lis.Addr().String()),
	}

	go func() {
		// Serve returns http.ErrServerClosed on Close; ignore it.
		_ = s.httpSrv.Serve(lis)
	}()

	return s, nil
}

// URL is the base URL the CLI connects to (on 127.0.0.1).
func (s *Server) URL() string { return s.url }

// Name is the server name (the key under which it is registered).
func (s *Server) Name() string { return s.name }

// Port is the TCP port the server listens on.
func (s *Server) Port() int { return s.lis.Addr().(*net.TCPAddr).Port }

// Config returns the mcp.ServerConfig for this server (reachable on 127.0.0.1),
// keyed by its name — for a local agent.
func (s *Server) Config() (string, mcp.ServerConfig) {
	return s.name, mcp.ServerConfig{Type: "http", URL: s.url}
}

// ConfigForHost returns the mcp.ServerConfig with the URL rewritten to reach the
// server from a REMOTE agent (container or ssh host) that can't use the host's
// 127.0.0.1. Use with:
//   - Docker: host = "host.docker.internal" (add the host-gateway mapping to the
//     transport, e.g. transport.DockerRun{Options: tools.DockerHostGateway()}).
//   - SSH: set up a reverse tunnel so the remote's 127.0.0.1:port maps back to
//     the host (transport.SSH{Options: tools.SSHReverseTunnel(s.Port())}), then
//     host = "127.0.0.1".
func (s *Server) ConfigForHost(host string) (string, mcp.ServerConfig) {
	return s.name, mcp.ServerConfig{
		Type: "http",
		URL:  fmt.Sprintf("http://%s:%d/", host, s.Port()),
	}
}

// Close stops the HTTP server. Safe to call multiple times.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.httpSrv.Close()
}
