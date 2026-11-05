//go:build integration && unix

package temporal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/graph"
	"github.com/tggo/claude-agent-go/runner"
)

// TestMain turns the test binary into a Temporal worker when TEMPORAL_CHILD is
// set: the parent kills it mid-activity (the subprocess pattern of graph's
// crash tests).
func TestMain(m *testing.M) {
	if os.Getenv("TEMPORAL_CHILD") == "worker" {
		childWorker()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type itState struct {
	Trail  []string `json:"trail"`
	Reply  string   `json:"reply"`
	Answer string   `json:"answer"`
}

func loadTestEnv() {
	b, err := os.ReadFile("../.env")
	if err != nil {
		return
	}
	for _, ln := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		k = strings.TrimSpace(k)
		if !ok || strings.HasPrefix(k, "#") || !(strings.HasPrefix(k, "ANTHROPIC_") || k == "PIZZA_MODEL") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}

func itModel() string {
	if m := os.Getenv("PIZZA_MODEL"); m != "" {
		return m
	}
	return "haiku"
}

type inputSpy struct {
	mu      sync.Mutex
	resumes []string
	prompts []string
}

func (*inputSpy) NodeStart(context.Context, graph.StepInfo)      {}
func (*inputSpy) NodeEnd(context.Context, graph.StepInfo, error) {}
func (s *inputSpy) AgentInput(_ context.Context, _ graph.StepInfo, in graph.AgentInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumes = append(s.resumes, in.Input.Resume)
	s.prompts = append(s.prompts, in.Input.Prompt)
}

// itGraph is n1 -> agent -> ask. The agent is told a secret word; ask pauses
// for a human answer. With kill set, the process SIGKILLs itself when the
// agent's first reply arrives (its session id has been heartbeated by then).
func itGraph(name, secret string, kill bool, opts ...graph.CompileOption) (*graph.Runnable[itState], error) {
	rn := runner.New(runner.WithModel(itModel()), runner.WithMaxTurns(2), runner.WithTimeout(3*time.Minute),
		runner.WithEntrypoint("claude-agent-go-itest"))
	var ao []graph.AgentOption
	if kill {
		ao = append(ao, graph.WithProgress(func(ev claudecli.StreamEvent, _ int) {
			if ev.Type == "assistant" {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}))
	}
	ao = append(ao, graph.WithRecoverPrompt("What was the secret word I told you earlier? Reply with only that word, nothing else."))
	return graph.New[itState](name).
		AddNode("n1", graph.Func(func(s *itState) error { s.Trail = append(s.Trail, "n1"); return nil })).
		AddAgentNode("agent", graph.Agent(rn,
			func(*itState) runner.Input {
				return runner.Input{Prompt: "Remember this secret word: " + secret + ". Reply with only the word OK."}
			},
			func(s *itState, res *runner.Result) error {
				s.Reply = res.ResultText
				s.Trail = append(s.Trail, "agent")
				return nil
			}, ao...)).
		AddNode("ask", func(ctx context.Context, s *itState) error {
			if a, ok := graph.Answer(ctx); ok {
				s.Answer = a
				s.Trail = append(s.Trail, "ask")
				return nil
			}
			return graph.Interrupt("ship it?")
		}).
		AddEdge(graph.Start, "n1").AddEdge("n1", "agent").AddEdge("agent", "ask").AddEdge("ask", graph.End).
		Compile(opts...)
}

var itOpts = []Option{
	WithHeartbeat(3*time.Second, 500*time.Millisecond),
	WithActivityTimeout(5 * time.Minute),
	WithRetryPolicy(sdkRetry()),
}

func childWorker() {
	c, err := client.Dial(client.Options{HostPort: os.Getenv("TEMPORAL_ADDR")})
	if err != nil {
		panic(err)
	}
	r, err := itGraph(os.Getenv("IT_GRAPH"), os.Getenv("IT_SECRET"), true)
	if err != nil {
		panic(err)
	}
	w := worker.New(c, os.Getenv("IT_QUEUE"), worker.Options{})
	Register(w, r, itOpts...)
	if err := w.Run(worker.InterruptCh()); err != nil {
		panic(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// devServer starts `temporal server start-dev` on a free port and returns a
// connected client and the address. The server is stopped on cleanup.
func devServer(t *testing.T) (client.Client, string) {
	t.Helper()
	if _, err := exec.LookPath("temporal"); err != nil {
		t.Skip("temporal CLI not on PATH")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	loadTestEnv()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var logs bytes.Buffer
	cmd := exec.Command("temporal", "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", fmt.Sprint(port),
		"--http-port", fmt.Sprint(freePort(t)), "--log-level", "error")
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	var c client.Client
	var err error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if c, err = client.Dial(client.Options{HostPort: addr}); err == nil {
			t.Cleanup(c.Close)
			t.Logf("temporal dev server up on %s", addr)
			return c, addr
		}
	}
	t.Fatalf("temporal dev server did not come up: %v\n%s", err, logs.String())
	return nil, ""
}

func waitPending(t *testing.T, c client.Client, id string, d time.Duration) Pending {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p, err := PendingQuestion(context.Background(), c, id); err == nil && p.Waiting {
			return p
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("workflow %s never paused", id)
	return Pending{}
}

func secretWord() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "kumquat-" + hex.EncodeToString(b[:])
}

// Happy path on a real server: a real agent in node 2, an interrupt in node 3
// answered through a signal.
func TestIntegrationAgentGraphWithInterruptAnswer(t *testing.T) {
	c, _ := devServer(t)
	secret := secretWord()
	r, err := itGraph("it-happy", secret, false)
	require.NoError(t, err)
	w := worker.New(c, TaskQueue(r), worker.Options{})
	Register(w, r, itOpts...)
	require.NoError(t, w.Start())
	defer w.Stop()

	ctx := context.Background()
	run, err := Start(ctx, c, r, itState{}, StartOptions{ID: "it-happy-1"})
	require.NoError(t, err)
	p := waitPending(t, c, "it-happy-1", 3*time.Minute)
	assert.Equal(t, "ask", p.Node)
	assert.Equal(t, "ship it?", p.Question)
	require.NoError(t, Answer(ctx, c, "it-happy-1", "yes"))

	st, res, err := Wait[itState](ctx, run)
	require.NoError(t, err)
	t.Logf("result: reply=%q trail=%v cost=$%.4f path=%v", st.Reply, st.Trail, res.TotalCostUSD, res.Path)
	assert.Equal(t, []string{"n1", "agent", "ask"}, st.Trail)
	assert.Equal(t, "yes", st.Answer)
	assert.Contains(t, strings.ToUpper(st.Reply), "OK")
	assert.Greater(t, res.TotalCostUSD, 0.0)
	assert.Equal(t, []string{"n1", "agent", "ask", "ask"}, res.Path)
}

// The worker process is SIGKILLed mid-activity, after the agent's session id was
// heartbeated. A new worker takes over: the workflow completes and the agent
// resumes its session (it must still know the secret word).
func TestIntegrationWorkerKilledMidActivityAgentResumes(t *testing.T) {
	c, addr := devServer(t)
	secret := secretWord()
	r0, err := itGraph("it-crash", secret, false)
	require.NoError(t, err)
	queue := TaskQueue(r0) + "-" + secret

	ctx := context.Background()
	// worker 1: a separate process that kills itself on the agent's first reply
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "TEMPORAL_CHILD=worker", "TEMPORAL_ADDR="+addr, "IT_GRAPH=it-crash", "IT_SECRET="+secret, "IT_QUEUE="+queue)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	run, err := Start(ctx, c, r0, itState{}, StartOptions{ID: "it-crash-1", TaskQueue: queue})
	require.NoError(t, err)
	select {
	case <-exited:
	case <-time.After(3 * time.Minute):
		t.Fatalf("worker never died: %s", out.String())
	}
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ws.Signaled() && ws.Signal() == syscall.SIGKILL, "want SIGKILL, got %v", ws)
	t.Log("worker 1 killed mid-activity; starting worker 2")

	spy := &inputSpy{}
	r, err := itGraph("it-crash", secret, false, graph.WithObserver(spy))
	require.NoError(t, err)
	w := worker.New(c, queue, worker.Options{})
	Register(w, r, itOpts...)
	require.NoError(t, w.Start())
	defer w.Stop()

	waitPending(t, c, "it-crash-1", 3*time.Minute)
	require.NoError(t, Answer(ctx, c, "it-crash-1", "go"))
	st, res, err := Wait[itState](ctx, run)
	require.NoError(t, err)
	t.Logf("result: reply=%q trail=%v cost=$%.4f", st.Reply, st.Trail, res.TotalCostUSD)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Len(t, spy.resumes, 1, "the agent ran once on worker 2")
	assert.NotEmpty(t, spy.resumes[0], "worker 2 must --resume the session worker 1 heartbeated")
	assert.NotContains(t, spy.prompts[0], secret, "only the resumed session can know the secret")
	assert.Contains(t, strings.ToLower(st.Reply), secret, "the resumed agent remembers the word from before the crash")
	assert.Equal(t, []string{"n1", "agent", "ask"}, st.Trail)
	assert.Equal(t, "go", st.Answer)
}
