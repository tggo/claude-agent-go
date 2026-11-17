// Command babysitter shows a babysitter in graphweb without a model: a scripted
// stand-in for the agent goes through the loop docs/babysitter.md describes.
//
//	fetch ──▶ transform ──▶ publish
//
// transform has a bug. The run fails; the babysitter wakes, reads the error,
// asks you in the browser whether to patch it, "fixes" the node (a flag
// stands in for an edit and a rebuild) and continues the same run with
// Recover. fetch does not run again. Answer "no" and it hands the run over to
// you instead; "Take over" stops it, "Continue run" recovers the run yourself.
//
//	go run ./examples/babysitter            # opens on http://127.0.0.1:8799
//	go run ./examples/babysitter -auto      # answers "yes" itself, no browser
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/graphweb"
)

type State struct {
	Rows      []string `json:"rows"`
	Published int      `json:"published"`
}

func main() {
	addr := flag.String("web", "127.0.0.1:8799", "graphweb address")
	auto := flag.Bool("auto", false, "answer the babysitter's question with yes, without a browser")
	step := flag.Duration("step", 600*time.Millisecond, "how long each node takes")
	flag.Parse()
	dir, err := os.MkdirTemp("", "babysitter-demo-")
	if err != nil {
		log.Fatal(err)
	}
	h, err := newHarness(dir, *step)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if !*auto {
		url, err := h.srv.Start(ctx, *addr)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("graphweb:", url)
	}
	if err := h.demo(ctx, *auto); err != nil {
		log.Fatal(err)
	}
	if !*auto {
		fmt.Println("done; Ctrl-C to quit")
		select {}
	}
}

// harness runs the graph and its babysitter, and is the graphweb
// BabysitterControl the panel's buttons call.
type harness struct {
	srv   *graphweb.Server
	store graph.CheckpointStore
	sig   *graph.MemPause
	run   *graph.Runnable[State]
	fixed atomic.Bool   // the "patched code"
	think time.Duration // how long the stand-in agent "works" on a step

	answers  chan string
	mu       sync.Mutex
	takenOut bool
	asking   bool // q1 is open: Answer is accepted
	runID    string
	done     chan error // the result of the last Recover
	// ctx is the harness's lifetime: a Recover the panel started stops with it.
	ctx context.Context
	// recovering guards the run against two Recovers at once (the babysitter's
	// and a person's Continue, or two clicks): both would append to one log.
	recovering bool
}

func newHarness(dir string, step time.Duration) (*harness, error) {
	h := &harness{think: 2 * step, sig: &graph.MemPause{}, answers: make(chan string, 1), done: make(chan error, 1), ctx: context.Background()}
	var err error
	if h.store, err = graph.FileStore(dir); err != nil {
		return nil, err
	}
	if h.srv, err = graphweb.New(graphweb.WithBabysitter(h)); err != nil {
		return nil, err
	}
	nap := func(ctx context.Context) error {
		select {
		case <-time.After(step):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.run, err = graph.New[State]("babysitter-demo").
		AddNode("fetch", func(ctx context.Context, s *State) error {
			s.Rows = []string{"a", "b", ""}
			return nap(ctx)
		}).
		AddNode("transform", func(ctx context.Context, s *State) error {
			if err := nap(ctx); err != nil {
				return err
			}
			if !h.fixed.Load() {
				return errors.New("transform: row 3 is empty: index out of range [0] with length 0")
			}
			var out []string
			for _, r := range s.Rows {
				if r != "" {
					out = append(out, strings.ToUpper(r))
				}
			}
			s.Rows = out
			return nil
		}).
		AddNode("publish", func(ctx context.Context, s *State) error {
			s.Published = len(s.Rows)
			return nap(ctx)
		}).
		AddEdge(graph.Start, "fetch").AddEdge("fetch", "transform").AddEdge("transform", "publish").AddEdge("publish", graph.End).
		Compile(graph.WithObserver(h.srv), graph.WithCheckpointStore(h.store), graph.WithPauseSignal(h.sig))
	return h, err
}

// demo runs the graph once and, when it fails, lets the babysitter handle it.
func (h *harness) demo(ctx context.Context, auto bool) error {
	time.Sleep(h.think) // let a browser connect first
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	_, rep, err := h.run.Run(ctx, State{})
	h.mu.Lock()
	h.runID = rep.RunID
	h.mu.Unlock()
	if err == nil {
		return nil
	}
	return h.babysit(ctx, err, auto)
}

// runIDSafe is runID for other goroutines (the panel's handlers).
func (h *harness) runIDSafe() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runID
}

func (h *harness) say(kind, text string) {
	h.srv.Babysitter(h.runID, graphweb.BabysitterEvent{Kind: kind, Text: text})
}

func (h *harness) out() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.takenOut
}

// babysit is what an agent would do; each step is reported to the panel.
func (h *harness) babysit(ctx context.Context, runErr error, auto bool) error {
	h.srv.Babysitter(h.runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterWoke, Reason: "failed", Text: runErr.Error()})
	h.say(graphweb.BabysitterDoing, "cag runs <store> "+h.runID)
	time.Sleep(h.think)
	h.say(graphweb.BabysitterSaid, "fetch committed 3 rows; transform fails on the empty third row. The fix is to skip empty rows.")
	h.mu.Lock()
	h.asking = !h.takenOut
	h.mu.Unlock()
	h.srv.Babysitter(h.runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterAsked, ID: "q1",
		Text: "Patch transform to skip empty rows and continue the run? (yes / no)"})
	if auto {
		h.answers <- "yes"
	}
	var answer string
	select {
	case answer = <-h.answers:
	case <-ctx.Done():
		return ctx.Err()
	}
	if h.out() {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
		h.say(graphweb.BabysitterHandover, "Not patching. The run stays failed at transform; Continue run recovers it once you fixed the code.")
		return nil
	}
	h.say(graphweb.BabysitterDoing, "patch transform; go build ./...")
	time.Sleep(h.think)
	h.fixed.Store(true)
	if !h.startRecover() {
		h.say(graphweb.BabysitterHandover, "Someone is already continuing this run; leaving it to them.")
		return nil
	}
	defer h.endRecover()
	h.say(graphweb.BabysitterDoing, "Recover "+h.runID)
	_, _, err := h.run.Recover(ctx, h.runID)
	if err != nil {
		h.say(graphweb.BabysitterHandover, "Recover failed: "+err.Error())
		return err
	}
	h.say(graphweb.BabysitterIdle, "Run finished. fetch was not re-run; transform ran with the fix.")
	return nil
}

// ---- graphweb.BabysitterControl ----

// ours refuses a run id this harness does not run: the panel sends the id of
// the run it shows, and a harness must not act on another run because of it.
func (h *harness) ours(runID string) error {
	if id := h.runIDSafe(); runID == "" || runID != id {
		return fmt.Errorf("run %q is not this harness's run", runID)
	}
	return nil
}

func (h *harness) startRecover() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.recovering {
		return false
	}
	h.recovering = true
	return true
}

func (h *harness) endRecover() {
	h.mu.Lock()
	h.recovering = false
	h.mu.Unlock()
}

func (h *harness) Pause(_ context.Context, runID string) error {
	if err := h.ours(runID); err != nil {
		return err
	}
	h.sig.Request(runID)
	return nil
}

// Continue recovers the run in the background, under the harness's context
// (not the request's, which ends with the HTTP call). It refuses while a
// Recover is already going.
func (h *harness) Continue(_ context.Context, runID string) error {
	if err := h.ours(runID); err != nil {
		return err
	}
	if !h.startRecover() {
		return errors.New("the run is already being continued")
	}
	h.mu.Lock()
	ctx := h.ctx
	h.mu.Unlock()
	go func() {
		defer h.endRecover()
		_, _, err := h.run.Recover(ctx, runID)
		select {
		case h.done <- err:
		default:
		}
	}()
	return nil
}

func (h *harness) TakeOver(_ context.Context, runID string) error {
	if err := h.ours(runID); err != nil {
		return err
	}
	h.mu.Lock()
	h.takenOut = true
	wasAsking := h.asking
	h.asking = false
	h.mu.Unlock()
	if wasAsking {
		h.answers <- "" // release the babysitter waiting for an answer
	}
	return nil
}

func (h *harness) Answer(_ context.Context, runID, id, answer string) error {
	if err := h.ours(runID); err != nil {
		return err
	}
	if id != "q1" {
		return fmt.Errorf("no open question %q", id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.asking {
		return errors.New("the babysitter is not waiting for an answer")
	}
	h.asking = false
	h.answers <- answer // buffered: one answer per question
	return nil
}
