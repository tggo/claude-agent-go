// Command cag is a command-line runner for Claude Code agents — the CLI over
// the claude-agent-go SDK. It runs agents locally, in containers, or over SSH,
// and fans batches of tasks across a fleet from a YAML file, without writing Go.
//
//	cag run --transport ssh --host gpu-box "summarize the test failures"
//	cag fleet tasks.yaml
//
// Note: the SDK library packages have no external runtime deps; this CLI binary
// additionally uses gopkg.in/yaml.v3 for the fleet config.
package main

import (
	"fmt"
	"log/slog"
	"os"
)

// quietLogger suppresses the SDK's info logs so the CLI prints only its own
// output; SDK warnings/errors still surface on stderr.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

const usage = `cag — a runner for Claude Code agents (local, docker, ssh, fleet)

usage:
  cag run [flags] "<prompt>"     run a single agent
  cag fleet <config.yaml>        run a batch of tasks across a fleet
  cag version                    print version

run 'cag run -h' or 'cag fleet -h' for flags.`

var version = "dev" // overridden at build time via -ldflags

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "fleet":
		fleetCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("cag %s\n", version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cag: "+format+"\n", args...)
	os.Exit(1)
}
