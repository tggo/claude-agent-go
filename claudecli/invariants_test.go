package claudecli

import (
	"strings"
	"testing"
)

func evs(t *testing.T, lines ...string) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for _, l := range lines {
		ev := ParseStreamLine([]byte(l))
		if ev == nil {
			t.Fatalf("bad line %s", l)
		}
		out = append(out, *ev)
	}
	return out
}

const (
	lInit   = `{"type":"system","subtype":"init","session_id":"s1"}`
	lUse1   = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"a","input":{}}]}}`
	lUse2   = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t2","name":"b","input":{}}]}}`
	lRes1   = `{"type":"user","session_id":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	lRes2   = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}}`
	lPrompt = `{"type":"user","message":{"role":"user","content":"hello"}}`
	lDone   = `{"type":"result","subtype":"success","session_id":"s1","result":"fin"}`
)

func TestCheckInvariants(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		rules []string
	}{
		{"empty", nil, nil},
		{"clean", []string{lInit, lPrompt, lUse1, lRes1, lUse2, lRes2, lDone}, nil},
		{"no result event is fine", []string{lInit, lUse1, lRes1}, nil},
		{"unmatched use", []string{lInit, lUse1, lDone}, []string{RuleUnmatchedToolUse}},
		{"orphan result", []string{lInit, lRes1, lDone}, []string{RuleOrphanToolResult}},
		{"duplicate result", []string{lInit, lUse1, lRes1, lRes1, lDone}, []string{RuleDuplicateToolResult}},
		{"duplicate use", []string{lInit, lUse1, lUse1, lRes1, lDone}, []string{RuleDuplicateToolUse}},
		{"two results", []string{lInit, lDone, lDone}, []string{RuleMultipleResults}},
		{"event after result", []string{lInit, lDone, lUse1}, []string{RuleEventAfterResult, RuleUnmatchedToolUse}},
		{"session mismatch", []string{lInit, strings.Replace(lUse1, `"s1"`, `"zz"`, 1), lRes1, lDone}, []string{RuleSessionMismatch}},
		{"sub-agent known parent", []string{lInit, lUse1,
			`{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"text","text":"sub"}]}}`, lRes1, lDone}, nil},
		{"sub-agent unknown parent", []string{lInit,
			`{"type":"assistant","parent_tool_use_id":"nope","message":{"content":[{"type":"text","text":"sub"}]}}`, lDone}, []string{RuleUnknownParent}},
		{"garbage message tolerated", []string{`{"type":"assistant","message":"str"}`, `{"type":"user","message":{"content":{"x":1}}}`, `{"type":"user"}`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := CheckInvariants(evs(t, tc.lines...))
			var got []string
			for _, v := range vs {
				got = append(got, v.Rule)
				if v.Detail == "" || v.String() == "" {
					t.Errorf("empty detail: %+v", v)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.rules, ",") {
				t.Errorf("rules = %v, want %v (%v)", got, tc.rules, vs)
			}
		})
	}
}

func TestCheckInvariantsOrdered(t *testing.T) {
	vs := CheckInvariants(evs(t, lInit, lUse1, lRes2, lDone, lDone))
	for i := 1; i < len(vs); i++ {
		if vs[i].Index < vs[i-1].Index {
			t.Fatalf("not sorted: %v", vs)
		}
	}
	if len(vs) != 3 {
		t.Fatalf("got %v", vs)
	}
}
