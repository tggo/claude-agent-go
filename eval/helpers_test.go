package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeClaude writes a /bin/sh "claude" that prints script's output.
func fakeClaude(t *testing.T, shell string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+shell+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func catClaude(t *testing.T, body string) string {
	return fakeClaude(t, "cat <<'EOF'\n"+body+"\nEOF")
}

// A realistic stream: init, text+tool_use (same message id repeated per block),
// tool_result, a parallel pair, final text and result.
const fixtureStream = `{"type":"system","subtype":"init","session_id":"sess-1","model":"haiku"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Let me run it."}],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":100}}}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"echo hi"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":100}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"hi","is_error":false}]}}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"tu_2","name":"Read","input":{"file_path":"/a"}},{"type":"tool_use","id":"tu_3","name":"Read","input":{"file_path":"/b"}}],"usage":{"input_tokens":5,"output_tokens":7}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_3","content":"boom","is_error":true}]}}
{"type":"user","message":{"role":"user","content":"plain string echo"}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"unknown","content":"x"}]}}
{"type":"assistant","message":{"id":"msg_3","role":"assistant","content":[{"type":"text","text":"The output is hi."}],"usage":{"input_tokens":3,"output_tokens":4}}}
{"type":"result","subtype":"success","session_id":"sess-1","result":"The output is hi.","total_cost_usd":0.0123,"num_turns":3,"duration_ms":4000}`

func lines(s string) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, []byte(s[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
