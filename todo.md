# todo / roadmap

## North star: a remote/distributed Claude Code runner

The Go-SDK space is crowded (15+ libraries; the leader has ~160★ and already
covers query/client, permissions, hooks, and in-process tools). Competing
head-on as "yet another SDK" is a losing position.

Where almost nobody is playing: **running Claude Code agents off the local box,
at scale.** That is our wedge. Re-position from "a Go SDK" to:

> **A runner for Claude Code agents across machines and containers** — same typed
> Go API, but the agent executes locally, in a container, over SSH, or on a
> fleet, with isolated git worktrees for safe parallelism.

We already have the two pieces nobody else ships as first-class:
- `transport` — Local / DockerExec / SSH (SSH is unique among the top repos).
- `workspace` — git worktrees for parallel, isolated agent runs.

### Roadmap toward the niche

- [x] **Reframe the README** around remote + parallel execution. _(done)_
- [ ] **Kubernetes transport** — `transport.Kubernetes{}` → `kubectl exec` into
      a pod (or a Job per run). The obvious next transport after Docker/SSH.
- [ ] **Remote workspace** — make `workspace` git operations run *through the
      transport*, so worktrees/clone/commit happen on the remote side, not just
      locally. This unlocks the full remote-FS workflow.
- [x] **Fleet/pool API** — done, see `fleet` package + `examples/fleet`.
- [ ] **Reverse tunnel for in-process tools** — auto port-forward (SSH `-R` /
      docker host networking) so `tools` (local HTTP MCP) are reachable from a
      remote agent. Removes the current caveat.
- [ ] **Teardown per transport** — guaranteed remote cleanup (ssh `-tt` +
      remote pkill / `docker stop`), not just killing the local proxy.
- [ ] **Health/preflight** — verify the target has `claude` + creds before
      burning a run (we already do repo preflight; extend to transports).
- [ ] **Observability** — per-run cost/turns/duration aggregated across a fleet.

### Keep (already differentiating)
- SSH transport, worktree isolation, typed generic tools (`Register[In,Out]`),
  single static binary, context cancellation throughout.

### Explicitly NOT chasing
- Re-implementing the agent loop in pure Go (armatrix's bet) — out of scope; we
  wrap the CLI on purpose.
- Feature-by-feature parity races with the leader.

---

## Competitor scan (2026-06-30)

Scanned the top Go SDKs: severity1 (~160★), lancekrogers (42★), character-ai
(30★), armatrix (9★, pure-Go/no-subprocess).

### Applied in this pass
- **Callback timeout** — a hung `CanUseTool`/hook callback no longer stalls the
  turn forever (`client.Config.CallbackTimeout`, default 60s). _(character-ai bug)_
- **Typed errors** — `CLINotFoundError`, `ProcessError{ExitCode,Stderr}`,
  `TimeoutError` + `IsCLINotFound/IsProcessError/IsTimeout`. _(severity1 #73)_
- **`WithMaxBufferSize` with `<=0` guard** — configurable scan buffer, pre-empts
  the negative-buffer scanner panic. _(severity1 #122)_
- **`WithStderrCallback`** — live stderr line capture during RunStream. _(severity1 #53)_
- **`mcp.ServerConfig.AlwaysLoad`** — CLI 2.1.121 field. _(severity1 #119)_
- **`Input.ForkSession`** → `--fork-session`. _(lancekrogers #8)_
- **Structured output capture** + lenient parse (`ExecutionMetadata.StructuredOutput`,
  `StreamEvent.Raw`, `IsRateLimit()`). _(severity1 #18/#126)_
- **Fluent hooks builder** — `NewHooks().PreToolUse("Bash", fn).Build()`. _(character-ai)_

### Roadmap (fleet/remote niche — highest value first)
- [x] **Retry + error classification + jittered backoff** — `runner.RunJSONWithRetry`
      + `RetryPolicy` (honors rate-limit retry-after; accumulates cost across
      attempts; `MaxSpendUSD` caps retry spend). _(done)_
- [x] **BudgetTracker** — `budget.Tracker`: cross-run cap + warn/exceed callbacks,
      per-session spend, CanSpend gate; 100% covered. _(done)_
- [x] **`fleet`** — worker pool + `SharedTaskList` (atomic claim + dependency DAG) +
      tagged `OnResult` stream; one worktree per task; cost aggregation + soft
      spend cap; optional per-task retry. _(done)_
- [x] **`transport.DockerRun`** — ephemeral `docker run --rm --init` container with
      network/memory/cpu limits; --rm + signal-proxy teardown; verified live. _(done)_
- [ ] **Per-transport teardown** — guaranteed remote cleanup (ssh remote pkill /
      `docker stop`), not just killing the local proxy. _(character-ai)_
- [ ] **`transport.Kubernetes`** — `kubectl exec` / Job-per-run.
- [ ] **SSE bridge** (`httpx` subpackage) — expose a RunStream over `text/event-stream`
      for remote dashboards. _(character-ai sse.go)_
- [ ] **Permission-string parser/validator** — validate `"Bash(git log:*)"` /
      `"Write(src/**)"` instead of passing `AllowedTools` raw. _(lancekrogers)_
- [ ] **Typed `RateLimitInfo`** — model the rate_limit_event fields once the schema
      is pinned (we surface the raw line today). _(severity1 #23)_
- [ ] **Min-CLI-version warning** — compare parsed `ClaudeVersion` to a floor. _(severity1 #78)_

### Deliberately skipped
- Pure-Go no-subprocess core (armatrix), in-process chat providers (character-ai),
  plugin lifecycle bus + brittle substring permission callbacks (lancekrogers),
  session-CRUD Python-parity churn (severity1) — all orthogonal to wrapping the CLI
  or weaker than what we already have.
