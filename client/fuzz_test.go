package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// go test -run '^$' -fuzz '^FuzzHandleControlRequest$' -fuzztime 30s ./client

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) Close() error                { return nil }
func (s *syncBuf) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(strings.TrimRight(s.b.String(), "\n"), "\n")
}

func fuzzClient(variant int) (*Client, *syncBuf) {
	out := &syncBuf{}
	cfg := Config{CallbackTimeout: 2 * time.Second}
	c := &Client{stdin: out, hookCallbacks: map[string]HookCallback{}}
	switch variant % 5 {
	case 0: // permission callback allowing everything
		cfg.CanUseTool = func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
			return Allow(), nil
		}
	case 1: // deny / error / panic mix keyed on the tool name
		cfg.CanUseTool = func(_ context.Context, tool string, _ json.RawMessage, _ PermissionContext) (PermissionResult, error) {
			switch len(tool) % 3 {
			case 0:
				return DenyAndInterrupt("no"), nil
			case 1:
				return PermissionResult{}, fmt.Errorf("boom %s", tool)
			}
			panic("callback panic " + tool)
		}
	case 2: // AskUser only
		cfg.OnAskUser = func(_ context.Context, qs []Question) (map[string]string, error) {
			a := map[string]string{}
			for _, q := range qs {
				a[q.Question] = "x"
				if len(q.Options) > 0 {
					a[q.Question] = q.Options[0].Label
				}
			}
			return a, nil
		}
	case 3: // AskUser that panics + deny-other-tools
		cfg.OnAskUser = func(context.Context, []Question) (map[string]string, error) { panic("ask panic") }
		cfg.denyOtherTools = true
	default: // nothing configured
	}
	c.cfg = cfg
	c.hookCallbacks["hook_1"] = func(_ context.Context, in json.RawMessage, _ string) (json.RawMessage, error) {
		if len(in)%2 == 0 {
			return json.RawMessage(`{"decision":"block"}`), nil
		}
		return json.RawMessage(`not an object`), nil
	}
	c.hookCallbacks["hook_panic"] = func(context.Context, json.RawMessage, string) (json.RawMessage, error) { panic("hook") }
	return c, out
}

func FuzzHandleControlRequest(f *testing.F) {
	for i, s := range []string{
		`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu1"}}`,
		`{"type":"control_request","request_id":"r2","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which?","header":"H","options":[{"label":"A"},{"label":"B","description":"d"}],"multiSelect":false}]}}}`,
		`{"type":"control_request","request_id":"r3","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":"oops"}}}`,
		`{"type":"control_request","request_id":"r4","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":[1,2]}}`,
		`{"type":"control_request","request_id":"r5","request":{"subtype":"hook_callback","callback_id":"hook_1","input":{"tool_name":"Bash"},"tool_use_id":"t"}}`,
		`{"type":"control_request","request_id":"r6","request":{"subtype":"hook_callback","callback_id":"nope"}}`,
		`{"type":"control_request","request_id":"r7","request":{"subtype":"hook_callback","callback_id":"hook_panic"}}`,
		`{"type":"control_request","request_id":"r8","request":{"subtype":"mcp_message"}}`,
		`{"type":"control_request","request_id":"","request":{}}`,
		`{"request_id":"\u0000é\"","request":{"subtype":"can_use_tool","input":null}}`,
		`not json`, `[]`, `{"request":"str"}`, `{"request_id":5}`, ``,
	} {
		for v := 0; v < 5; v++ {
			f.Add(s, uint8(v+i))
		}
	}
	f.Fuzz(func(t *testing.T, line string, variant uint8) {
		c, out := fuzzClient(int(variant))
		var probe incomingControlRequest
		parses := json.Unmarshal([]byte(line), &probe) == nil
		c.handleControlRequest([]byte(line))
		if !parses {
			if got := out.b.String(); got != "" {
				t.Fatalf("response written for unparseable request: %q", got)
			}
			return
		}
		ls := out.lines()
		if len(ls) != 1 || ls[0] == "" {
			t.Fatalf("want exactly one response, got %d: %q", len(ls), ls)
		}
		var resp struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(ls[0]), &resp); err != nil {
			t.Fatalf("response is not valid JSON: %v: %q", err, ls[0])
		}
		if resp.Type != "control_response" || resp.Response.RequestID != probe.RequestID {
			t.Fatalf("bad envelope %q for request_id %q", ls[0], probe.RequestID)
		}
		switch resp.Response.Subtype {
		case "success":
			if !json.Valid(resp.Response.Response) {
				t.Fatalf("success without a JSON payload: %q", ls[0])
			}
		case "error":
			if resp.Response.Error == "" {
				t.Fatalf("error response without message: %q", ls[0])
			}
		default:
			t.Fatalf("unexpected subtype in %q", ls[0])
		}
		// A can_use_tool answer, when successful, is an explicit allow/deny.
		if probe.Request.Subtype == "can_use_tool" && resp.Response.Subtype == "success" {
			var d struct {
				Behavior string `json:"behavior"`
			}
			_ = json.Unmarshal(resp.Response.Response, &d)
			if d.Behavior != "allow" && d.Behavior != "deny" {
				t.Fatalf("can_use_tool answered without allow/deny: %q", ls[0])
			}
		}
	})
}
