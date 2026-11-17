package graphweb

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeControl records the calls the panel's buttons make; fail makes them refuse.
type fakeControl struct {
	mu    sync.Mutex
	calls []string
	fail  error
}

func (f *fakeControl) record(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.fail
}

func (f *fakeControl) Pause(_ context.Context, run string) error { return f.record("pause " + run) }
func (f *fakeControl) Continue(_ context.Context, run string) error {
	return f.record("continue " + run)
}
func (f *fakeControl) TakeOver(_ context.Context, run string) error {
	return f.record("takeover " + run)
}
func (f *fakeControl) Answer(_ context.Context, run, id, a string) error {
	return f.record("answer " + run + " " + id + " " + a)
}

func lastBabysitter(t *testing.T, s *Server) (string, BabysitterEvent) {
	t.Helper()
	evs := s.Events()
	require.NotEmpty(t, evs)
	ev := evs[len(evs)-1]
	require.Equal(t, TypeBabysitter, ev.Type)
	require.NotNil(t, ev.Babysitter)
	return ev.Run, *ev.Babysitter
}

// What the harness reports is journaled as babysitter events of the run.
func TestBabysitterEventsJournaled(t *testing.T) {
	s := newSrv(t)
	s.Babysitter("r1", BabysitterEvent{Kind: BabysitterWoke, Reason: "failed", Text: "boom"})
	s.Babysitter("r1", BabysitterEvent{Kind: BabysitterAsked, ID: "q1", Text: "delete?"})
	run, ev := lastBabysitter(t, s)
	assert.Equal(t, "r1", run)
	assert.Equal(t, BabysitterEvent{Kind: BabysitterAsked, ID: "q1", Text: "delete?"}, ev)
	assert.Equal(t, []string{TypeBabysitter, TypeBabysitter}, types(s.Events()))
}

// Every button reaches the control with the run (and the answer), and the
// person's action is journaled only after the control accepted it.
func TestBabysitterActions(t *testing.T) {
	ctx := context.Background()
	fc := &fakeControl{}
	s := newSrv(t, WithBabysitter(fc))
	for _, c := range []struct{ action, kind string }{
		{"pause", BabysitterPauseRequested},
		{"continue", BabysitterContinueRequested},
		{"takeover", BabysitterTakeover},
	} {
		require.NoError(t, s.BabysitterAct(ctx, "r1", c.action, "", ""))
		run, ev := lastBabysitter(t, s)
		assert.Equal(t, "r1", run)
		assert.Equal(t, BabysitterEvent{Kind: c.kind, By: "graphweb"}, ev)
	}
	require.NoError(t, s.BabysitterAct(ctx, "r1", "answer", "q1", "yes, delete"))
	_, ev := lastBabysitter(t, s)
	assert.Equal(t, BabysitterEvent{Kind: BabysitterAnswered, ID: "q1", Text: "yes, delete", By: "graphweb"}, ev)
	assert.Equal(t, []string{"pause r1", "continue r1", "takeover r1", "answer r1 q1 yes, delete"}, fc.calls)

	// Refused by the harness: the error says so and nothing is journaled.
	n := len(s.Events())
	fc.fail = errors.New("run is not paused")
	err := s.BabysitterAct(ctx, "r1", "continue", "", "")
	require.ErrorContains(t, err, "babysitter continue: run is not paused")
	assert.ErrorIs(t, err, fc.fail)
	assert.Len(t, s.Events(), n)

	// Malformed requests never reach the harness.
	fc.fail = nil
	calls := len(fc.calls)
	assert.ErrorContains(t, s.BabysitterAct(ctx, "", "pause", "", ""), "needs a run")
	assert.ErrorContains(t, s.BabysitterAct(ctx, "r1", "reboot", "", ""), "unknown babysitter action")
	assert.ErrorContains(t, s.BabysitterAct(ctx, "r1", "answer", "", "x"), "question id")
	assert.ErrorContains(t, s.BabysitterAct(ctx, "r1", "answer", "q1", ""), "non-empty answer")
	assert.Len(t, fc.calls, calls)

	// No control: the panel has no buttons.
	assert.ErrorIs(t, newSrv(t).BabysitterAct(ctx, "r1", "pause", "", ""), errNoBabysitter)
}

func TestBabysitterEndpoint(t *testing.T) {
	fc := &fakeControl{}
	s := newSrv(t, WithBabysitter(fc))
	ts := newTS(t, s)
	hdr := map[string]string{"X-Graphweb-Token": s.Token(), "Content-Type": "application/json"}
	post := func(body string, h map[string]string) (int, string) {
		resp := do(t, "POST", ts.URL+"/api/babysitter", body, h)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, _ := post(`{"run":"r1","action":"answer","id":"q1","answer":"go"}`, hdr)
	assert.Equal(t, 200, code)
	assert.Equal(t, []string{"answer r1 q1 go"}, fc.calls)

	code, _ = post(`{"run":"r1","action":"pause"}`, map[string]string{"Content-Type": "application/json"})
	assert.Equal(t, 403, code, "no token")
	code, _ = post(`not json`, hdr)
	assert.Equal(t, 400, code)
	code, _ = post(`{"run":"r1","action":"reboot"}`, hdr)
	assert.Equal(t, 400, code)
	fc.fail = errors.New("busy")
	code, body := post(`{"run":"r1","action":"pause"}`, hdr)
	assert.Equal(t, 409, code)
	assert.Contains(t, body, "busy")

	bare := newSrv(t)
	code, _ = do(t, "POST", newTS(t, bare).URL+"/api/babysitter", `{"run":"r1","action":"pause"}`,
		map[string]string{"X-Graphweb-Token": bare.Token(), "Content-Type": "application/json"}).StatusCode, ""
	assert.Equal(t, 404, code)
}

// The page tells the UI whether the buttons work.
func TestBabysitterBootFlag(t *testing.T) {
	page := func(s *Server) string {
		b, _ := io.ReadAll(do(t, "GET", newTS(t, s).URL+"/", "", nil).Body)
		return string(b)
	}
	assert.Contains(t, page(newSrv(t, WithBabysitter(&fakeControl{}))), `"babysitter":true`)
	assert.Contains(t, page(newSrv(t)), `"babysitter":false`)
}

// Babysitter events survive the journal, and a replay shows them read-only.
func TestBabysitterReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournal)
	s := newSrv(t, WithJournal(path), WithBabysitter(&fakeControl{}))
	s.Babysitter("r1", BabysitterEvent{Kind: BabysitterWoke, Reason: "crashed"})
	require.NoError(t, s.BabysitterAct(context.Background(), "r1", "takeover", "", ""))
	require.NoError(t, s.Close())

	rp, err := NewReplay(path)
	require.NoError(t, err)
	evs := rp.Events()
	require.Len(t, evs, 2)
	assert.Equal(t, BabysitterEvent{Kind: BabysitterWoke, Reason: "crashed"}, *evs[0].Babysitter)
	assert.Equal(t, BabysitterTakeover, evs[1].Babysitter.Kind)
	assert.ErrorIs(t, rp.BabysitterAct(context.Background(), "r1", "pause", "", ""), errNoBabysitter)
	resp := do(t, "POST", newTS(t, rp).URL+"/api/babysitter", `{"run":"r1","action":"pause"}`,
		map[string]string{"X-Graphweb-Token": rp.Token(), "Content-Type": "application/json"})
	assert.Equal(t, 403, resp.StatusCode, "replay is read-only")
	b, _ := io.ReadAll(do(t, "GET", newTS(t, rp).URL+"/", "", nil).Body)
	assert.True(t, strings.Contains(string(b), `"babysitter":false`))
}
