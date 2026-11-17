package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrPaused matches (errors.Is) the error Run, Resume and Recover return when
// the run stopped between nodes because its PauseSignal asked it to.
var ErrPaused = errors.New("graph: paused")

// PausedError is the error a run returns when it stopped on a pause request.
// The run's log ends with a pause record (no run_end): Recover continues it at
// Node, with the state of the last commit.
type PausedError struct {
	RunID string
	// Node is the node the run will continue at; it has not started.
	Node string
	// Step is the number of node executions completed before the pause.
	Step int
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("graph: run %q paused before node %q", e.RunID, e.Node)
}

func (e *PausedError) Is(target error) bool { return target == ErrPaused }

// PauseSignal lets something outside the run (a person, a babysitter, another
// process) stop it cleanly between two nodes. The run asks PauseRequested
// before every top-level node; when it says yes, the run logs a pause record
// and returns *PausedError instead of failing. Recover of a paused run calls
// ClearPause first, then continues.
//
// A node that is already running is not interrupted: the pause takes effect
// after it commits. A PauseRequested error fails the run (Recover retries it).
type PauseSignal interface {
	PauseRequested(ctx context.Context, runID string) (bool, error)
	ClearPause(ctx context.Context, runID string) error
}

// WithPauseSignal makes every Run, Resume and Recover check sig before each
// node. It requires WithCheckpointStore: a paused run is continued from its
// log.
func WithPauseSignal(sig PauseSignal) CompileOption {
	return func(o *options) { o.pause = sig }
}

// FilePause is a PauseSignal backed by marker files: a pause for runID is
// requested while <Dir>/<runID>.pause exists. Any process that sees Dir can
// request one (Request, or `touch`), so it works across processes on one
// machine. Dir may be a FileStore's directory.
type FilePause struct {
	Dir string
}

func (f FilePause) path(runID string) (string, error) {
	if !validRunID.MatchString(runID) {
		return "", fmt.Errorf("graph: invalid run id %q", runID)
	}
	return filepath.Join(f.Dir, runID+".pause"), nil
}

// Request asks runID to pause before its next node.
func (f FilePause) Request(runID string) error {
	p, err := f.path(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return fmt.Errorf("graph: pause: %w", err)
	}
	return os.WriteFile(p, nil, 0o600)
}

// PauseRequested implements PauseSignal.
func (f FilePause) PauseRequested(_ context.Context, runID string) (bool, error) {
	p, err := f.path(runID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("graph: pause: %w", err)
	}
}

// ClearPause implements PauseSignal. Clearing a pause that was not requested
// is not an error.
func (f FilePause) ClearPause(_ context.Context, runID string) error {
	p, err := f.path(runID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("graph: pause: %w", err)
	}
	return nil
}

// MemPause is an in-process PauseSignal: a pause flag per run. Use it when
// the code that asks for the pause runs in the same process as the graph (a
// UI handler, a supervisor goroutine, a test). It does not survive a restart
// and other processes cannot see it; use FilePause, or a PauseSignal over your
// own database, for that. The zero value is ready to use.
type MemPause struct {
	mu   sync.Mutex
	runs map[string]bool
}

// Request asks runID to pause before its next node.
func (m *MemPause) Request(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		m.runs = map[string]bool{}
	}
	m.runs[runID] = true
}

// PauseRequested implements PauseSignal.
func (m *MemPause) PauseRequested(_ context.Context, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[runID], nil
}

// ClearPause implements PauseSignal.
func (m *MemPause) ClearPause(_ context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, runID)
	return nil
}

// checkPause is called by the run loop before node cur. It returns a
// *PausedError (after logging the pause record) when a pause was requested.
func (r *Runnable[S]) checkPause(ctx context.Context, rec *recorder, s *S, cur string, step int) error {
	if r.cfg.pause == nil {
		return nil
	}
	ok, err := r.cfg.pause.PauseRequested(ctx, rec.runID)
	if err != nil {
		return fmt.Errorf("graph %q: pause signal before node %q: %w", r.name, cur, err)
	}
	if !ok {
		return nil
	}
	if rec.ck != nil {
		if err := rec.ck.append(ctx, Record{Kind: KindPause, Node: cur, Step: step, State: snapshot(s), Next: cur}); err != nil {
			return err
		}
	}
	return &PausedError{RunID: rec.runID, Node: cur, Step: step}
}
