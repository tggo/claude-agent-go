//go:build integration

package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeWowhSkill creates a project-local skill that makes the model append a
// distinctive marker, so its presence/absence in output is a behavioral signal.
func writeWowhSkill(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".claude", "skills", "wowh")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: wowh\ndescription: Appends the marker WOWH to replies. Use whenever the user asks anything.\n---\nWhen you answer, you MUST end every reply with the exact token: WOWH-SKILL-ACTIVE\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const wowhMarker = "WOWH-SKILL-ACTIVE"
const wowhPrompt = "Use the wowh skill, then greet me in one short sentence."

// TestIntegrationSkillsAllowlistFilters proves the skills allowlist actually
// changes behavior: with the skill available the model applies it (marker
// present); with an allowlist that omits it, the skill is hidden from context
// and the model cannot use it (marker absent).
func TestIntegrationSkillsAllowlistFilters(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	dir := writeWowhSkill(t)

	// A) no allowlist -> skill available -> marker appears
	t.Run("available", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		c, err := New(ctx, Config{Model: "haiku", WorkDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer c.Close()
		turn, err := c.Query(ctx, wowhPrompt, nil)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !strings.Contains(turn.Text, wowhMarker) {
			t.Errorf("skill available but marker absent — skill not applied: %q", turn.Text)
		}
		t.Logf("available: marker present=%v", strings.Contains(turn.Text, wowhMarker))
	})

	// B) allowlist without wowh -> skill filtered out -> marker absent
	t.Run("filtered", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		c, err := New(ctx, Config{Model: "haiku", WorkDir: dir, Skills: []string{"go-review"}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer c.Close()
		turn, err := c.Query(ctx, wowhPrompt, nil)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if strings.Contains(turn.Text, wowhMarker) {
			t.Errorf("skill was filtered out but marker still present — allowlist had no effect: %q", turn.Text)
		}
		t.Logf("filtered: marker present=%v (want false)", strings.Contains(turn.Text, wowhMarker))
	})
}
