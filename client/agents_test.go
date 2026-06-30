package client

import (
	"encoding/json"
	"testing"
)

func TestAgentDefinitionMarshalOmitsEmpty(t *testing.T) {
	a := AgentDefinition{Description: "d", Prompt: "p"}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["description"] != "d" || m["prompt"] != "p" {
		t.Errorf("required fields missing: %v", m)
	}
	// optional fields must be omitted when zero
	for _, k := range []string{"tools", "disallowedTools", "model", "skills", "memory", "initialPrompt", "maxTurns", "background", "effort", "permissionMode", "mcpServers"} {
		if _, ok := m[k]; ok {
			t.Errorf("optional field %q should be omitted when empty", k)
		}
	}
}

func TestAgentDefinitionMarshalFull(t *testing.T) {
	bg := true
	a := AgentDefinition{
		Description: "reviewer", Prompt: "review",
		Tools: []string{"Read", "Grep"}, Model: "opus",
		MaxTurns: 3, Background: &bg, Effort: "high", PermissionMode: "plan",
	}
	b, _ := json.Marshal(a)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["model"] != "opus" || m["effort"] != "high" || m["permissionMode"] != "plan" {
		t.Errorf("fields = %v", m)
	}
	if m["background"] != true || m["maxTurns"] != float64(3) {
		t.Errorf("background/maxTurns = %v", m)
	}
}

func TestInitRequest(t *testing.T) {
	// bare: only hooks key (nil)
	c := &Client{cfg: Config{}}
	req := c.initRequest()
	if _, ok := req["hooks"]; !ok {
		t.Error("hooks key should always be present")
	}
	if _, ok := req["agents"]; ok {
		t.Error("agents should be absent when none configured")
	}
	if _, ok := req["skills"]; ok {
		t.Error("skills should be absent when nil")
	}

	// with agents + skills
	c2 := &Client{cfg: Config{
		Agents: map[string]AgentDefinition{"r": {Description: "d", Prompt: "p"}},
		Skills: []string{"go-review"},
	}}
	req2 := c2.initRequest()
	agents, ok := req2["agents"].(map[string]AgentDefinition)
	if !ok || len(agents) != 1 {
		t.Errorf("agents = %v", req2["agents"])
	}
	skills, ok := req2["skills"].([]string)
	if !ok || len(skills) != 1 || skills[0] != "go-review" {
		t.Errorf("skills = %v", req2["skills"])
	}

	// empty (non-nil) skills slice is still sent (explicit "no skills")
	c3 := &Client{cfg: Config{Skills: []string{}}}
	if _, ok := c3.initRequest()["skills"]; !ok {
		t.Error("explicit empty skills slice should be sent")
	}
}
