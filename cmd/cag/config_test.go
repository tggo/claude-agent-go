package main

import (
	"testing"

	"github.com/tggo/claude-agent-go/transport"
)

func TestTransportSpecBuild(t *testing.T) {
	tests := []struct {
		name    string
		spec    transportSpec
		want    string // type name expectation via type switch
		wantErr bool
	}{
		{"default local", transportSpec{}, "local", false},
		{"local", transportSpec{Kind: "local"}, "local", false},
		{"ssh ok", transportSpec{Kind: "ssh", Host: "u@h"}, "ssh", false},
		{"ssh no host", transportSpec{Kind: "ssh"}, "", true},
		{"docker ok", transportSpec{Kind: "docker", Container: "c"}, "docker", false},
		{"docker no container", transportSpec{Kind: "docker"}, "", true},
		{"docker-run ok", transportSpec{Kind: "docker-run", Image: "img"}, "dockerrun", false},
		{"docker-run no image", transportSpec{Kind: "docker-run"}, "", true},
		{"unknown", transportSpec{Kind: "carrier-pigeon"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := tc.spec.build()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var got string
			switch tr.(type) {
			case transport.Local:
				got = "local"
			case transport.SSH:
				got = "ssh"
			case transport.DockerExec:
				got = "docker"
			case transport.DockerRun:
				got = "dockerrun"
			}
			if got != tc.want {
				t.Errorf("built %s, want %s", got, tc.want)
			}
		})
	}
}
