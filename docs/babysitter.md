# Babysitter: looking after durable graph runs

A **babysitter** is an agent that takes over when a graph run stops. It finds out
why, fixes what it can and continues the run, or hands the problem to a person
with a note. The idea comes from aether (`internal/babysitter`).

The SDK does **not** ship a babysitter. A babysitter needs a process to live in,
a Claude session, limits, retry counts and a notification channel, and those are
infrastructure, which this SDK keeps out of. The SDK gives you the parts a
babysitter is built from, plus a panel for it in `graphweb`. This page is for
whoever writes a harness on the SDK and wants to add a babysitter: which parts
to use, how they fit together, and what to leave to a person.

## What you build on

| Need | SDK part |
|---|---|
| The run's history and state survive the process | `graph.WithCheckpointStore(store)`: `FileStore`, `MemStore`, or your own (`examples/pgstore`) |
| See which runs need help | `store.List(ctx)` → `RunSummary.Status`: `failed`, `crashed`, `paused`, `running`, `done` (`cag runs <dir>`) |
| Read what happened | `store.Load(ctx, runID)` → `[]graph.Record`, in order. The last `node_commit` holds the state, `run_end.Err` the error (`cag runs <dir> <runID>`) |
| Stop a run cleanly between nodes | `graph.WithPauseSignal(sig)` with `graph.FilePause` (across processes) or `graph.MemPause` (in-process); `cag pause <dir> <runID>` |
| Continue a run | `run.Recover(ctx, runID)`: re-runs the node that failed, crashed or was next, and keeps everything committed before it |
| Continue with a changed state | `run.Fork(ctx, runID, seq, newID, edit)`: copies the history up to a commit, applies `edit`, continues as a new run, and leaves the original as it was |
| Answer a node's question | `graph.Interrupt` → `*graph.Interrupted[S]` → `run.Resume(ctx, cp, answer)` |
| Re-runs must not repeat side effects | `graph.IdempotencyKey(ctx)`: the same key for every retry of a step |
| Show it to a person | `graphweb`: `Server.Babysitter(...)` events and `graphweb.WithBabysitter(control)` (see below) |

## When to wake it

Look at each run's `RunSummary` (`store.List`), from a loop in your harness or
after the run returns:

| Status | What happened | What the babysitter usually does |
|---|---|---|
| `failed` | A node returned an error; the log ends in `run_end` with `Err` | Read `Err` and the committed state, fix the cause, `Recover` |
| `crashed` | No record for `graph.StaleAfter` and no `run_end`: the process died | Check that the workspace is intact, then `Recover` (an agent node resumes its CLI session) |
| `paused`, `Question` set | A node called `Interrupt` and waits for an answer | Answer it if the answer is within its rules (`Recover` returns `*Interrupted`, then `Resume`), otherwise ask a person |
| `paused`, `Question` empty | Someone paused it with a `PauseSignal` | Usually nothing: a person or the babysitter paused it on purpose. `Recover` when the reason is resolved |

`crashed` is a guess based on time. A node that runs longer than `StaleAfter`
without writing anything also looks crashed, so check that the process is
really gone before you `Recover`. Two processes recovering the same run write
into the same log. The SDK has no lease that would prevent it, so your harness
must make sure only one process works on a run.

## What it may do

Give it a short list of verbs, and leave the rest to a person:

- **Recover** a failed, crashed or paused run, after fixing the cause.
- **Fork** from an earlier commit with an edited state. This is a rollback or a
  "try again with X", and the original run stays as it was for comparison.
- **Pause** a running run that it sees going wrong (`FilePause.Request`). The
  node in flight finishes and commits first. A pause takes effect between nodes
  and never stops a node in the middle.
- **Answer** an `Interrupt` whose answer is covered by its instructions.
- **Fix the code and rebuild**, then `Recover` with the new binary: the
  debugging loop below.

Keep these for a person:

- **A change of the graph's shape.** `Recover` refuses it with
  `ErrTopologyChanged` unless the new build compiles with
  `AllowTopologyChange()`. Make that a person's decision: committed state may
  not fit the new graph.
- **A problem it has tried too often.** Count wakes per problem in your
  harness. aether's default is 3, then the problem goes to a person.
- **Questions that are not its to answer:** money, deleting data, anything
  irreversible.

## The debugging loop: pause, fix, rebuild, continue

1. The run fails in node `n3`, or the babysitter pauses it before `n3` because
   it knows `n3` is broken. Everything up to `n2` is committed.
2. It reads the state at the last commit (`store.Load`, the last `node_commit`
   or `pause` record) and the error.
3. It fixes the node's code and rebuilds the binary.
4. The **new** binary calls `Recover(ctx, runID)` on the same store. `n1` and
   `n2` do not run again; `n3` runs with the fix; a pause marker is removed.

`graph/debugbin_test.go` tests exactly this with two real builds of
`graph/testdata/debugbin` (`n3` broken by default, fixed with `-tags fixed`),
for a failed run and for a run paused from another process.

Rules that keep it safe:

- **The node that failed runs again from its top.** Make side effects
  idempotent with `IdempotencyKey(ctx)`.
- **Changing a node's body is fine.** The topology hash covers nodes, edges and
  their kinds, not code.
- **Only the graph state `S` is in the log.** Files the nodes wrote, git
  worktrees and the agent's Claude session (`~/.claude/projects/...`) live on
  the machine. Recover on the same machine, in the same workspace.
- **Edit state with Fork.** You cannot change the state in place before
  `Recover`, because the log is append-only: `Fork(runID, seq, newID, edit)`
  continues from an edited copy.

## A minimal babysitter loop

```go
store, _ := graph.FileStore("./checkpoints")
sig := graph.FilePause{Dir: "./checkpoints"}
run, _ := g.Compile(graph.WithCheckpointStore(store), graph.WithPauseSignal(sig))

runs, _ := store.List(ctx)
for _, r := range runs {
    switch {
    case r.Status == graph.StatusFailed || r.Status == graph.StatusCrashed:
        // wake your agent with r.Err and the state of the last commit; it fixes,
        // rebuilds if needed, and the (new) binary runs:
        _, _, err := run.Recover(ctx, r.RunID)
        _ = err // ErrPaused, *Interrupted, a new failure: the next round looks again
    case r.Status == graph.StatusPaused && r.Question != "":
        // an Interrupt: answer it, or ask a person
    }
}
```

The agent itself can be a `client.New` session or a `runner.Run` call with a
prompt that names the run ID, the store directory, the error and the verbs it
may use. Restrict its tools with a `permissions.Policy`. Cap its spend with
`runner.WithMaxBudgetUSD` / `SpendCap`, and bound its time with a context
deadline.

## Showing it in graphweb

`graphweb` has a babysitter panel for the run that is shown: what the
babysitter is doing, its question to a person, and buttons for the person.
The harness reports what the babysitter does, and graphweb calls the harness
back when the person acts. graphweb never runs the babysitter.

**Report** with `Server.Babysitter(runID, ev)`:

```go
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterWoke, Reason: "failed", Text: r.Err})
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterSaid, Text: "n3 indexes past the slice; fixing"})
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterDoing, Text: "go build ./cmd/app"})
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterAsked, ID: "q1", Text: "Delete the 3 stale fixtures?"})
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterHandover, Text: "needs a schema change; notes in NOTES.md"})
srv.Babysitter(runID, graphweb.BabysitterEvent{Kind: graphweb.BabysitterIdle, Text: "recovered; run continues"})
```

| Kind | Meaning | Panel state |
|---|---|---|
| `woke` | A wake started; `Reason` says why (`failed`, `crashed`, `paused`, `interrupt`, `answer`, ...) | working |
| `said` | What it says, in a sentence | (feed) |
| `doing` | The tool or command it runs now | (feed) |
| `asked` | A question to a person; `ID` names it | asking: shows the question with an answer box |
| `answered` | A person answered `ID` (graphweb emits it after a successful answer) | working |
| `handover` | It gives the decision to a person, with a note | handed over |
| `takeover` | A person took the decision away (graphweb emits it) | taken over |
| `pause_requested`, `continue_requested` | A person pressed the button (graphweb emits them); the panel state does not change, the run's own events show what happens | (feed) |
| `idle` | Its session ended until the next wake | idle |

The events are journaled like every other event, so `cag web --replay` shows
them later, read-only.

**Act**: give the server a `BabysitterControl` and the panel shows its buttons.
Each one is a POST to `/api/babysitter` with the server's token:

```go
type control struct{ sig graph.FilePause /* + your harness */ }

func (c control) Pause(ctx context.Context, runID string) error    { return c.sig.Request(runID) }
func (c control) Continue(ctx context.Context, runID string) error { return harness.Recover(ctx, runID) }
func (c control) TakeOver(ctx context.Context, runID string) error { return harness.StopBabysitter(ctx, runID) }
func (c control) Answer(ctx context.Context, runID, questionID, answer string) error {
    return harness.Deliver(ctx, runID, questionID, answer) // and wake it again
}

srv, _ := graphweb.New(graphweb.WithBabysitter(control{sig: graph.FilePause{Dir: dir}}))
```

`Continue` should return quickly: start `Recover` in a goroutine or another
process, and let the run's own events show the progress.

A runnable demo, with a scripted stand-in for the agent and no model:
`go run ./examples/babysitter`, then open the printed URL. The run fails, the
babysitter asks you in the panel, and on "yes" it patches the node and recovers
the run. On "no" it hands the run over to you, and "Continue run" recovers it
yourself. `-auto` runs it headless.
