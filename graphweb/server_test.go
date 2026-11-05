package graphweb

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/graph"
)

type st struct {
	N     int    `json:"n"`
	Note  string `json:"note,omitempty"`
	Asked string `json:"asked,omitempty"`
}

func newSrv(t *testing.T, opts ...Option) *Server {
	t.Helper()
	s, err := New(opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func loopRun(t *testing.T, s *Server) *graph.Runnable[st] {
	t.Helper()
	g := graph.New[st]("loop").
		AddNode("inc", graph.Func(func(x *st) error { x.N++; x.Note = "n=" + strconv.Itoa(x.N); return nil })).
		AddNode("fin", graph.Func(func(*st) error { return nil })).
		AddEdge(graph.Start, "inc").
		AddConditionalEdge("inc", func(x *st) string {
			if x.N < 3 {
				return "inc"
			}
			return "fin"
		}, "inc", "fin").
		AddEdge("fin", graph.End)
	r, err := g.Compile(graph.WithObserver(s))
	require.NoError(t, err)
	return r
}

func newTS(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })
	return ts
}

func types(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func TestEventStreamContent(t *testing.T) {
	s := newSrv(t)
	_, rep, err := loopRun(t, s).Run(context.Background(), st{})
	require.NoError(t, err)
	evs := s.Events()
	assert.Equal(t, []string{
		"graph_start",
		"edge", "node_start", "node_end", "edge",
		"node_start", "node_end", "edge",
		"node_start", "node_end", "edge",
		"node_start", "node_end", "edge",
		"graph_end"}, types(evs))
	for i, e := range evs {
		assert.Equal(t, int64(i+1), e.Seq)
		assert.Equal(t, SchemaVersion, e.V)
		assert.NotEmpty(t, e.Run)
		assert.False(t, e.TS.IsZero())
	}
	assert.Equal(t, "loop", evs[0].Topology.Name)
	assert.JSONEq(t, `{"n":0}`, string(evs[0].State))
	// edge iteration counts and back flag
	var back []Event
	for _, e := range evs {
		if e.Type == TypeEdge && e.From == "inc" && e.To == "inc" {
			back = append(back, e)
		}
	}
	require.Len(t, back, 2)
	assert.Equal(t, 1, back[0].Iter)
	assert.Equal(t, 2, back[1].Iter)
	assert.True(t, back[0].Back)
	// diff: only changed keys, with before/after
	end := evs[3]
	assert.Equal(t, TypeNodeEnd, end.Type)
	assert.JSONEq(t, `{"n":{"before":0,"after":1},"note":{"after":"n=1"}}`, mustJSON(t, end.Diff))
	assert.JSONEq(t, `{"n":1,"note":"n=1"}`, string(end.State))
	last := evs[len(evs)-1]
	assert.Equal(t, "ok", last.Status)
	assert.Equal(t, []string{"inc", "inc", "inc", "fin"}, last.Path)
	assert.Equal(t, int64(rep.Duration/time.Millisecond), last.DurationMS)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

type sse struct {
	resp *http.Response
	sc   *bufio.Scanner
}

func openSSE(t *testing.T, url string, hdr map[string]string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/events", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	return &sse{resp, sc}
}

// next returns the next data event (skipping comments) with its id.
func (c *sse) next(t *testing.T) (string, Event) {
	t.Helper()
	var id string
	done := make(chan struct{})
	var ev Event
	go func() {
		defer close(done)
		for c.sc.Scan() {
			l := c.sc.Text()
			switch {
			case strings.HasPrefix(l, "id: "):
				id = l[4:]
			case strings.HasPrefix(l, "data: "):
				_ = json.Unmarshal([]byte(l[6:]), &ev)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
	return id, ev
}

func TestSSEOrderLiveAndReconnect(t *testing.T) {
	s := newSrv(t)
	ts := newTS(t, s)
	c := openSSE(t, ts.URL, nil)

	// Subscriber attached before the run sees events as they happen.
	go func() { _, _, _ = loopRun(t, s).Run(context.Background(), st{}) }()
	var got []Event
	for len(got) == 0 || got[len(got)-1].Type != TypeGraphEnd {
		id, ev := c.next(t)
		assert.Equal(t, strconv.FormatInt(ev.Seq, 10), id)
		got = append(got, ev)
	}
	for i, e := range got {
		assert.Equal(t, int64(i+1), e.Seq, "in order, no gaps")
	}
	total := len(got)

	// Fresh connection replays from the start (page reload).
	c2 := openSSE(t, ts.URL, nil)
	id, first := c2.next(t)
	assert.Equal(t, "1", id)
	assert.Equal(t, TypeGraphStart, first.Type)

	// Reconnect with Last-Event-ID resumes after it.
	c3 := openSSE(t, ts.URL, map[string]string{"Last-Event-ID": strconv.Itoa(total - 2)})
	_, e1 := c3.next(t)
	_, e2 := c3.next(t)
	assert.Equal(t, int64(total-1), e1.Seq)
	assert.Equal(t, TypeGraphEnd, e2.Type)

	// And ?from= works the same way, then live events keep flowing.
	c5 := openSSE(t, ts.URL, map[string]string{"Last-Event-ID": strconv.Itoa(total)})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _, _ = loopRun(t, s).Run(context.Background(), st{})
	}()
	_, nx := c5.next(t)
	assert.Equal(t, int64(total+1), nx.Seq)
	assert.Equal(t, TypeGraphStart, nx.Type)
}

func do(t *testing.T, method, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTokenHostOriginChecks(t *testing.T) {
	s := newSrv(t)
	ts := newTS(t, s)
	tok := map[string]string{"X-Graphweb-Token": s.Token()}

	// page embeds the token; JSON content type for the API
	page := do(t, "GET", ts.URL+"/", "", nil)
	require.Equal(t, 200, page.StatusCode)
	var sb strings.Builder
	_, _ = bufio.NewReader(page.Body).WriteTo(&sb)
	assert.Contains(t, sb.String(), s.Token())
	assert.NotContains(t, sb.String(), "GRAPHWEB_BOOT")
	assert.Contains(t, sb.String(), `"readOnly":false`)

	for _, p := range []string{"/api/answer", "/api/breakpoints", "/api/control"} {
		assert.Equal(t, 403, do(t, "POST", ts.URL+p, `{}`, nil).StatusCode, p+" without token")
		assert.Equal(t, 403, do(t, "POST", ts.URL+p, `{}`, map[string]string{"X-Graphweb-Token": "nope"}).StatusCode, p+" wrong token")
	}
	// token ok but a foreign Origin (CSRF from another site holding a leaked token)
	assert.Equal(t, 403, do(t, "POST", ts.URL+"/api/control", `{"action":"pause"}`,
		map[string]string{"X-Graphweb-Token": s.Token(), "Origin": "https://evil.example"}).StatusCode)
	// same origin is fine
	host := strings.TrimPrefix(ts.URL, "http://")
	assert.Equal(t, 200, do(t, "POST", ts.URL+"/api/control", `{"action":"pause"}`,
		map[string]string{"X-Graphweb-Token": s.Token(), "Origin": ts.URL}).StatusCode)
	_ = host

	// DNS rebinding: a non-local Host is refused on every route, even with the token.
	for _, p := range []string{"/", "/api/events", "/api/topology", "/api/breakpoints"} {
		assert.Equal(t, 403, do(t, "GET", ts.URL+p, "", map[string]string{"Host": "evil.example"}).StatusCode, p)
	}
	assert.Equal(t, 403, do(t, "POST", ts.URL+"/api/control", `{"action":"pause"}`,
		map[string]string{"Host": "evil.example:80", "X-Graphweb-Token": s.Token()}).StatusCode)
	assert.Equal(t, 200, do(t, "GET", ts.URL+"/api/breakpoints", "", map[string]string{"Host": "localhost:1234"}).StatusCode)
	assert.Equal(t, 200, do(t, "GET", ts.URL+"/api/breakpoints", "", map[string]string{"Host": "[::1]:1234"}).StatusCode)

	// validation
	assert.Equal(t, 400, do(t, "POST", ts.URL+"/api/control", `{"action":"explode"}`, tok).StatusCode)
	assert.Equal(t, 400, do(t, "POST", ts.URL+"/api/control", `nope`, tok).StatusCode)
	assert.Equal(t, 400, do(t, "POST", ts.URL+"/api/answer", `nope`, tok).StatusCode)
	assert.Equal(t, 400, do(t, "POST", ts.URL+"/api/breakpoints", `nope`, tok).StatusCode)
	assert.Equal(t, 409, do(t, "POST", ts.URL+"/api/answer", `{"answer":"x"}`, tok).StatusCode, "no question open")
	assert.Equal(t, 404, do(t, "GET", ts.URL+"/api/topology", "", nil).StatusCode, "no run yet")

	// extra allowed hosts
	s2 := newSrv(t, WithAllowedHosts("box.lan"))
	ts2 := newTS(t, s2)
	assert.Equal(t, 200, do(t, "GET", ts2.URL+"/api/breakpoints", "", map[string]string{"Host": "box.lan:8799"}).StatusCode)
}

func TestTopologyEndpoint(t *testing.T) {
	s := newSrv(t)
	ts := newTS(t, s)
	_, _, err := loopRun(t, s).Run(context.Background(), st{})
	require.NoError(t, err)
	for _, q := range []string{"", "?run=" + s.Events()[0].Run} {
		resp := do(t, "GET", ts.URL+"/api/topology"+q, "", nil)
		require.Equal(t, 200, resp.StatusCode)
		var top graph.Topology
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&top))
		assert.Equal(t, "loop", top.Name)
	}
	assert.Equal(t, 404, do(t, "GET", ts.URL+"/api/topology?run=zzz", "", nil).StatusCode)
}

func askGraph(t *testing.T, s *Server) *graph.Runnable[st] {
	t.Helper()
	r, err := graph.New[st]("ask").
		AddNode("ask", func(ctx context.Context, x *st) error {
			if a, ok := graph.Answer(ctx); ok {
				x.Asked = a
				return nil
			}
			return graph.Interrupt("proceed?")
		}).
		AddEdge(graph.Start, "ask").AddEdge("ask", graph.End).Compile(graph.WithObserver(s))
	require.NoError(t, err)
	return r
}

func TestAnswerEndpointResumesInterruptedGraph(t *testing.T) {
	s := newSrv(t)
	ts := newTS(t, s)
	r := askGraph(t, s)
	type res struct {
		st  st
		rep *graph.Report
		err error
	}
	ch := make(chan res, 1)
	go func() {
		x, rep, err := RunInteractive(context.Background(), r, st{}, s)
		ch <- res{x, rep, err}
	}()
	c := openSSE(t, ts.URL, nil)
	var q Event
	for q.Type != TypeInterrupt {
		_, q = c.next(t)
	}
	assert.Equal(t, "proceed?", q.Question)
	assert.Equal(t, "ask", q.Node)
	select {
	case <-ch:
		t.Fatal("finished before the answer")
	case <-time.After(50 * time.Millisecond):
	}
	resp := do(t, "POST", ts.URL+"/api/answer", `{"answer":"yes"}`, map[string]string{"X-Graphweb-Token": s.Token(), "Content-Type": "application/json"})
	require.Equal(t, 200, resp.StatusCode)
	out := <-ch
	require.NoError(t, out.err)
	assert.Equal(t, "yes", out.st.Asked)
	assert.Equal(t, []string{"ask", "ask"}, out.rep.Path)
	evs := s.Events()
	ty := types(evs)
	assert.Contains(t, ty, TypeResume)
	assert.Equal(t, TypeGraphEnd, ty[len(ty)-1])
	assert.Equal(t, 1, count(ty, TypeGraphStart), "a resumed segment does not restart the run in the UI")
	var runs = map[string]bool{}
	for _, e := range evs {
		runs[e.Run] = true
	}
	assert.Len(t, runs, 1, "one run id across the resume")
	// second answer with nothing open
	assert.Equal(t, 409, do(t, "POST", ts.URL+"/api/answer", `{"answer":"again"}`, map[string]string{"X-Graphweb-Token": s.Token()}).StatusCode)
}

func count(xs []string, x string) (n int) {
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return
}

func TestRunInteractiveWithoutObserverAndCancel(t *testing.T) {
	s := newSrv(t)
	// Graph compiled WITHOUT the server as observer: the question must still surface.
	r, err := graph.New[st]("ask").
		AddNode("ask", func(ctx context.Context, x *st) error {
			if _, ok := graph.Answer(ctx); ok {
				return nil
			}
			return graph.Interrupt("sure?")
		}).AddEdge(graph.Start, "ask").AddEdge("ask", graph.End).Compile()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := RunInteractive(ctx, r, st{}, s); done <- err }()
	require.Eventually(t, func() bool { return s.Answer("go") == nil }, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, <-done)
	assert.Contains(t, types(s.Events()), TypeInterrupt)

	// Cancel while waiting.
	go func() { _, _, err := RunInteractive(ctx, r, st{}, s); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

func TestAskHelper(t *testing.T) {
	s := newSrv(t)
	r, err := graph.New[st]("ask").
		AddNode("n", func(ctx context.Context, x *st) error {
			a, err := s.Ask(ctx, "name?")
			x.Asked = a
			return err
		}).AddEdge(graph.Start, "n").AddEdge("n", graph.End).Compile(graph.WithObserver(s))
	require.NoError(t, err)
	done := make(chan st, 1)
	go func() { x, _, _ := r.Run(context.Background(), st{}); done <- x }()
	require.Eventually(t, func() bool { return s.Answer("bob") == nil }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "bob", (<-done).Asked)
	var q, a Event
	for _, e := range s.Events() {
		switch e.Type {
		case TypeInterrupt:
			q = e
		case TypeResume:
			a = e
		}
	}
	assert.Equal(t, "name?", q.Question)
	assert.Equal(t, "n", q.Node)
	assert.Equal(t, "bob", a.Answer)

	// Ask honours ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Ask(ctx, "x")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBreakpointPauseContinueStep(t *testing.T) {
	s := newSrv(t)
	ts := newTS(t, s)
	tok := map[string]string{"X-Graphweb-Token": s.Token(), "Content-Type": "application/json"}
	require.Equal(t, 200, do(t, "POST", ts.URL+"/api/breakpoints", `{"nodes":["fin","inc"]}`, tok).StatusCode)
	resp := do(t, "GET", ts.URL+"/api/breakpoints", "", nil)
	var bp struct{ Nodes []string }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&bp))
	assert.Equal(t, []string{"fin", "inc"}, bp.Nodes)
	require.Equal(t, 200, do(t, "POST", ts.URL+"/api/breakpoints", `{"nodes":["fin"]}`, tok).StatusCode)

	done := make(chan error, 1)
	go func() { _, _, err := loopRun(t, s).Run(context.Background(), st{}); done <- err }()
	hit := func() Event {
		var h Event
		require.Eventually(t, func() bool {
			evs := s.Events()
			for i := len(evs) - 1; i >= 0; i-- {
				if evs[i].Type == TypeBreakpointHit {
					h = evs[i]
					return true
				}
			}
			return false
		}, 3*time.Second, 5*time.Millisecond)
		return h
	}
	h := hit()
	assert.Equal(t, "fin", h.Node)
	assert.Equal(t, 4, h.Step)
	select {
	case <-done:
		t.Fatal("ran past the breakpoint")
	case <-time.After(60 * time.Millisecond):
	}
	assert.NotContains(t, types(s.Events())[len(s.Events())-1:], TypeGraphEnd)
	require.Equal(t, 200, do(t, "POST", ts.URL+"/api/control", `{"action":"continue"}`, tok).StatusCode)
	require.NoError(t, <-done)

	// pause then step: stops before each next node.
	s2 := newSrv(t)
	require.NoError(t, s2.Control("pause"))
	done2 := make(chan error, 1)
	go func() { _, _, err := loopRun(t, s2).Run(context.Background(), st{}); done2 <- err }()
	waitHits := func(n int) {
		require.Eventually(t, func() bool { return count(types(s2.Events()), TypeBreakpointHit) == n }, 3*time.Second, 5*time.Millisecond)
	}
	waitHits(1)
	require.NoError(t, s2.Control("step"))
	waitHits(2)
	require.NoError(t, s2.Control("step"))
	waitHits(3)
	require.NoError(t, s2.Control("continue"))
	require.NoError(t, <-done2)
	assert.Equal(t, 3, count(types(s2.Events()), TypeBreakpointHit))
	assert.Error(t, s2.Control("bogus"))

	// ctx cancel releases a blocked gate and fails the run.
	s3 := newSrv(t)
	s3.SetBreakpoints([]string{"inc"})
	ctx, cancel := context.WithCancel(context.Background())
	done3 := make(chan error, 1)
	go func() { _, _, err := loopRun(t, s3).Run(ctx, st{}); done3 <- err }()
	require.Eventually(t, func() bool { return count(types(s3.Events()), TypeBreakpointHit) == 1 }, 3*time.Second, 5*time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done3, context.Canceled)
	last := s3.Events()[len(s3.Events())-1]
	assert.Equal(t, "cancelled", last.Status)
}

func TestGraphEndStatuses(t *testing.T) {
	s := newSrv(t)
	boom := graph.New[st]("boom").
		AddNode("x", graph.Func(func(*st) error { return errors.New("kaboom") })).
		AddEdge(graph.Start, "x").AddEdge("x", graph.End)
	r, err := boom.Compile(graph.WithObserver(s))
	require.NoError(t, err)
	_, _, err = r.Run(context.Background(), st{})
	require.Error(t, err)
	evs := s.Events()
	nodeEnd := evs[len(evs)-2]
	assert.Equal(t, TypeNodeEnd, nodeEnd.Type)
	assert.Contains(t, nodeEnd.Err, "kaboom")
	assert.Equal(t, "error", evs[len(evs)-1].Status)

	s2 := newSrv(t)
	lp := graph.New[st]("spin").AddNode("a", graph.Func(func(*st) error { return nil })).
		AddEdge(graph.Start, "a").AddConditionalEdge("a", func(*st) string { return "a" }, "a", graph.End)
	r2, err := lp.Compile(graph.WithObserver(s2), graph.WithMaxSteps(3))
	require.NoError(t, err)
	_, _, err = r2.Run(context.Background(), st{})
	require.ErrorIs(t, err, graph.ErrMaxSteps)
	assert.Equal(t, "max_steps", s2.Events()[len(s2.Events())-1].Status)
}

func TestAgentEventsJournaled(t *testing.T) {
	s := newSrv(t)
	i := graph.StepInfo{RunID: "r1", Node: "coder", Step: 2, Parent: "par"}
	ev := claudecli.StreamEvent{Type: "assistant", Message: json.RawMessage(`{"content":[{"type":"text","text":"hi"}]}`)}
	s.AgentEvent(context.Background(), i, ev)
	ev.Raw = []byte(`{"type":"result","result":"done","extra":1}`)
	s.AgentEvent(context.Background(), i, ev)
	ev.Raw = []byte(`not json`)
	s.AgentEvent(context.Background(), i, ev)
	evs := s.Events()
	require.Len(t, evs, 3)
	assert.Equal(t, "coder", evs[0].Node)
	assert.Equal(t, 2, evs[0].Step)
	assert.Equal(t, "par", evs[0].Parent)
	assert.JSONEq(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`, string(evs[0].Stream))
	assert.JSONEq(t, `{"type":"result","result":"done","extra":1}`, string(evs[1].Stream), "raw line wins (keeps unmodeled fields)")
	assert.Contains(t, string(evs[2].Stream), `"assistant"`, "invalid raw falls back to the typed marshal")
}

func TestJournalRoundTripReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	s := newSrv(t, WithJournal(path))
	r := askGraph(t, s)
	go func() {
		require.Eventually(t, func() bool { return s.Answer("ok") == nil }, 2*time.Second, 5*time.Millisecond)
	}()
	_, _, err := RunInteractive(context.Background(), r, st{}, s)
	require.NoError(t, err)
	require.NoError(t, s.Err())
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "idempotent")

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtimeUnix() {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	// journal never overwrites an existing one
	_, err = New(WithJournal(path))
	require.Error(t, err)

	for _, p := range []string{path, dir} { // file or runs dir
		t.Run(filepath.Base(p), func(t *testing.T) {
			rp, err := NewReplay(p)
			require.NoError(t, err)
			got := rp.Events()
			want := s.Events()
			require.Equal(t, len(want), len(got))
			for i := range want {
				assert.Equal(t, want[i].Type, got[i].Type)
				assert.Equal(t, want[i].Seq, got[i].Seq)
				assert.Equal(t, want[i].Question, got[i].Question)
			}
			ts := newTS(t, rp)
			c := openSSE(t, ts.URL, nil)
			for range want {
				c.next(t)
			}
			page := do(t, "GET", ts.URL+"/", "", nil)
			var sb strings.Builder
			_, _ = bufio.NewReader(page.Body).WriteTo(&sb)
			assert.Contains(t, sb.String(), `"readOnly":true`)
			// read-only: every POST is refused, even with a token
			for _, ep := range []string{"/api/answer", "/api/control", "/api/breakpoints"} {
				assert.Equal(t, 403, do(t, "POST", ts.URL+ep, `{}`, map[string]string{"X-Graphweb-Token": rp.Token()}).StatusCode)
			}
			assert.Equal(t, 200, do(t, "GET", ts.URL+"/api/topology", "", nil).StatusCode)
		})
	}
}

func runtimeUnix() bool { return os.PathSeparator == '/' }

func TestReplayErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := NewReplay(filepath.Join(dir, "nope"))
	require.Error(t, err)
	_, err = NewReplay(dir) // dir without journal.jsonl
	require.ErrorContains(t, err, "graphweb.jsonl")
	bad := filepath.Join(dir, "bad.jsonl")
	require.NoError(t, os.WriteFile(bad, []byte("{\"type\":\"x\"}\nnot json\n"), 0o600))
	_, err = NewReplay(bad)
	require.ErrorContains(t, err, "line 2")
	// a foreign journal.jsonl (someone else's notes) is rejected, graphweb.jsonl wins over it
	notes := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(notes, "journal.jsonl"), []byte(`{"unix_ms":1,"text":"hi"}`+"\n"), 0o600))
	_, err = NewReplay(notes)
	require.ErrorContains(t, err, "not a graphweb event")
	require.NoError(t, os.WriteFile(filepath.Join(notes, "graphweb.jsonl"), []byte(`{"type":"graph_start","run":"r"}`+"\n"), 0o600))
	rp0, err := NewReplay(notes)
	require.NoError(t, err)
	assert.Len(t, rp0.Events(), 1)
	empty := filepath.Join(dir, "empty.jsonl")
	require.NoError(t, os.WriteFile(empty, []byte("\n\n"), 0o600))
	_, err = NewReplay(empty)
	require.ErrorContains(t, err, "empty")
	// a huge line and a missing trailing newline are fine
	big := filepath.Join(dir, "big.jsonl")
	line := `{"type":"agent_event","run":"r","node":"n","stream":{"x":"` + strings.Repeat("a", 3<<20) + `"}}`
	require.NoError(t, os.WriteFile(big, []byte(line), 0o600))
	rp, err := NewReplay(big)
	require.NoError(t, err)
	assert.Len(t, rp.Events(), 1)
}

func TestJournalCreateFailure(t *testing.T) {
	_, err := New(WithJournal(filepath.Join(t.TempDir(), "missing", "j.jsonl")))
	require.Error(t, err)
}

func TestDiffAndTruncate(t *testing.T) {
	big := strings.Repeat("é", 3000) // 6000 bytes
	d := diffState(json.RawMessage(`{"a":1,"b":"x","gone":true,"same":0}`),
		json.RawMessage(`{"a":2,"b":"x","new":"`+big+`","same":0,"obj":{"k":"`+strings.Repeat("z", 5000)+`"}}`))
	assert.JSONEq(t, `1`, string(d["a"].Before))
	assert.JSONEq(t, `2`, string(d["a"].After))
	assert.NotContains(t, d, "b")
	assert.NotContains(t, d, "same")
	assert.Nil(t, d["gone"].After)
	assert.JSONEq(t, `true`, string(d["gone"].Before))
	var s string
	require.NoError(t, json.Unmarshal(d["new"].After, &s))
	assert.Contains(t, s, "[truncated, 6000 bytes total]")
	assert.Less(t, len(s), 4200)
	assert.True(t, strings.HasPrefix(s, "éé"))
	var o string
	require.NoError(t, json.Unmarshal(d["obj"].After, &o), "oversized non-string becomes a truncated string")
	assert.Contains(t, o, "truncated")

	assert.Nil(t, diffState(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1}`)))
	nonObj := diffState(json.RawMessage(`[1]`), json.RawMessage(`[1,2]`))
	assert.Contains(t, nonObj, "$")
	assert.Nil(t, diffState(json.RawMessage(`[1]`), json.RawMessage(`[1]`)))
}

func TestStartServesAndShutsDown(t *testing.T) {
	s := newSrv(t)
	ctx, cancel := context.WithCancel(context.Background())
	url, err := s.Start(ctx, ":0") // empty host must bind loopback
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "http://127.0.0.1:"), url)
	resp, err := http.Get(url + "/api/breakpoints")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	cancel()
	require.Eventually(t, func() bool {
		c := http.Client{Timeout: 200 * time.Millisecond}
		r, err := c.Get(url + "/api/breakpoints")
		if err == nil {
			r.Body.Close()
		}
		return err != nil
	}, 3*time.Second, 20*time.Millisecond)

	_, err = s.Start(context.Background(), "no-port")
	require.Error(t, err)
	_, err = s.Start(context.Background(), "127.0.0.1:999999")
	require.Error(t, err)
	u2, err := s.Start(context.Background(), "")
	require.NoError(t, err)
	assert.Contains(t, u2, "127.0.0.1")
}

func TestStartExplicitHostAllowed(t *testing.T) {
	s := newSrv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 0.0.0.0 is "explicitly non-loopback": its name is then accepted as a Host.
	url, err := s.Start(ctx, "0.0.0.0:0")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "http://0.0.0.0:"))
}
