# CLAUDE.md — claude-agent-go

A Go SDK that drives the **Claude Code CLI** (`claude`) as a subprocess: a thin,
typed client around the binary, plus an interactive session client, in-process
Go tools, permission/hook callbacks, and inline subagents.

Module: `github.com/tggo/claude-agent-go` · Go 1.27.

## What this is (and isn't)

- It is a **client around the `claude` binary** — it builds CLI argv, streams the
  prompt over stdin, and parses `--output-format json`/`stream-json` back into
  typed Go structs. Same architecture as the Python `claude-agent-sdk`.
- It does **not** reimplement the agent loop, context management, or model
  calls — those live inside the CLI.
- It is **infrastructure-free**: no orchestration framework, no VCS auth, no
  third-party logger baked in. Those are the caller's job and attach via seams
  (a `ProgressFunc` callback, a stdlib `*slog.Logger`).

## Package map

| Package | Responsibility |
|---|---|
| `claudecli` | Output types + parsers: `ParseOutput`, `ParseStreamLine`, `ExtractTextFromStream`, typed content blocks, partial-stream deltas, `CheckInvariants` (tool_use/result pairing, one result, one session). No deps. |
| `runner` | One-shot execution: `Run` (`--print`), `RunJSON`, `RunStream` (+ `ProgressFunc`), retries with `Resume`, plugins (`SpendCap`), `Effective(Input)`, `Preflight`/`WithPreflight`. Process-group teardown on cancel/timeout. |
| `client` | Interactive multi-turn session over bidirectional stream-json: `New`/`Query`/`Interrupt`/`Close`, plus the control protocol: `can_use_tool` callbacks, hooks, inline agents, skills allowlist, `OnAskUser`. |
| `permissions` | Ordered Allow/Deny/Ask rule `Policy` for `can_use_tool` (`Policy.CanUseTool()`), Claude Code pattern syntax, compound-safe Bash matching. Fuzzed. Known limit: Deny does not see through `sudo`/`env`/`xargs`. |
| `tools` | Expose Go functions to the agent as MCP tools, in-process: `Serve`, `Register[In,Out]`, `Registry.ServeMCP`. The server requires a random bearer token. |
| `mcp` | Writes `--mcp-config` files (`HTTPServer`/`StdioServer`/`WriteConfig`). |
| `transport` | How the binary is launched: `Local` (default), `DockerExec`, `DockerRun`, `SSH`. Builds the `*exec.Cmd`; runner/client own piping + teardown. `Terminator` (`teardown.go`) stops the remote claude via `CommandOpts.RunID`/`CAG_RUN_ID`; `Preflight` (`preflight.go`) checks binary, version, auth. |
| `workspace` | Project/session dirs, `CLAUDE.md` placement, git worktrees, `RunGit`. |
| `signal` | Marker-agnostic outcome detection. |
| `graph` | Typed execution graphs (`Graph[S]`): nodes, conditional edges/loops, parallel fan-out, `Group` stages, `Interrupt`/`Resume`, `InterruptReview`, `EmitArtifact`, `Agent` nodes over runner, per-node `Report`, `Mermaid()`, `Topology()`. Observers in `events.go` (optional interfaces by type assertion). **Durable runs:** `checkpoint.go` (`CheckpointStore`, `FileStore`, `MemStore`, `Record`), `durable.go` (`WithCheckpointStore`, `Recover`, `Fork`, `IdempotencyKey`, `AllowTopologyChange`), `group.go`, `artifact.go`, `review.go`. |
| `graphweb` | Live and replay web UI for graph runs: a `graph.Observer`+`Gate` server (`New`, `Start`, `RunInteractive`), journals (`WithJournal`, `NewReplay`), `NewReplayStore` for checkpoint stores. Embedded `index.html`. |
| `eval` | Agent evals: trajectory matching, ROUGE/judge (+`JudgeSamples`, rubrics), budgets, pass rate / `PassAtK`, user simulation, JSON eval sets, `RunT`. Metrics come from stream lines or recordings. |
| `recording` | JSONL run recordings (`runner.WithRecorder`) + `ReplayTransport` for offline replay. One file is replay fixture, eval golden and `cag trace` input. |
| `clitest` | Fake `claude` binaries for tests (`FakeCLI`, `FromRecording`, step builders, argv/stdin capture). |
| `fleet` / `budget` | Worker pool over worktrees; cross-run spend tracking. |
| `cmd/cag` | The CLI: `run`, `fleet`, `trace`, `web --replay`, `runs`, `version`. |
| `otel/` (separate module) | OpenTelemetry adapters for `runner.Observer` and `graph.Observer`. |
| `temporal/` (separate module, Temporal SDK v1.50) | Graphs as Temporal workflows, one activity per node (`Register`, `Start`, `Wait`, `Answer`, `PendingQuestion`); built on `graph.Runnable.Step`. Integration tests need `temporal server start-dev`. |
| `examples/` | Runnable demos, incl. `pipeline`, `pizza` (durable `-store`/`-recover`), `artifacts`, `pgstore` (Postgres `CheckpointStore`). |
| `internal/trace` | HTML rendering for `cag trace`. |
| `internal/procgroup` | Cross-platform process-group setup/kill. |
| `internal/readmetest` | Parses every ```` ```go ```` fence of README.md; keep it green when editing the README. |

## Commands

```sh
go build ./...
go test ./...                      # unit tests — no claude needed (fake binaries)
go test ./... -cover               # per-package coverage
go vet ./... && gofmt -l .         # lint/format gate (gofmt must print nothing)

# Real integration tests — require `claude` on PATH + credentials. Cost money.
go test -tags integration ./...    # uses the cheap haiku model

scripts/fuzz.sh 10s                # every native Fuzz* target (CI runs it non-blocking)
go test ./internal/readmetest/     # README go snippets must parse

go run ./cmd/cag trace runs/ --open          # HTML trace of recordings
go run ./cmd/cag web --replay run.jsonl      # graph debugger on a journal or checkpoint dir
go run ./cmd/cag runs ./checkpoints [runID]  # durable graph runs / one run's log
```

## Quality bar

- **Code coverage must stay > 90%** (statements, across the substantive
  packages; `internal/procgroup` syscall glue and `examples` main funcs are
  exempt). When a change drops it below 90%, add tests in the same change.
- Every behavioral claim about the CLI must have an **integration test that
  asserts the behavior** (the effect), not just that data was transmitted.

## Conventions

- **No external runtime deps** beyond the MCP go-sdk (used only by `tools`).
  testify is test-only.
- Unit tests must not require `claude`: they drive **fake binaries** (shell
  scripts emitting canned JSON / speaking stream-json). Anything needing the
  real binary goes behind `//go:build integration` and `t.Skip`s if `claude` is
  absent.
- Keep infrastructure out of the SDK. New infra hooks = new seams (callbacks,
  interfaces, config), never a hard dependency.
- Cost is read via `StreamEvent.Cost()` — current CLI emits `total_cost_usd`,
  older builds `cost_usd`. Never read the raw field directly.
- Functional options on `runner`; plain `Config` structs on `client`/`tools`.
- Graph observers extend `graph.Observer` through optional interfaces (type assertion), never by
  changing the base interface. Durable state `S` must round-trip through JSON.
- Fuzz every parser and every security-relevant matcher (`permissions`, `claudecli`, redaction).

## CLI-version caveats

- `can_use_tool` needs the CLI to emit `control_request{subtype:"can_use_tool"}`
  under `--permission-prompt-tool stdio`. Broken in CLI 2.1.6, fixed in 2.1.196
  (the version this SDK targets). On older CLIs the callback silently won't fire.
- Stream-json per-message `usage` is a mid-stream snapshot (output tokens are
  mostly absent); the `result` event's `usage` holds the run totals. Prefer it.
- `AskUserQuestion` reaches the host as `can_use_tool` (with
  `requires_user_interaction: true`); answer with `updatedInput` = input +
  `answers` keyed by question text (verified on 2.1.318).
- The `skills` allowlist filters skills out of the model's **context**, not the
  installed-skills list reported in `system/init`.
