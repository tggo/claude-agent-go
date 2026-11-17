package graphweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// TypeBabysitter is the event type of everything the babysitter panel shows:
// Event.Babysitter says what happened.
const TypeBabysitter = "babysitter"

// Babysitter event kinds (BabysitterEvent.Kind). The harness reports woke,
// said, doing, asked, handover and idle; graphweb itself reports the person's
// actions: answered, takeover, pause_requested and continue_requested.
const (
	BabysitterWoke              = "woke"
	BabysitterSaid              = "said"
	BabysitterDoing             = "doing"
	BabysitterAsked             = "asked"
	BabysitterAnswered          = "answered"
	BabysitterHandover          = "handover"
	BabysitterTakeover          = "takeover"
	BabysitterPauseRequested    = "pause_requested"
	BabysitterContinueRequested = "continue_requested"
	BabysitterIdle              = "idle"
)

// BabysitterEvent is one thing a run's babysitter did, or a person did to it.
// See docs/babysitter.md.
type BabysitterEvent struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"` // woke: why (failed, crashed, paused, interrupt, answer, ...)
	Text   string `json:"text,omitempty"`   // said, doing, asked, answered, handover, idle
	ID     string `json:"id,omitempty"`     // asked, answered: the question
	By     string `json:"by,omitempty"`     // answered, takeover, *_requested: who acted
}

// BabysitterControl is how the babysitter panel's buttons reach the harness
// that runs the babysitter. graphweb never runs one itself: it calls these
// and journals the person's action when the call succeeds. Every method gets
// the request's context and should return quickly (start long work, such as
// Recover, in the background, under the harness's own context).
//
// runID comes from the page: check that it is a run your harness looks after,
// and refuse Continue while a Recover of that run is already going (two
// Recovers would append to one log). examples/babysitter does both.
type BabysitterControl interface {
	// Pause asks the run to stop before its next node (e.g. FilePause.Request).
	Pause(ctx context.Context, runID string) error
	// Continue resumes a stopped run (e.g. Recover in the background).
	Continue(ctx context.Context, runID string) error
	// TakeOver stops the babysitter: the decision is the person's now.
	TakeOver(ctx context.Context, runID string) error
	// Answer delivers the person's answer to the babysitter's question.
	Answer(ctx context.Context, runID, questionID, answer string) error
}

// WithBabysitter connects the babysitter panel's buttons to c. Without it the
// panel only shows what Server.Babysitter reports.
func WithBabysitter(c BabysitterControl) Option {
	return func(cfg *config) { cfg.babysitter = c }
}

// Babysitter journals what runID's babysitter did, for the panel. Call it from
// the harness that runs the babysitter.
func (s *Server) Babysitter(runID string, ev BabysitterEvent) {
	s.emit(Event{Run: runID, Type: TypeBabysitter, Babysitter: &ev})
}

// MaxBabysitterAnswer bounds a person's answer to the babysitter: it is
// journaled and handed to the harness.
const MaxBabysitterAnswer = 16 << 10

// errNoBabysitter: a babysitter action arrived but no BabysitterControl is set.
var errNoBabysitter = errors.New("graphweb: no babysitter is connected (graphweb.WithBabysitter)")

// refusedError is a BabysitterControl method's error: the request was valid,
// the harness said no.
type refusedError struct {
	action string
	err    error
}

func (e *refusedError) Error() string {
	return "graphweb: babysitter " + e.action + ": " + e.err.Error()
}
func (e *refusedError) Unwrap() error { return e.err }

// BabysitterAct applies a person's action to runID's babysitter (what POST
// /api/babysitter does): "pause", "continue", "takeover", or "answer" with the
// question id and the answer. On success it journals the action.
func (s *Server) BabysitterAct(ctx context.Context, runID, action, questionID, answer string) error {
	c := s.babysitter
	if c == nil {
		return errNoBabysitter
	}
	if runID == "" {
		return errors.New("graphweb: babysitter action needs a run")
	}
	s.mu.Lock()
	known := s.seen[runID]
	s.mu.Unlock()
	if !known {
		return fmt.Errorf("graphweb: babysitter action for run %q, which this server has no events of", runID)
	}
	const by = "graphweb"
	var ev BabysitterEvent
	var err error
	switch action {
	case "pause":
		ev = BabysitterEvent{Kind: BabysitterPauseRequested, By: by}
		err = c.Pause(ctx, runID)
	case "continue":
		ev = BabysitterEvent{Kind: BabysitterContinueRequested, By: by}
		err = c.Continue(ctx, runID)
	case "takeover":
		ev = BabysitterEvent{Kind: BabysitterTakeover, By: by}
		err = c.TakeOver(ctx, runID)
	case "answer":
		if questionID == "" || answer == "" {
			return errors.New("graphweb: an answer needs the question id and a non-empty answer")
		}
		if len(answer) > MaxBabysitterAnswer {
			return fmt.Errorf("graphweb: an answer is limited to %d bytes", MaxBabysitterAnswer)
		}
		ev = BabysitterEvent{Kind: BabysitterAnswered, ID: questionID, Text: answer, By: by}
		err = c.Answer(ctx, runID, questionID, answer)
	default:
		return fmt.Errorf("graphweb: unknown babysitter action %q (want pause, continue, takeover or answer)", action)
	}
	if err != nil {
		return &refusedError{action: action, err: err}
	}
	s.Babysitter(runID, ev)
	return nil
}

// handleBabysitter is POST /api/babysitter: 404 without a BabysitterControl,
// 400 for a malformed request, 409 when the harness refused.
func (s *Server) handleBabysitter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Run    string `json:"run"`
		Action string `json:"action"`
		ID     string `json:"id"`
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.BabysitterAct(r.Context(), in.Run, in.Action, in.ID, in.Answer); err != nil {
		code := http.StatusBadRequest
		var re *refusedError
		switch {
		case errors.Is(err, errNoBabysitter):
			code = http.StatusNotFound
		case errors.As(err, &re):
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
