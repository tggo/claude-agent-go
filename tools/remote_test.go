package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestConfigForHostAndPort(t *testing.T) {
	s, err := Serve("s", Tool{Name: "x", Description: "d",
		Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.Port() <= 0 {
		t.Fatalf("Port() = %d", s.Port())
	}
	name, cfg := s.ConfigForHost("host.docker.internal")
	want := fmt.Sprintf("http://host.docker.internal:%d/", s.Port())
	if name != "s" || cfg.Type != "http" || cfg.URL != want {
		t.Errorf("ConfigForHost = %q %+v, want url %q", name, cfg, want)
	}
	// local Config still points at 127.0.0.1
	_, local := s.Config()
	if !strings.HasPrefix(local.URL, "http://127.0.0.1:") {
		t.Errorf("local URL = %q", local.URL)
	}
}

func TestRemoteHelpers(t *testing.T) {
	if got := DockerHostGateway(); strings.Join(got, " ") != "--add-host host.docker.internal:host-gateway" {
		t.Errorf("DockerHostGateway = %v", got)
	}
	if got := SSHReverseTunnel(8080); strings.Join(got, " ") != "-R 8080:127.0.0.1:8080" {
		t.Errorf("SSHReverseTunnel = %v", got)
	}
}
