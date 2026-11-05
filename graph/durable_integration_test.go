//go:build integration && unix

package graph

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

type itState struct {
	Trail []string `json:"trail"`
	Reply string   `json:"reply"`
}

func init() { childMains["itest-agent-crash"] = itestAgentChild }

// loadTestEnv sets the model-access variables from the repo's .env, as the
// examples do (allowlisted keys, already-set variables win).
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

// itGraph is n1 -> agent -> n3. The agent is told a secret word.
func itGraph(secret string, store CheckpointStore, killOnFirstReply bool, extra ...CompileOption) (*Runnable[itState], error) {
	r := runner.New(runner.WithModel(itModel()), runner.WithMaxTurns(2), runner.WithTimeout(3*time.Minute),
		runner.WithEntrypoint("claude-agent-go-itest"))
	var opts []AgentOption
	if killOnFirstReply {
		opts = append(opts, WithProgress(func(ev claudecli.StreamEvent, _ int) {
			if ev.Type == "assistant" {
				// the session id was logged on the init event; die before the node can finish
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}))
	}
	opts = append(opts, WithRecoverPrompt("What was the secret word I told you earlier? Reply with only that word, nothing else."))
	return New[itState]("itest-durable").
		AddNode("n1", Func(func(s *itState) error { s.Trail = append(s.Trail, "n1"); return nil })).
		AddAgentNode("agent", Agent(r,
			func(*itState) runner.Input {
				return runner.Input{Prompt: "Remember this secret word: " + secret + ". Reply with only the word OK."}
			},
			func(s *itState, res *runner.Result) error {
				s.Reply = res.ResultText
				s.Trail = append(s.Trail, "agent")
				return nil
			},
			opts...)).
		AddNode("n3", Func(func(s *itState) error { s.Trail = append(s.Trail, "n3"); return nil })).
		AddEdge(Start, "n1").AddEdge("n1", "agent").AddEdge("agent", "n3").AddEdge("n3", End).
		Compile(append([]CompileOption{WithCheckpointStore(store)}, extra...)...)
}

func itestAgentChild() {
	store, err := FileStore(os.Getenv("IT_STORE"))
	if err != nil {
		panic(err)
	}
	r, err := itGraph(os.Getenv("IT_SECRET"), store, true)
	if err != nil {
		panic(err)
	}
	_, _, err = r.Run(context.Background(), itState{})
	println("child: run returned (it should have been killed):", err)
}

type inputSpy struct {
	mu      sync.Mutex
	resumes []string
	prompts []string
}

func (*inputSpy) NodeStart(context.Context, StepInfo)      {}
func (*inputSpy) NodeEnd(context.Context, StepInfo, error) {}
func (s *inputSpy) AgentInput(_ context.Context, _ StepInfo, in AgentInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumes = append(s.resumes, in.Input.Resume)
	s.prompts = append(s.prompts, in.Input.Prompt)
}

// A 3-node graph whose middle node is a real agent. The process is SIGKILLed
// after the agent's session id is in the log and before the node commits. The
// recovered node must --resume that session, and the model must still know the
// secret word it was told before the crash.
func TestIntegrationAgentSessionSurvivesCrash(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	loadTestEnv()
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	secret := "kumquat-" + hex.EncodeToString(rnd[:])
	storeDir := filepath.Join(t.TempDir(), "runs")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GRAPH_CHILD=itest-agent-crash", "IT_STORE="+storeDir, "IT_SECRET="+secret)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "child must die: %s", out)
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, ws.Signaled() && ws.Signal() == syscall.SIGKILL, "want SIGKILL, got %v: %s", ws, out)

	store, err := FileStore(storeDir)
	require.NoError(t, err)
	ctx := context.Background()
	l, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, l, 1)
	recs, err := store.Load(ctx, l[0].RunID)
	require.NoError(t, err)
	var session string
	for _, r := range recs {
		require.False(t, r.Kind == KindNodeCommit && r.Node == "agent", "agent must not be committed")
		if r.Kind == KindSession {
			session = r.SessionID
		}
	}
	require.NotEmpty(t, session, "session id must be in the log: %v", durKinds(recs))
	t.Logf("log before recovery: %v, session %s", durKinds(recs), session)

	spy := &inputSpy{}
	r, err := itGraph(secret, store, false, WithObserver(spy))
	require.NoError(t, err)
	st, rep, err := r.Recover(ctx, l[0].RunID)
	require.NoError(t, err)
	t.Logf("recovered: reply=%q trail=%v cost=$%.4f", st.Reply, st.Trail, rep.TotalCostUSD)

	require.Len(t, spy.resumes, 1)
	assert.Equal(t, session, spy.resumes[0], "recovery must pass --resume <logged session>")
	assert.NotContains(t, spy.prompts[0], secret, "the recovery prompt does not repeat the secret: only the session can know it")
	assert.Contains(t, strings.ToLower(st.Reply), secret, "the resumed agent must remember the word from before the crash")
	assert.Equal(t, []string{"n1", "agent", "n3"}, st.Trail)
	assert.Equal(t, []string{"n1", "agent", "n3"}, rep.Path)
}
