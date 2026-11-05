//go:build integration

package client

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIntegrationOnAskUser proves the model's AskUserQuestion reaches OnAskUser
// and that the returned answer is what the model acts on (the effect).
func TestIntegrationOnAskUser(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var got []Question
	c, err := New(ctx, Config{
		Model:    "haiku",
		WorkDir:  t.TempDir(),
		MaxTurns: 6,
		OnAskUser: func(_ context.Context, qs []Question) (map[string]string, error) {
			mu.Lock()
			got = append(got, qs...)
			mu.Unlock()
			ans := map[string]string{}
			for _, q := range qs {
				ans[q.Question] = "teal"
			}
			return ans, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	turn, err := c.Query(ctx, `Use the AskUserQuestion tool to ask the user which color they prefer, with exactly two options: "teal" and "orange". After you receive the answer, reply with exactly the chosen color in UPPERCASE and nothing else.`, nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("OnAskUser never called; final=%q", turn.Text)
	}
	var labels []string
	for _, o := range got[0].Options {
		labels = append(labels, strings.ToLower(o.Label))
	}
	l := strings.Join(labels, ",")
	if !strings.Contains(l, "teal") || !strings.Contains(l, "orange") {
		t.Errorf("options = %v, want teal and orange", labels)
	}
	if !strings.Contains(turn.Text, "TEAL") {
		t.Errorf("final text %q does not contain TEAL", turn.Text)
	}
}
