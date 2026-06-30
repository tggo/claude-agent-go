// Command transport shows how to launch claude over different transports —
// locally (default), inside a container via docker exec, or on a remote host
// via ssh — without changing any other SDK code.
//
//	go run ./examples/transport                              # local
//	go run ./examples/transport -mode docker -container box  # docker exec
//	go run ./examples/transport -mode ssh -host user@server  # ssh
//
// Docker/SSH need the respective infrastructure (a running container with
// claude installed, or an ssh host with claude on PATH and credentials).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/transport"
)

func main() {
	mode := flag.String("mode", "local", "transport: local | docker | ssh")
	prompt := flag.String("prompt", "Reply with exactly: PONG", "prompt")
	container := flag.String("container", "", "container name (docker mode)")
	host := flag.String("host", "", "user@host (ssh mode)")
	bin := flag.String("bin", "claude", "claude binary path on the target")
	flag.Parse()

	var tr transport.Transport
	switch *mode {
	case "local":
		tr = transport.Local{Binary: *bin}
	case "docker":
		if *container == "" {
			log.Fatal("docker mode needs -container")
		}
		tr = transport.DockerExec{Container: *container, Binary: *bin}
	case "ssh":
		if *host == "" {
			log.Fatal("ssh mode needs -host")
		}
		// -tt makes the remote receive signals when the local ssh is killed.
		tr = transport.SSH{Host: *host, Binary: *bin, Options: []string{"-tt"}}
	default:
		log.Fatalf("unknown mode %q", *mode)
	}

	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithTimeout(2*time.Minute),
		runner.WithTransport(tr),
	)
	res, err := r.RunJSON(context.Background(), runner.Input{Prompt: *prompt})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[%s] %s  (cost $%.5f)\n", *mode, res.ResultText, res.TotalCostUSD)
}
