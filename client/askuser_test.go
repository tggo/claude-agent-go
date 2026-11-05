package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const askInput = `{"questions":[{"question":"Color?","header":"Color","options":[{"label":"teal","description":"t"},{"label":"orange"}],"multiSelect":false}]}`

func askReq(tool, input string) incomingControlRequest {
	var req incomingControlRequest
	_ = json.Unmarshal([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"`+tool+`","input":`+input+`,"tool_use_id":"tu"}}`), &req)
	return req
}

func TestAskUserAnswersInUpdatedInput(t *testing.T) {
	var got []Question
	c := &Client{cfg: Config{OnAskUser: func(_ context.Context, qs []Question) (map[string]string, error) {
		got = qs
		return map[string]string{"Color?": "teal"}, nil
	}}}
	data, err := c.dispatchControl(askReq("AskUserQuestion", askInput))
	if err != nil {
		t.Fatal(err)
	}
	if data["behavior"] != "allow" {
		t.Fatalf("data = %v", data)
	}
	var upd struct {
		Questions []Question        `json:"questions"`
		Answers   map[string]string `json:"answers"`
	}
	if err := json.Unmarshal(data["updatedInput"].(json.RawMessage), &upd); err != nil {
		t.Fatal(err)
	}
	if upd.Answers["Color?"] != "teal" || len(upd.Questions) != 1 {
		t.Errorf("updatedInput = %+v", upd)
	}
	if len(got) != 1 || got[0].Header != "Color" || len(got[0].Options) != 2 || got[0].Options[0].Description != "t" || got[0].MultiSelect {
		t.Errorf("questions = %+v", got)
	}
}

func TestAskUserPrecedenceOverCanUseTool(t *testing.T) {
	var toolsSeen []string
	asked := false
	c := &Client{cfg: Config{
		CanUseTool: func(_ context.Context, tool string, _ json.RawMessage, _ PermissionContext) (PermissionResult, error) {
			toolsSeen = append(toolsSeen, tool)
			return Deny("no"), nil
		},
		OnAskUser: func(context.Context, []Question) (map[string]string, error) {
			asked = true
			return nil, nil
		},
	}}
	data, _ := c.dispatchControl(askReq("AskUserQuestion", askInput))
	if !asked || data["behavior"] != "allow" || len(toolsSeen) != 0 {
		t.Errorf("asked=%v data=%v toolsSeen=%v", asked, data, toolsSeen)
	}
	data, _ = c.dispatchControl(askReq("Bash", `{}`))
	if data["behavior"] != "deny" || len(toolsSeen) != 1 || toolsSeen[0] != "Bash" {
		t.Errorf("Bash: data=%v toolsSeen=%v", data, toolsSeen)
	}
}

func TestAskUserOnlyAllowsOtherTools(t *testing.T) {
	c := &Client{cfg: Config{OnAskUser: func(context.Context, []Question) (map[string]string, error) { return nil, nil }}}
	data, err := c.dispatchControl(askReq("Bash", `{"command":"ls"}`))
	if err != nil || data["behavior"] != "allow" {
		t.Errorf("data=%v err=%v", data, err)
	}
}

func TestAskUserDeniesOnErrorAndMalformed(t *testing.T) {
	c := &Client{cfg: Config{OnAskUser: func(context.Context, []Question) (map[string]string, error) {
		return nil, errors.New("user walked away")
	}}}
	data, _ := c.dispatchControl(askReq("AskUserQuestion", askInput))
	if data["behavior"] != "deny" || data["message"] != "user walked away" {
		t.Errorf("error case: %v", data)
	}
	for _, in := range []string{`"str"`, `{"questions":"x"}`} {
		data, _ = c.dispatchControl(askReq("AskUserQuestion", in))
		if data["behavior"] != "deny" || !strings.Contains(data["message"].(string), "malformed") {
			t.Errorf("input %s: %v", in, data)
		}
	}
	// no questions key: callback runs with none, empty answers
	c.cfg.OnAskUser = func(_ context.Context, qs []Question) (map[string]string, error) {
		if len(qs) != 0 {
			t.Errorf("qs = %v", qs)
		}
		return nil, nil
	}
	data, _ = c.dispatchControl(askReq("AskUserQuestion", `{}`))
	if data["behavior"] != "allow" || !strings.Contains(string(data["updatedInput"].(json.RawMessage)), `"answers":{}`) {
		t.Errorf("empty: %v", data)
	}
}

func TestAskUserTimeout(t *testing.T) {
	c := &Client{cfg: Config{
		CallbackTimeout: 30 * time.Millisecond,
		OnAskUser: func(ctx context.Context, _ []Question) (map[string]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}}
	_, err := c.dispatchControl(askReq("AskUserQuestion", askInput))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
}

func TestAskUserWritesControlResponse(t *testing.T) {
	buf := &bufCloser{}
	c := &Client{stdin: buf, cfg: Config{OnAskUser: func(context.Context, []Question) (map[string]string, error) {
		return map[string]string{"Color?": "orange"}, nil
	}}}
	c.handleControlRequest([]byte(`{"type":"control_request","request_id":"q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":` + askInput + `}}`))
	var out outgoingControlResponse
	if err := json.Unmarshal(buf.data, &out); err != nil {
		t.Fatal(err)
	}
	inner := out.Response["response"].(map[string]any)
	ui := inner["updatedInput"].(map[string]any)
	if inner["behavior"] != "allow" || ui["answers"].(map[string]any)["Color?"] != "orange" {
		t.Errorf("response = %v", out.Response)
	}
}

func TestOnAskUserEnablesPermissionRouting(t *testing.T) {
	cfg := Config{OnAskUser: func(context.Context, []Question) (map[string]string, error) { return nil, nil }}
	applyClientDefaults(&cfg)
	line := " " + join(buildClientArgs(cfg)) + " "
	if cfg.SkipPermissions || !contains(line, "--permission-prompt-tool stdio") || contains(line, "--dangerously-skip-permissions") {
		t.Errorf("args: %s", line)
	}
}

func TestAnswerFromStdin(t *testing.T) {
	qs := []Question{
		{Question: "Color?", Header: "H", Options: []QuestionOption{{Label: "teal", Description: "d"}, {Label: "orange"}}},
		{Question: "Toppings?", MultiSelect: true, Options: []QuestionOption{{Label: "a"}, {Label: "b"}, {Label: "c"}}},
		{Question: "Name?", Options: []QuestionOption{{Label: "x"}}},
		{Question: "Bad?", MultiSelect: true, Options: []QuestionOption{{Label: "a"}}},
		{Question: "Last?", Options: []QuestionOption{{Label: "p"}}},
	}
	var w bytes.Buffer
	f := AnswerFromStdin(strings.NewReader("2\n1, 3\nbob\n1,9\n1"), &w) // last line has no newline
	ans, err := f(context.Background(), qs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Color?": "orange", "Toppings?": "a, c", "Name?": "bob", "Bad?": "1,9", "Last?": "p"}
	for k, v := range want {
		if ans[k] != v {
			t.Errorf("%s = %q, want %q", k, ans[k], v)
		}
	}
	if !strings.Contains(w.String(), "1) teal - d") || !strings.Contains(w.String(), "[H] Color?") {
		t.Errorf("prompt = %s", w.String())
	}
	_, err = AnswerFromStdin(strings.NewReader(""), &w)(context.Background(), qs[:1])
	if err == nil {
		t.Error("expected error on EOF")
	}
}

func TestOnAskUserOtherToolsRule(t *testing.T) {
	ask := func(context.Context, []Question) (map[string]string, error) { return nil, nil }
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unset mode allows", Config{}, "allow"},
		{"skip permissions allows", Config{SkipPermissions: true, PermissionMode: "plan"}, "allow"},
		{"bypass allows", Config{PermissionMode: "bypassPermissions"}, "allow"},
		{"default denies", Config{PermissionMode: "default"}, "deny"},
		{"plan denies", Config{PermissionMode: "plan"}, "deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.OnAskUser = ask
			applyClientDefaults(&cfg)
			c := &Client{cfg: cfg}
			data, err := c.dispatchControl(askReq("Bash", `{"command":"ls"}`))
			if err != nil || data["behavior"] != tc.want {
				t.Fatalf("data=%v err=%v", data, err)
			}
			if tc.want == "deny" && !strings.Contains(data["message"].(string), "CanUseTool") {
				t.Errorf("message = %v", data["message"])
			}
			// AskUserQuestion is always answered.
			data, _ = c.dispatchControl(askReq("AskUserQuestion", askInput))
			if data["behavior"] != "allow" {
				t.Errorf("ask: %v", data)
			}
		})
	}
	// With CanUseTool set, the deny rule never applies.
	cfg := Config{PermissionMode: "plan", OnAskUser: ask, CanUseTool: func(context.Context, string, json.RawMessage, PermissionContext) (PermissionResult, error) {
		return Allow(), nil
	}}
	applyClientDefaults(&cfg)
	if cfg.denyOtherTools {
		t.Error("denyOtherTools set despite CanUseTool")
	}
}
