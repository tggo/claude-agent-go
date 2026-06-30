package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPermissionResultResponseData(t *testing.T) {
	orig := json.RawMessage(`{"path":"x"}`)

	// allow keeps original input when none provided
	d := Allow().toResponseData(orig)
	if d["behavior"] != "allow" {
		t.Errorf("behavior = %v", d["behavior"])
	}
	if string(d["updatedInput"].(json.RawMessage)) != `{"path":"x"}` {
		t.Errorf("updatedInput = %v", d["updatedInput"])
	}

	// allow with replacement input
	d = AllowWithInput(json.RawMessage(`{"path":"y"}`)).toResponseData(orig)
	if string(d["updatedInput"].(json.RawMessage)) != `{"path":"y"}` {
		t.Errorf("updatedInput = %v", d["updatedInput"])
	}

	// deny carries message
	d = Deny("nope").toResponseData(orig)
	if d["behavior"] != "deny" || d["message"] != "nope" {
		t.Errorf("deny data = %v", d)
	}
	if _, ok := d["interrupt"]; ok {
		t.Error("plain deny should not set interrupt")
	}

	// deny + interrupt
	d = DenyAndInterrupt("stop").toResponseData(orig)
	if d["interrupt"] != true {
		t.Errorf("interrupt not set: %v", d)
	}
}

func TestDispatchCanUseTool(t *testing.T) {
	var gotTool string
	c := &Client{cfg: Config{
		CanUseTool: func(_ context.Context, tool string, input json.RawMessage, pctx PermissionContext) (PermissionResult, error) {
			gotTool = tool
			if pctx.ToolUseID != "tu1" {
				t.Errorf("tool_use_id = %q", pctx.ToolUseID)
			}
			return Deny("no"), nil
		},
	}}

	var req incomingControlRequest
	_ = json.Unmarshal([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"cmd":"ls"},"tool_use_id":"tu1"}}`), &req)

	data, err := c.dispatchControl(req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotTool != "Bash" || data["behavior"] != "deny" {
		t.Errorf("unexpected: tool=%q data=%v", gotTool, data)
	}
}

func TestDispatchCanUseToolNoCallback(t *testing.T) {
	c := &Client{cfg: Config{}}
	var req incomingControlRequest
	req.Request.Subtype = "can_use_tool"
	if _, err := c.dispatchControl(req); err == nil {
		t.Error("expected error when no CanUseTool configured")
	}
}

func TestDispatchCanUseToolCallbackError(t *testing.T) {
	c := &Client{cfg: Config{
		CanUseTool: func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
			return PermissionResult{}, errors.New("boom")
		},
	}}
	var req incomingControlRequest
	req.Request.Subtype = "can_use_tool"
	if _, err := c.dispatchControl(req); err == nil {
		t.Error("expected propagated callback error")
	}
}

func TestDispatchHookCallback(t *testing.T) {
	c := &Client{
		cfg:           Config{},
		hookCallbacks: map[string]HookCallback{},
	}
	c.hookCallbacks["hook_1"] = func(_ context.Context, input json.RawMessage, tuid string) (json.RawMessage, error) {
		return json.RawMessage(`{"decision":"block","reason":"policy"}`), nil
	}

	var req incomingControlRequest
	req.Request.Subtype = "hook_callback"
	req.Request.CallbackID = "hook_1"
	data, err := c.dispatchControl(req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if data["decision"] != "block" || data["reason"] != "policy" {
		t.Errorf("hook data = %v", data)
	}
}

func TestDispatchHookUnknownAndUnsupported(t *testing.T) {
	c := &Client{cfg: Config{}, hookCallbacks: map[string]HookCallback{}}

	var hookReq incomingControlRequest
	hookReq.Request.Subtype = "hook_callback"
	hookReq.Request.CallbackID = "missing"
	if _, err := c.dispatchControl(hookReq); err == nil {
		t.Error("expected error for unknown hook id")
	}

	var unk incomingControlRequest
	unk.Request.Subtype = "something_else"
	if _, err := c.dispatchControl(unk); err == nil {
		t.Error("expected error for unsupported subtype")
	}
}

func TestRawToMap(t *testing.T) {
	if m := rawToMap(nil); len(m) != 0 {
		t.Errorf("nil -> %v", m)
	}
	if m := rawToMap(json.RawMessage(`not json`)); len(m) != 0 {
		t.Errorf("invalid -> %v", m)
	}
	if m := rawToMap(json.RawMessage(`{"a":1}`)); m["a"] != float64(1) {
		t.Errorf("valid -> %v", m)
	}
}

func TestBuildHooksConfig(t *testing.T) {
	c := &Client{
		hookCallbacks: map[string]HookCallback{},
		cfg: Config{Hooks: map[string][]HookMatcher{
			"PreToolUse": {{
				Matcher: "Bash",
				Callbacks: []HookCallback{
					func(context.Context, json.RawMessage, string) (json.RawMessage, error) { return nil, nil },
				},
			}},
		}},
	}
	cfg := c.buildHooksConfig()
	if cfg == nil {
		t.Fatal("expected non-nil hooks config")
	}
	pre, ok := cfg["PreToolUse"].([]map[string]any)
	if !ok || len(pre) != 1 {
		t.Fatalf("PreToolUse entries = %v", cfg["PreToolUse"])
	}
	ids, _ := pre[0]["hookCallbackIds"].([]string)
	if len(ids) != 1 {
		t.Fatalf("callback ids = %v", pre[0]["hookCallbackIds"])
	}
	if _, registered := c.hookCallbacks[ids[0]]; !registered {
		t.Errorf("callback %q not registered", ids[0])
	}

	// no hooks -> nil
	empty := &Client{cfg: Config{}}
	if empty.buildHooksConfig() != nil {
		t.Error("expected nil for empty hooks")
	}
}

func TestCanUseToolArgsAndDefaults(t *testing.T) {
	// applyClientDefaults must drop skip-permissions and default the mode.
	cfg := Config{CanUseTool: func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
		return Allow(), nil
	}}
	applyClientDefaults(&cfg)
	if cfg.SkipPermissions {
		t.Error("CanUseTool should disable SkipPermissions")
	}
	if cfg.PermissionMode != "default" {
		t.Errorf("PermissionMode = %q, want default", cfg.PermissionMode)
	}

	line := " " + join(buildClientArgs(cfg)) + " "
	if !contains(line, "--permission-prompt-tool stdio") {
		t.Errorf("missing permission-prompt-tool flag: %s", line)
	}
	if contains(line, "--dangerously-skip-permissions") {
		t.Errorf("should not skip permissions: %s", line)
	}
}

// bufCloser is an in-memory io.WriteCloser for capturing stdin writes.
type bufCloser struct{ data []byte }

func (b *bufCloser) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }
func (b *bufCloser) Close() error                { return nil }

func TestHandleControlRequestWritesResponse(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{
		stdin:         buf,
		hookCallbacks: map[string]HookCallback{},
		cfg: Config{
			CanUseTool: func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
				return Allow(), nil
			},
		},
	}
	c.handleControlRequest([]byte(`{"type":"control_request","request_id":"r9","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"path":"a"},"tool_use_id":"t"}}`))

	var out outgoingControlResponse
	if err := json.Unmarshal(buf.data, &out); err != nil {
		t.Fatalf("response not valid json: %v (%s)", err, buf.data)
	}
	if out.Type != "control_response" || out.Response["subtype"] != "success" || out.Response["request_id"] != "r9" {
		t.Errorf("bad response: %+v", out.Response)
	}
	inner, _ := out.Response["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Errorf("inner = %v", inner)
	}
}

func TestHandleControlRequestErrorResponse(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{stdin: buf, hookCallbacks: map[string]HookCallback{}, cfg: Config{}}
	// can_use_tool with no callback -> error response
	c.handleControlRequest([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`))

	var out outgoingControlResponse
	if err := json.Unmarshal(buf.data, &out); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if out.Response["subtype"] != "error" || out.Response["error"] == "" {
		t.Errorf("expected error response, got %+v", out.Response)
	}
}

func TestHandleControlRequestInvalidJSON(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{stdin: buf, cfg: Config{}}
	c.handleControlRequest([]byte(`not json`))
	if len(buf.data) != 0 {
		t.Errorf("invalid request should produce no response, got %s", buf.data)
	}
}

// helpers (local to avoid extra deps)
func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

func contains(h, n string) bool {
	return len(n) == 0 || (len(h) >= len(n) && indexOf(h, n) >= 0)
}

func indexOf(s, sub string) int {
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
