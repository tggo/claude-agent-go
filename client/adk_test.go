package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ADK #3645: malformed HITL input or a misbehaving callback must be answered
// with a deny (never a hang or a crash).
func TestADK3645AskUserRobustness(t *testing.T) {
	ok := func(context.Context, []Question) (map[string]string, error) {
		return map[string]string{"Color?": "teal"}, nil
	}
	cases := []struct {
		name     string
		input    string
		fn       AskUserFunc
		behavior string
		msgHas   string
	}{
		{"not an object", `[1,2]`, ok, "deny", "malformed"},
		{"questions not array", `{"questions":"x"}`, ok, "deny", "malformed"},
		{"questions is null", `{"questions":null}`, ok, "allow", ""},
		{"options not array", `{"questions":[{"question":"Q","options":"a,b"}]}`, ok, "deny", "malformed"},
		{"option not object", `{"questions":[{"question":"Q","options":["a"]}]}`, ok, "deny", "malformed"},
		{"missing questions", `{}`, ok, "allow", ""},
		{"empty questions", `{"questions":[]}`, ok, "allow", ""},
		{"empty answers", askInput, func(context.Context, []Question) (map[string]string, error) { return map[string]string{}, nil }, "allow", ""},
		{"nil answers", askInput, func(context.Context, []Question) (map[string]string, error) { return nil, nil }, "allow", ""},
		{"callback error", askInput, func(context.Context, []Question) (map[string]string, error) { return nil, errors.New("nope") }, "deny", "nope"},
		{"callback panic", askInput, func(context.Context, []Question) (map[string]string, error) { panic("boom") }, "deny", "panicked"},
		{"callback nil-map write panic", askInput, func(context.Context, []Question) (map[string]string, error) {
			var m map[string]string
			m["x"] = "y"
			return m, nil
		}, "deny", "panicked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bufCloser{}
			c := &Client{stdin: buf, cfg: Config{OnAskUser: tc.fn}}
			c.handleControlRequest([]byte(`{"type":"control_request","request_id":"r","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":` + tc.input + `}}`))
			var out outgoingControlResponse
			if err := json.Unmarshal(buf.data, &out); err != nil {
				t.Fatalf("no reply written: %q (%v)", buf.data, err)
			}
			if out.Response["request_id"] != "r" || out.Response["subtype"] != "success" {
				t.Fatalf("response = %v", out.Response)
			}
			inner := out.Response["response"].(map[string]any)
			if inner["behavior"] != tc.behavior {
				t.Fatalf("behavior = %v, want %s (%v)", inner["behavior"], tc.behavior, inner)
			}
			if tc.msgHas != "" && !strings.Contains(inner["message"].(string), tc.msgHas) {
				t.Errorf("message = %v, want %q", inner["message"], tc.msgHas)
			}
		})
	}
}

func TestADK3645CanUseToolPanicDenies(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{stdin: buf, cfg: Config{CanUseTool: func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
		panic("kaboom")
	}}}
	c.handleControlRequest([]byte(`{"type":"control_request","request_id":"r","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`))
	var out outgoingControlResponse
	if err := json.Unmarshal(buf.data, &out); err != nil {
		t.Fatalf("no reply: %q", buf.data)
	}
	inner := out.Response["response"].(map[string]any)
	if inner["behavior"] != "deny" || !strings.Contains(inner["message"].(string), "panicked") {
		t.Errorf("response = %v", out.Response)
	}
}

func TestADK3645HookPanicReplies(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{stdin: buf, hookCallbacks: map[string]HookCallback{
		"h": func(context.Context, json.RawMessage, string) (json.RawMessage, error) { panic("x") },
	}}
	c.handleControlRequest([]byte(`{"type":"control_request","request_id":"r","request":{"subtype":"hook_callback","callback_id":"h","input":{}}}`))
	var out outgoingControlResponse
	if err := json.Unmarshal(buf.data, &out); err != nil || out.Response["subtype"] != "error" {
		t.Errorf("reply = %q err=%v", buf.data, err)
	}
}
