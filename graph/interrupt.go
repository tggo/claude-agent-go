package graph

import (
	"context"
	"fmt"
)

type interruptError struct{ Question string }

func (e *interruptError) Error() string { return "graph: interrupted: " + e.Question }

// Interrupt returns the error a node returns to pause the run and ask a human
// question. Run then fails with *Interrupted; answer it through Resume.
func Interrupt(question string) error { return &interruptError{Question: question} }

// Checkpoint is everything needed to resume a paused run. It is plain data and
// JSON-serializable whenever S is, so it can be stored between requests.
type Checkpoint[S any] struct {
	State S `json:"state"`
	// Node is the node to re-run on Resume.
	Node string `json:"node"`
	// Step is the number of node executions completed before Node's
	// interrupted attempt (it counts against WithMaxSteps).
	Step int `json:"step"`
	// Path is the execution trace so far, including the interrupted attempt.
	Path []string `json:"path"`
}

// Interrupted is the error Run and Resume return when a node asks a question.
// Extract it with errors.As.
type Interrupted[S any] struct {
	Question   string
	Node       string
	Checkpoint Checkpoint[S]
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
