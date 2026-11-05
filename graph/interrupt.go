package graph

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInterrupt matches (errors.Is) every interrupt error: the one a node gets
// from Interrupt and the *Interrupted[S] that Run and Resume return.
var ErrInterrupt = errors.New("graph: interrupted")

type interruptError struct {
	Question string
	Items    []ReviewItem
}

func (e *interruptError) Error() string        { return "graph: interrupted: " + e.Question }
func (e *interruptError) Is(target error) bool { return target == ErrInterrupt }

// Interrupt returns the error a node returns to pause the run and ask a human
// question. Run then fails with *Interrupted; answer it through Resume.
func Interrupt(question string) error { return &interruptError{Question: question} }

// Checkpoint is everything needed to resume a paused run. It is plain data and
// JSON-serializable whenever S is, so it can be stored between requests.
type Checkpoint[S any] struct {
	// RunID is the run this checkpoint belongs to; Resume keeps it so
	// observers can tie the segments together.
	RunID string `json:"run_id,omitempty"`
	State S      `json:"state"`
	// Node is the node to re-run on Resume.
	Node string `json:"node"`
	// Step is the number of node executions completed before Node's
	// interrupted attempt (it counts against WithMaxSteps).
	Step int `json:"step"`
	// Path is the execution trace so far, including the interrupted attempt.
	Path []string `json:"path"`
	// Prior is what the run had accumulated before this checkpoint, so Resume
	// can return a cumulative Report.
	Prior Prior `json:"prior"`
}

// Prior is the report data carried across a pause.
type Prior struct {
	Steps    []Step        `json:"steps,omitempty"`
	Duration time.Duration `json:"duration"`
}

// Interrupted is the error Run and Resume return when a node asks a question.
// Extract it with errors.As.
type Interrupted[S any] struct {
	Question   string
	Node       string
	Checkpoint Checkpoint[S]
	// Items are the things to review when the node called InterruptReview.
	Items []ReviewItem
}

func (e *Interrupted[S]) Is(target error) bool { return target == ErrInterrupt }

// AsInterrupt extracts the *Interrupted[S] from err (a Run or Resume error),
// for callers that cannot name the unexported internals.
func AsInterrupt[S any](err error) (*Interrupted[S], bool) {
	var ie *Interrupted[S]
	if errors.As(err, &ie) {
		return ie, true
	}
	return nil, false
}

func (e *Interrupted[S]) Error() string {
	return fmt.Sprintf("graph: node %q is waiting for an answer: %s", e.Node, e.Question)
}

// Answer returns the human's answer when the current node is being re-run by
// Resume, and false on any ordinary execution.
func Answer(ctx context.Context) (string, bool) {
	a, ok := ctx.Value(answerKey{}).(string)
	return a, ok
}
