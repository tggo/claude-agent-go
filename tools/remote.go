package tools

import "fmt"

// Making in-process tools reachable from a remote agent
// ------------------------------------------------------
// tools.Serve listens on the host's 127.0.0.1. A container or ssh host can't
// reach that directly, so pair Server.ConfigForHost with one of these transport
// option helpers.

// DockerHostGateway returns `docker run`/`docker exec` flags that map
// host.docker.internal to the host, so a container can reach the tools server:
//
//	srv, _ := tools.Serve("myapp", myTool)
//	tr := transport.DockerRun{Image: "img", Options: tools.DockerHostGateway()}
//	name, cfg := srv.ConfigForHost("host.docker.internal")
//	mcp.WriteConfig(path, map[string]mcp.ServerConfig{name: cfg})
//
// On Docker Desktop the mapping exists already; on Linux this adds it explicitly.
func DockerHostGateway() []string {
	return []string{"--add-host", "host.docker.internal:host-gateway"}
}

// SSHReverseTunnel returns ssh flags that reverse-forward the remote's
// 127.0.0.1:port back to the host's 127.0.0.1:port, so a remote agent reaches
// the tools server as if it were local:
//
//	srv, _ := tools.Serve("myapp", myTool)
//	tr := transport.SSH{Host: "box", Options: tools.SSHReverseTunnel(srv.Port())}
//	name, cfg := srv.ConfigForHost("127.0.0.1")
//
// The remote sshd must allow port forwarding (AllowTcpForwarding, the default).
func SSHReverseTunnel(port int) []string {
	return []string{"-R", fmt.Sprintf("%d:127.0.0.1:%d", port, port)}
}
