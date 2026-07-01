package fleet

import "sync"

// taskList is a dependency-aware work queue shared by the worker goroutines. A
// worker claims the next task whose dependencies have all succeeded; if none are
// ready yet but tasks are still running, it waits. When nothing is runnable and
// nothing is in flight, claim returns false and the worker exits.
type taskList struct {
	mu   sync.Mutex
	cond *sync.Cond

	pending map[string]Task // not yet claimed
	succeed map[string]bool // completed task id -> succeeded
	running int             // tasks currently claimed but not completed
}

func newTaskList(tasks []Task) *taskList {
	tl := &taskList{
		pending: make(map[string]Task, len(tasks)),
		succeed: make(map[string]bool),
	}
	tl.cond = sync.NewCond(&tl.mu)
	for _, t := range tasks {
		tl.pending[t.ID] = t
	}
	return tl
}

// ready reports whether all of t's dependencies have completed successfully.
func (tl *taskList) ready(t Task) bool {
	for _, dep := range t.DependsOn {
		if !tl.succeed[dep] {
			return false
		}
	}
	return true
}

// claim returns the next runnable task, blocking while tasks are in flight that
// might unblock dependencies. Returns ok=false when no task can ever run for
// this worker (queue drained, or remaining tasks are permanently blocked).
func (tl *taskList) claim() (Task, bool) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for {
		for id, t := range tl.pending {
			if tl.ready(t) {
				delete(tl.pending, id)
				tl.running++
				return t, true
			}
		}
		if len(tl.pending) == 0 {
			// nothing left; wake any peers also waiting so they can exit.
			tl.cond.Broadcast()
			return Task{}, false
		}
		if tl.running == 0 {
			// pending tasks remain but none are ready and nothing is running to
			// unblock them (failed deps / cycle): unrunnable.
			tl.cond.Broadcast()
			return Task{}, false
		}
		tl.cond.Wait()
	}
}

// complete marks a claimed task finished and wakes waiters (a newly satisfied
// dependency may make other tasks runnable).
func (tl *taskList) complete(id string, ok bool) {
	tl.mu.Lock()
	tl.running--
	tl.succeed[id] = ok
	tl.cond.Broadcast()
	tl.mu.Unlock()
}

// unfinished returns the IDs of tasks that were never claimed.
func (tl *taskList) unfinished() []string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	ids := make([]string, 0, len(tl.pending))
	for id := range tl.pending {
		ids = append(ids, id)
	}
	return ids
}
