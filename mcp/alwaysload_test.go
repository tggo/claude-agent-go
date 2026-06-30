package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAlwaysLoadField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := WriteConfig(path, map[string]ServerConfig{
		"kb":   {Type: "http", URL: "http://x", AlwaysLoad: true},
		"tool": {Type: "http", URL: "http://y"}, // AlwaysLoad false -> omitted
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var m map[string]map[string]map[string]any
	_ = json.Unmarshal(b, &m)
	if m["mcpServers"]["kb"]["alwaysLoad"] != true {
		t.Errorf("kb alwaysLoad = %v", m["mcpServers"]["kb"]["alwaysLoad"])
	}
	if _, present := m["mcpServers"]["tool"]["alwaysLoad"]; present {
		t.Error("alwaysLoad should be omitted when false")
	}
}
