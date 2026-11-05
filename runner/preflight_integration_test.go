//go:build integration

package runner

import (
	"context"
	"os/exec"
	"testing"
)

// Real `claude --version` + `claude auth status` through the local transport.
func TestIntegrationPreflight(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}
	info, err := New().Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	t.Logf("version=%s auth=%v/%s", info.Version, info.AuthChecked, info.AuthMethod)
	if info.Version == "" {
		t.Error("no version")
	}
}
