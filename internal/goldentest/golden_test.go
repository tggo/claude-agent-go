package goldentest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
)

func rec(lines ...string) *recording.Recording {
	r := &recording.Recording{Header: recording.Header{Label: "l", Model: "m"}}
	for _, l := range lines {
		r.Lines = append(r.Lines, recording.Line{Raw: []byte(l)})
	}
	return r
}

func TestSummaryMasksAndFormats(t *testing.T) {
	r := rec(
		`{"type":"system","subtype":"init","session_id":"b9e38571-cf4b-4490-a3fd-94f4279739b4"}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hello   there"},{"type":"tool_use","id":"toolu_AA","name":"n","input":{"b":1,"a":"cart-0123456789abcdef0123"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_AA","content":"took 1.52s ok"},{"type":"tool_result","tool_use_id":"toolu_BB","is_error":true,"content":[{"type":"text","text":"x"},{"type":"text","text":"y"}]}]}}`,
		`{"type":"user","parent_tool_use_id":"toolu_AA","message":{"content":"prompt"}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_AA","message":{"content":[{"type":"text","text":"sub"}]}}`,
		`{"type":"result","subtype":"success","num_turns":2,"total_cost_usd":0.5,"result":"`+strings.Repeat("z", 50)+`","session_id":"s"}`,
	)
	got := Summary(r, Options{MaxLen: 40})
	for _, want := range []string{
		`run label="l" model="m"`,
		"say: hello there",
		`call <call#1> n {"a":"cart-<id>","b":1}`,
		"return <call#1> ok: took <secs> ok",
		"return <call#2> error: x y",
		"say (sub-agent): sub",
		"result subtype=success is_error=false turns=2 cost=<cost>: " + strings.Repeat("z", 40) + "…",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "b9e38571") || strings.Contains(got, "toolu_") {
		t.Errorf("unmasked ids:\n%s", got)
	}
	if blocks(&claudecli.StreamEvent{Message: []byte(`"x"`)}) != nil || canon([]byte(`{`)) != "{" || resultText([]byte(`5`)) != "5" {
		t.Error("fallbacks")
	}
	if Summary(rec(), Options{}) == "" {
		t.Error("header expected")
	}
}

func TestCompare(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.golden")
	Compare(t, p, "a\n", true)
	Compare(t, p, "a\n", false)
	ft := &fakeT{}
	Compare(ft, p, "b\n", false)
	if !ft.errored {
		t.Error("mismatch must fail")
	}
	ft = &fakeT{}
	func() {
		defer func() { _ = recover() }()
		Compare(ft, filepath.Join(t.TempDir(), "missing"), "x", false)
	}()
	if !ft.fataled {
		t.Error("missing golden must be fatal")
	}
}

type fakeT struct {
	testing.TB
	errored, fataled bool
}

func (f *fakeT) Helper()               {}
func (f *fakeT) Logf(string, ...any)   {}
func (f *fakeT) Errorf(string, ...any) { f.errored = true }
func (f *fakeT) Fatalf(string, ...any) { f.fataled = true; panic("fatal") }
func (f *fakeT) Fatal(...any)          { f.fataled = true; panic("fatal") }
