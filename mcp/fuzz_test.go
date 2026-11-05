package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf8"
)

// go test -run '^$' -fuzz '^FuzzWriteConfig$' -fuzztime 30s ./mcp

func FuzzWriteConfig(f *testing.F) {
	f.Add("pizza", "http://127.0.0.1:8080/mcp", "tok", "Authorization", "node", "--flag", "K", "V", true)
	f.Add("a b/c\"d", "https://x/ ?q=\"\\", "", "X-\x00", "", "", "", "", false)
	f.Add("", "", "tok\n\"}", "", "\xff\xfe", "-c", "PATH", "\u0000", true)
	f.Add("é", "http://[::1]:1/", "bearer with space", "H", "/bin/sh", "-c", "k=v", "{}", false)
	path := filepath.Join(f.TempDir(), "mcp.json")
	f.Fuzz(func(t *testing.T, name, url, token, hdrKey, cmd, arg, envKey, envVal string, always bool) {
		http := HTTPServer(url, token)
		http.AlwaysLoad = always
		if hdrKey != "" {
			if http.Headers == nil {
				http.Headers = map[string]string{}
			}
			http.Headers[hdrKey] = token
		}
		stdio := StdioServer(cmd, arg, name)
		stdio.Env = map[string]string{envKey: envVal}
		servers := map[string]ServerConfig{name: http, name + "#2": stdio}

		if err := WriteConfig(path, servers); err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(b) {
			t.Fatalf("invalid JSON written: %q", b)
		}
		var back configFile
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("unmarshal own output: %v", err)
		}
		// JSON replaces invalid UTF-8 with U+FFFD, so compare after one
		// normalizing marshal/unmarshal pass of the input.
		var want configFile
		nb, _ := json.Marshal(configFile{MCPServers: servers})
		if err := json.Unmarshal(nb, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, back) {
			t.Fatalf("round trip mismatch:\n want %+v\n  got %+v", want, back)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("config file mode = %v (err %v), want 0600", st.Mode(), err)
		}
		// Valid UTF-8 inputs must survive exactly.
		if validStrings(name, url, token, cmd, arg, envKey, envVal) {
			got := back.MCPServers[name]
			if got.URL != url || got.Type != "http" || got.AlwaysLoad != always {
				t.Fatalf("http server not preserved: %+v", got)
			}
			if token != "" && got.Headers["Authorization"] != "Bearer "+token && hdrKey != "Authorization" {
				t.Fatalf("bearer header lost: %+v", got.Headers)
			}
		}
	})
}

func validStrings(ss ...string) bool {
	for _, s := range ss {
		if !utf8.ValidString(s) {
			return false
		}
	}
	return true
}
