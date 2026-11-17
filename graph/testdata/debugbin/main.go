// Command debugbin is the program of the graph package's build-and-recover
// tests: a four-node graph whose node n3 has a bug in the default build and a
// fix under the "fixed" build tag. The tests build it twice, run the buggy
// build until it fails or is paused, then continue the same run with the
// fixed build: what a babysitter does after it patched the code.
//
//	debugbin -store DIR -mark DIR [-wait FILE] run
//	debugbin -store DIR -mark DIR recover RUN_ID
//
// The pause signal is a FilePause on the store dir. Every node appends a line
// to MARK/<node>.log, so the test can count executions across processes.
// It prints one JSON line and exits 0 (done), 2 (paused) or 1 (error).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tggo/claude-agent-go/graph"
)

type state struct {
	Trail []string `json:"trail"`
	Build string   `json:"build,omitempty"`
}

func main() {
	storeDir := flag.String("store", "", "checkpoint dir")
	markDir := flag.String("mark", "", "side-effect dir")
	wait := flag.String("wait", "", "n2 blocks until this file exists")
	flag.Parse()

	store, err := graph.FileStore(*storeDir)
	if err != nil {
		fail(err)
	}
	mark := func(node string) {
		f, err := os.OpenFile(filepath.Join(*markDir, node+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fail(err)
		}
		fmt.Fprintf(f, "%s build=%s pid=%d\n", node, build, os.Getpid())
		f.Close()
	}
	step := func(name string) graph.NodeFunc[state] {
		return func(_ context.Context, s *state) error {
			mark(name)
			s.Trail = append(s.Trail, name)
			return nil
		}
	}
	g := graph.New[state]("debugbin").
		AddNode("n1", step("n1")).
		AddNode("n2", func(ctx context.Context, s *state) error {
			mark("n2")
			for *wait != "" {
				if _, err := os.Stat(*wait); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			s.Trail = append(s.Trail, "n2")
			return nil
		}).
		AddNode("n3", func(ctx context.Context, s *state) error {
			mark("n3")
			if err := n3(s); err != nil {
				return err
			}
			s.Trail = append(s.Trail, "n3")
			s.Build = build
			return nil
		}).
		AddNode("n4", step("n4")).
		AddEdge(graph.Start, "n1").AddEdge("n1", "n2").AddEdge("n2", "n3").AddEdge("n3", "n4").AddEdge("n4", graph.End)
	r, err := g.Compile(graph.WithCheckpointStore(store), graph.WithPauseSignal(graph.FilePause{Dir: *storeDir}))
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	var out state
	switch flag.Arg(0) {
	case "run":
		out, _, err = r.Run(ctx, state{})
	case "recover":
		out, _, err = r.Recover(ctx, flag.Arg(1))
	default:
		fail(errors.New("usage: debugbin -store DIR -mark DIR run | recover RUN_ID"))
	}
	res := map[string]any{"build": build, "trail": out.Trail, "state_build": out.Build}
	code := 0
	var pe *graph.PausedError
	switch {
	case errors.As(err, &pe):
		res["status"], res["run_id"], res["node"] = "paused", pe.RunID, pe.Node
		code = 2
	case err != nil:
		res["status"], res["err"] = "error", err.Error()
		code = 1
	default:
		res["status"] = "done"
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	os.Exit(code)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "debugbin:", err)
	os.Exit(1)
}
