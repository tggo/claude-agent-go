package main

import (
	"fmt"
	"strings"

	"github.com/tggo/claude-agent-go/transport"
)

// transportSpec is the shared shape (in flags and YAML) that selects and
// configures a transport.
type transportSpec struct {
	Kind      string   `yaml:"transport"` // local | ssh | docker | docker-run
	Binary    string   `yaml:"bin"`       // claude path on the target
	Host      string   `yaml:"host"`      // ssh
	SSHPort   string   `yaml:"port"`      // ssh
	SSHOpts   []string `yaml:"sshOpts"`   // ssh extra flags
	Container string   `yaml:"container"` // docker exec
	Image     string   `yaml:"image"`     // docker run
	User      string   `yaml:"user"`      // docker
	Network   string   `yaml:"network"`   // docker run
	Memory    string   `yaml:"memory"`    // docker run
	CPUs      string   `yaml:"cpus"`      // docker run
}

// build returns the transport for a spec, or an error if required fields are
// missing.
func (s transportSpec) build() (transport.Transport, error) {
	switch strings.ToLower(s.Kind) {
	case "", "local":
		return transport.Local{Binary: s.Binary}, nil
	case "ssh":
		if s.Host == "" {
			return nil, fmt.Errorf("ssh transport needs a host")
		}
		return transport.SSH{Host: s.Host, Binary: s.Binary, Port: s.SSHPort, Options: s.SSHOpts}, nil
	case "docker", "docker-exec":
		if s.Container == "" {
			return nil, fmt.Errorf("docker transport needs a container")
		}
		return transport.DockerExec{Container: s.Container, Binary: s.Binary, User: s.User}, nil
	case "docker-run":
		if s.Image == "" {
			return nil, fmt.Errorf("docker-run transport needs an image")
		}
		return transport.DockerRun{
			Image: s.Image, Binary: s.Binary, User: s.User,
			Network: s.Network, Memory: s.Memory, CPUs: s.CPUs,
		}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (local|ssh|docker|docker-run)", s.Kind)
	}
}
