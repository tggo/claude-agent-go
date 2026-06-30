// Package mcp writes Claude CLI MCP config files (the JSON passed via
// --mcp-config). It is content-agnostic: the caller supplies whatever servers
// it wants exposed to the agent.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
)

// ServerConfig describes one MCP server entry. Type is typically "http",
// "sse", or "stdio"; the relevant subset of the remaining fields applies per
// type. Unset fields are omitted from the JSON.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// stdio transport
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// configFile is the on-disk shape Claude CLI expects.
type configFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// HTTPServer is a convenience constructor for a bearer-authenticated HTTP MCP
// server (the common case: a remote knowledge-base endpoint).
func HTTPServer(url, bearerToken string) ServerConfig {
	s := ServerConfig{Type: "http", URL: url}
	if bearerToken != "" {
		s.Headers = map[string]string{"Authorization": "Bearer " + bearerToken}
	}
	return s
}

// StdioServer is a convenience constructor for a local stdio MCP server.
func StdioServer(command string, args ...string) ServerConfig {
	return ServerConfig{Type: "stdio", Command: command, Args: args}
}

// WriteConfig marshals the given servers and writes them to path (0600).
// Returns an error if servers is empty so callers don't silently produce a
// config with no servers. path should usually live in a per-session dir
// (see workspace.Workspace.SessionDir).
func WriteConfig(path string, servers map[string]ServerConfig) error {
	if len(servers) == 0 {
		return fmt.Errorf("no MCP servers provided")
	}
	b, err := json.MarshalIndent(configFile{MCPServers: servers}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal MCP config: %w", err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("write MCP config %q: %w", path, err)
	}
	return nil
}
