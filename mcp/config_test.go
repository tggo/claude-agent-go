package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHTTPServer(t *testing.T) {
	t.Run("with token sets type, url and bearer header", func(t *testing.T) {
		s := HTTPServer("https://example.com/mcp", "secret-token")
		if s.Type != "http" {
			t.Errorf("Type = %q, want http", s.Type)
		}
		if s.URL != "https://example.com/mcp" {
			t.Errorf("URL = %q", s.URL)
		}
		got := s.Headers["Authorization"]
		if got != "Bearer secret-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer secret-token")
		}
	})

	t.Run("empty token omits headers", func(t *testing.T) {
		s := HTTPServer("https://example.com/mcp", "")
		if s.Type != "http" {
			t.Errorf("Type = %q, want http", s.Type)
		}
		if s.Headers != nil {
			t.Errorf("Headers = %v, want nil", s.Headers)
		}
	})
}

func TestStdioServer(t *testing.T) {
	s := StdioServer("mycmd", "--flag", "value")
	if s.Type != "stdio" {
		t.Errorf("Type = %q, want stdio", s.Type)
	}
	if s.Command != "mycmd" {
		t.Errorf("Command = %q, want mycmd", s.Command)
	}
	if len(s.Args) != 2 || s.Args[0] != "--flag" || s.Args[1] != "value" {
		t.Errorf("Args = %v, want [--flag value]", s.Args)
	}
}

func TestStdioServerNoArgs(t *testing.T) {
	s := StdioServer("mycmd")
	if s.Type != "stdio" || s.Command != "mycmd" {
		t.Errorf("got %+v", s)
	}
	if len(s.Args) != 0 {
		t.Errorf("Args = %v, want empty", s.Args)
	}
}

func TestWriteConfigEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := WriteConfig(path, nil); err == nil {
		t.Fatal("WriteConfig with nil servers: expected error, got nil")
	}
	if err := WriteConfig(path, map[string]ServerConfig{}); err == nil {
		t.Fatal("WriteConfig with empty servers: expected error, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file written, stat err = %v", err)
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	servers := map[string]ServerConfig{
		"kb":    HTTPServer("https://kb.example.com", "tok"),
		"local": StdioServer("run-mcp", "--port", "1234"),
	}
	if err := WriteConfig(path, servers); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// File mode 0600 expected.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file mode = %o, want 600", perm)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	if _, ok := raw["mcpServers"]; !ok {
		t.Fatalf("missing top-level key mcpServers, got keys %v", raw)
	}

	var parsed struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}

	kb := parsed.MCPServers["kb"]
	if kb["type"] != "http" {
		t.Errorf("kb.type = %v, want http", kb["type"])
	}
	if kb["url"] != "https://kb.example.com" {
		t.Errorf("kb.url = %v", kb["url"])
	}
	headers, ok := kb["headers"].(map[string]any)
	if !ok {
		t.Fatalf("kb.headers not an object: %v", kb["headers"])
	}
	if headers["Authorization"] != "Bearer tok" {
		t.Errorf("kb Authorization = %v, want Bearer tok", headers["Authorization"])
	}

	local := parsed.MCPServers["local"]
	if local["type"] != "stdio" {
		t.Errorf("local.type = %v, want stdio", local["type"])
	}
	if local["command"] != "run-mcp" {
		t.Errorf("local.command = %v", local["command"])
	}
	args, ok := local["args"].([]any)
	if !ok || len(args) != 2 || args[0] != "--port" || args[1] != "1234" {
		t.Errorf("local.args = %v, want [--port 1234]", local["args"])
	}
}

func TestWriteConfigUnwritablePath(t *testing.T) {
	// A path whose parent is a file (not a dir) cannot be written.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := filepath.Join(notADir, "config.json")

	servers := map[string]ServerConfig{"kb": HTTPServer("https://x", "t")}
	if err := WriteConfig(path, servers); err == nil {
		t.Fatal("expected error writing under a file path, got nil")
	}
}
