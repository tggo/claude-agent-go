package transport

import (
	"slices"
	"strings"
	"testing"
)

// The docker argv is world-readable on the docker host (/proc/<pid>/cmdline,
// ps -eww). An ANTHROPIC_API_KEY forwarded to the container must reach it
// through the docker CLI's environment, never as "-e KEY=VALUE".
func TestDockerEnvValuesNotInArgv(t *testing.T) {
	const secret = "sk-ant-api03-SECRETVALUE"
	opt := CommandOpts{Env: []string{"ANTHROPIC_API_KEY=" + secret, "FLAG"}, RunID: "abc123"}
	for name, args := range map[string][]string{
		"exec": DockerExec{Container: "c"}.Command(ctx, []string{"-p"}, opt).Args,
		"run":  DockerRun{Image: "img"}.Command(ctx, []string{"-p"}, opt).Args,
	} {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, secret) {
			t.Errorf("%s: secret in argv: %s", name, joined)
		}
		for _, want := range []string{"-e ANTHROPIC_API_KEY ", "-e FLAG ", "-e CAG_RUN_ID "} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: missing %q in %s", name, want, joined)
			}
		}
	}
	for _, env := range [][]string{
		DockerExec{Container: "c"}.Command(ctx, nil, opt).Env,
		DockerRun{Image: "img"}.Command(ctx, nil, opt).Env,
	} {
		if !slices.Contains(env, "ANTHROPIC_API_KEY="+secret) || !slices.Contains(env, "CAG_RUN_ID=abc123") {
			t.Errorf("value missing from the docker CLI env")
		}
		if len(env) <= 3 {
			t.Errorf("docker CLI env lost the host environment: %d entries", len(env))
		}
	}
	if c := (DockerExec{Container: "c"}).Command(ctx, nil, CommandOpts{}); c.Env != nil {
		t.Errorf("no extras: env = %v, want inherited (nil)", c.Env)
	}
}
