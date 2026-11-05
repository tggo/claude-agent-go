// Package goldentest renders a recorded run as a short, masked, diff-friendly
// text summary and compares it with a golden file — the pattern of Google
// ADK's agent_test_runner: run (or replay) once, check in the result, fail on
// any behavioural drift, refresh with an -update flag.
package goldentest

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
)

// Options tunes Summary.
type Options struct {
	// MaxLen truncates every args/result/text field to this many runes
	// (0 means 200).
	MaxLen int
}

var (
	uuidRe   = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	toolIDRe = regexp.MustCompile(`toolu_[A-Za-z0-9]+`)
	msgIDRe  = regexp.MustCompile(`msg_[A-Za-z0-9]+`)
	cartRe   = regexp.MustCompile(`cart-[0-9a-f]{16,}`)
	secsRe   = regexp.MustCompile(`\b\d+(\.\d+)?s\b`)
)

// Summary renders rec as one line per tool call, tool result, assistant text
// and the final result. Volatile values are masked: session ids and uuids,
// tool_use/message ids (replaced by their order of appearance), cart ids,
// durations and costs.
func Summary(rec *recording.Recording, o Options) string {
	if o.MaxLen == 0 {
		o.MaxLen = 200
	}
	ids := map[string]int{}
	idOf := func(id string) string {
		if _, ok := ids[id]; !ok {
			ids[id] = len(ids) + 1
		}
		return fmt.Sprintf("<call#%d>", ids[id])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "run label=%q model=%q\n", rec.Header.Label, rec.Header.Model)
	for _, ev := range rec.Events() {
		switch {
		case ev.IsResult():
			fmt.Fprintf(&b, "result subtype=%s is_error=%t turns=%d cost=<cost>: %s\n",
				ev.Subtype, ev.IsError, ev.NumTurns, trunc(ev.Result, o.MaxLen))
		case ev.Type == "assistant" || ev.Type == "user":
			sub := ""
			if ev.IsSubagent() {
				sub = " (sub-agent)"
			}
			for _, bl := range blocks(&ev) {
				switch bl.Type {
				case claudecli.BlockText:
					if ev.Type == "assistant" {
						fmt.Fprintf(&b, "say%s: %s\n", sub, trunc(bl.Text, o.MaxLen))
					}
				case claudecli.BlockToolUse:
					fmt.Fprintf(&b, "call%s %s %s %s\n", sub, idOf(bl.ID), bl.Name, trunc(canon(bl.Input), o.MaxLen))
				case claudecli.BlockToolResult:
					status := "ok"
					if bl.IsError {
						status = "error"
					}
					fmt.Fprintf(&b, "return%s %s %s: %s\n", sub, idOf(bl.ToolUseID), status, trunc(resultText(bl.Content), o.MaxLen))
				}
			}
		}
	}
	return mask(b.String())
}

func blocks(ev *claudecli.StreamEvent) []claudecli.ContentBlock {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	var out []claudecli.ContentBlock
	if json.Unmarshal(ev.Message, &m) == nil {
		_ = json.Unmarshal(m.Content, &out)
	}
	return out
}

// canon re-encodes JSON with sorted keys and no insignificant space.
func canon(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// resultText flattens a tool_result content (string or text blocks).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &bs) == nil {
		var parts []string
		for _, b := range bs {
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func mask(s string) string {
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = toolIDRe.ReplaceAllString(s, "<tool_use_id>")
	s = msgIDRe.ReplaceAllString(s, "<msg_id>")
	s = cartRe.ReplaceAllString(s, "cart-<id>")
	return secsRe.ReplaceAllString(s, "<secs>")
}

// Compare checks got against the golden file at path. With update set it
// rewrites the file instead (and logs that it did).
func Compare(t testing.TB, path, got string, update bool) {
	t.Helper()
	if update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(want) != got {
		t.Errorf("summary differs from %s (rerun with -update if intended)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
