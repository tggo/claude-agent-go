# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-11-06

### Added
- `graph`: typed execution graphs over runs and Go steps.
  - Static and conditional edges (loops), and parallel fan-out with merge (`WithClone` is required for reference-typed state).
  - `Compile`-time validation (`ErrInvalidGraph`) and a `MaxSteps` guard.
  - `Interrupt` → JSON checkpoint → `Resume(answer)`, with `AsInterrupt[S]` / `ErrInterrupt`. `Resume` returns a cumulative `Report` (via `Checkpoint.Prior`).
  - `Agent` nodes over `runner`, plus `RunAgent` for hand-written nodes (retry, progress and cost accounting).
  - A per-node cost/duration `Report`, `Observer` hooks and `Mermaid()` export.
- `eval`: agent evaluation.
  - Tool-trajectory matching (`Exact`/`InOrder`/`AnyOrder`, `IgnoreArgs`, `ArgsSubset`, `MCPNames`).
  - ROUGE-1/ROUGE-L, `Contains`/regex checks, a `ClaudeJudge` LLM judge, and `JudgeSamples(n)` for a median over n runs (ADK #3593).
  - Rubrics: `Case.Rubrics` / `Criteria.Rubrics`, each graded yes/no separately, with a per-rubric breakdown.
  - Repeats with pass-rate semantics: `PassRate(f)` / `Criteria.MinPassRate`, `PassAtK`, and `4/5` in the report.
  - Multi-turn user simulation: `UserSim`, `ScriptedUser`, `LLMUser`, `Session`, `ClientSession`, `Conversation`.
  - Cost/tool-call/duration budgets, JSON eval sets, `CaseFromRecording` golden cases and `RunT` for `go test`.
  - Metrics: tool calls, inferences, tokens, cost, model vs tool time. `Combine`/`FromRecordingDir` evaluate a multi-node run.
- `recording`: a JSONL format for stream runs (a header, then raw lines stamped with `_cag_t`). `Dir` writes one 0600 file per run, `ReplayTransport` and `ReplayLines` replay it.
- `runner`:
  - `WithRecorder` and `Input.Label`.
  - `RunStreamWithRetry` with `RetryPolicy.Resume` / `ResumePrompt` (continue a session via `--resume`) and `UnknownAttemptCostUSD`.
  - `Plugin` with optional `BeforeRun`, `EventPlugin` and `AfterRun` hooks, `WithPlugins`, `PluginAbortError`, and a built-in `SpendCap`.
  - `Result.Subtype` and `IsMaxTurns` (ADK #5054).
  - `TruncatedStreamError`, `LineTooLongError` and `SessionIDOf`.
- `client.Config.OnAskUser`: answer the CLI's `AskUserQuestion` tool from Go. Verified against CLI 2.1.318.
- `permissions`: ordered `Allow`/`Deny`/`Ask` rules with Claude Code-style patterns (`Bash(git status*)`), compound-safe Bash matching, `Remember` and an audit callback. `Policy.CanUseTool()` plugs into `client`.
- `otel`, a separate module: OpenTelemetry adapters for `runner.Observer` and `graph.Observer`. The core module gains no dependency.
- `clitest`: fake `claude` binaries for your own tests (`FakeCLI`, `FromRecording`, stream builders, argv/stdin capture).
- `claudecli`:
  - `CheckInvariants` checks tool_use/tool_result pairing, a single final result and a consistent session id, and is fuzzed.
  - `StreamEvent.ParentToolUseID` / `IsSubagent` (ADK #3984).
- `tools`: `(*Registry).ServeMCP(dir)` serves and writes the MCP config in one call.
- `cag trace <recording|dir>`: a self-contained HTML view of recorded runs.
- `graph`: durable runs.
  - `WithCheckpointStore` appends every run to a `CheckpointStore` (`FileStore` on disk, `MemStore` in memory; bring your own, see `examples/pgstore`). State must survive a JSON round trip.
  - `Recover(ctx, runID)` continues a crashed, failed or paused run from its last commit; a node that was in flight is re-run, with `IdempotencyKey(ctx)` for side effects. A recovered agent resumes its CLI session (`RecoveredSession`, `WithRecoverPrompt`).
  - `Fork` copies a run's history up to a commit, optionally edits the state, and continues under a new id (rollback and what-if).
  - A changed graph is refused with `ErrTopologyChanged` unless compiled with `AllowTopologyChange`. `Report.RunID`, `ErrRunNotFound`, `ErrCheckpoint`.
  - `examples/pgstore` (Postgres store over `database/sql`); `examples/pizza -store/-recover`.
- `graph`: `EmitArtifact` / `ArtifactObserver` for files a node produces, and `InterruptReview` / `ReviewAnswer` for human review of a list of items.
- `graph`: stage groups (`Graph.Group`, `GroupWith`, `Topology.Groups`, Mermaid subgraphs) and `Topology()` for visualisers, plus `AddAgentNode`, `MarkAgent`, `AgentRunner` and `LabelEdge`.
- `graph`: optional observer interfaces, detected by type assertion: `RunObserver`, `EdgeObserver`, `StateObserver`, `ResumeObserver`, `Gate`, `AgentObserver`, `AgentInputObserver`. `StepInfo` gains `RunID` and `Attempt`.
- `graphweb`: a live and replay web UI for graph runs (topology, per-node state diffs, agent event stream, breakpoints, answering interrupts and reviews in the browser, artifacts gallery, Config tab). Snake/LR/TB layouts, automatic stages, collapsible groups, follow camera, minimap and keyboard navigation. `NewReplay` / `NewReplayStore` replay a journal or a checkpoint store. `-web` flag in `examples/pipeline` and `examples/pizza`.
- `runner.Effective(Input)`: the secret-free effective configuration of a run, shown in the graphweb Config tab.
- `cag web --replay <journal|dir>` serves a recorded run in the web UI; `cag runs <store> [runID]` lists durable runs (running/crashed/paused/done) or prints one run's log.
- `transport.Terminator` and `CommandOpts.RunID` (`CAG_RUN_ID` in the remote env): `SSH`, `DockerExec` and `DockerRun` stop the remote `claude` on cancel, timeout, abort and `client.Close`, instead of only killing the local proxy. `DockerRun` names its container `cag-<runid>`.
- `transport.Preflight`, `runner.Preflight` and `runner.WithPreflight` check the target before a run is paid for, failing with `ErrClaudeNotFound`, `ErrCLITooOld` or `ErrNotAuthenticated`.
- Fuzzing: 22 native fuzz targets and `scripts/fuzz.sh`, run by a non-blocking CI job.
- `graph`: `Runnable.Step` / `StepOptions` / `StepResult` / `StepHooks` execute one node and its route, so external engines can own the control loop.
- New module `github.com/tggo/claude-agent-go/temporal`: graphs as Temporal workflows, one activity per node (`Register`, `Start`, `Wait`, `Answer`, `PendingQuestion`). Interrupts are the `answer` signal and the `pending` query; a retried agent node resumes its CLI session via heartbeat details; `IdempotencyKey` is `workflowID:node:step`. The core gains no Temporal dependency. Limits: no continue-as-new yet, failed-attempt cost is not in the report.
- Examples:
  - `examples/pipeline`: plan → code → review/fix → docs.
  - `examples/pizza`: an order-taking agent over mocked services, with a human-confirmed draft bound by hash, a live view, `-replay`, and a 4-scenario eval.
  - Both have golden replay tests built from real recorded runs.

### Changed
- Minimum Go version is now 1.27 (was 1.25). CI and release build on 1.27.
- **Behavior change:** `RunStream` returns the partial `Result` *and* a `*TruncatedStreamError` when the stream ends without a `result` event. It used to return an empty success.
- **Behavior change (security):** the in-process MCP server from `tools` now requires a random 256-bit bearer token (401 otherwise). `Config()` carries it as a header, so callers using `Config()` are unaffected.
- **Behavior change (security):** with `OnAskUser` set and no `CanUseTool`, other tools are now denied unless `PermissionMode` is `bypassPermissions` or `SkipPermissions` is set. They used to be allowed when the mode was unset.
- `RetryPolicy.MaxSpendUSD` takes effect with `Resume`: a truncated attempt, whose cost the CLI never reports, is charged an estimate for the guard.
- `cag trace` writes HTML with mode 0600.

### Fixed
- Cancel/timeout no longer hangs on a CLI that traps SIGTERM: the process group gets SIGTERM, then SIGKILL after 2s. `RunStream` returns the partial result together with the error, and `TimeoutError` matches `context.DeadlineExceeded` (ADK #2425).
- A stream line over `MaxBufferSize` kills the CLI at once and returns the partial result with `LineTooLongError`. Before, the SDK drained stdout while the agent kept spending.
- `DefaultRetryable` retries a transient (429/529/overloaded) error result that the CLI reports only on stdout (ADK #1214).
- `Result.Attempts` is the real attempt count when retries stop early.
- `client`: a panicking `OnAskUser`/`CanUseTool`/hook callback is answered instead of crashing the host (ADK #3645).
- Sub-agent text no longer leaks into the final answer in `ExtractTextFromStream` and `eval` (ADK #3984).
- Huge `Retry-After` and `_cag_t` values no longer overflow; `ProcessError` output is truncated on rune boundaries.

### Security
Found by fuzzing:
- `permissions`: Allow/Ask rules for file tools never match a path containing `..`, and Deny rules match the cleaned path.
- `permissions`: Bash Deny rules see commands inside `$()`, backticks, subshells and groups.
- `permissions`: `Remember` keys can no longer collide.
- `RedactTokens` masks every occurrence of a token, not only the first.
- Known limit: a Bash Deny rule does not see through wrappers such as `sudo`, `env` or `xargs`. Deny the wrapper too, or do not rely on Deny alone.

Found in the release review:
- `transport.SSH` drops env keys that are not plain shell identifiers (the key was spliced into the remote command unquoted).
- `graph.FileStore`: a final record missing only its newline is kept and terminated instead of being cut on the next append.
- Known limit: `cag web --replay` serves artifact paths straight from the journal; replay only journals you trust, and keep the default loopback bind.

## [0.2.1] - 2026-07-16

### Added
- `client.Config.StderrFunc` — capture the CLI's stderr line-by-line from an
  interactive session, instead of it going only to the parent's `os.Stderr`
  where a caller with a structured logger never sees it (#2). Unset keeps the
  `os.Stderr` passthrough, so existing callers are unaffected.

### Fixed
- A `client` session that exits non-zero now reports the tail of the CLI's
  stderr in its `Close`/`Wait` error, rather than a bare "exit status 1" (#2) —
  the interactive counterpart to the `runner` fix in 0.2.0.
- `cag version` reported "dev" for `go install`-built binaries, which stamp no
  ldflags — it now falls back to the module version recorded in build info.

### Removed
- The Homebrew tap. Publishing the formula needed a PAT that was never
  configured, so goreleaser 401'd and failed every release *after* the binaries
  had published. `go install` and the release binaries cover installation.

## [0.2.0] - 2026-07-16

### Added
- `runner.ProcessError.Stdout` / `.ProcessState` / `.MaxRSSBytes()` — a CLI that
  exits non-zero without writing to stderr now still yields a diagnosable error:
  the tail of stdout, the exit reason (a signal, where the OS reports one), and
  peak memory, so an OOM kill is distinguishable from a crash (#1).
- `runner.WithObserver` / `RunRecord` — a dependency-free tracing/metrics seam
  emitting cost/tokens/turns/transport/duration per run (bridge to OpenTelemetry
  or Prometheus in ~10 lines; core stays dep-free).
- Remote reachability for in-process tools: `tools.Server.Port` / `ConfigForHost`
  plus `tools.DockerHostGateway()` and `tools.SSHReverseTunnel()` so a container
  or ssh agent can reach the host tools server (docker path verified live).
- `cag` CLI (`cmd/cag`) — run agents without writing Go: `cag run` (with
  --transport local/ssh/docker/docker-run) and `cag fleet <config.yaml>` (a worker
  pool from YAML, with dependencies, worktrees, retry, and a spend cap).
- goreleaser config + release workflow — cross-platform `cag` binaries on tagged
  releases.
- `budget.Tracker` — thread-safe cross-run/fleet spend tracker with a hard cap,
  warn/exceed callbacks, per-session totals, and a CanSpend gate.
- Fuzz targets for the `claudecli` parsers (ParseOutput, ParseStreamLine, JSON extractors).
- `transport.DockerRun` — run claude in a fresh, throwaway container (`docker run
  --rm --init`), created for the run and removed on exit; closes the remote-teardown
  gap for docker. Verified live against the docker daemon.
- `fleet` package — fan a batch of tasks (with a dependency DAG) across a pool of
  workers, each task in its own git worktree, aggregating cost with a soft spend cap.
- `runner.RunJSONWithRetry` + `RetryPolicy` — retry transient failures with
  backoff (honoring server retry-after); accumulates the cost of every attempt
  into `Result.TotalCostUSD` and stops at `MaxSpendUSD` so retries can’t multiply spend.
- Typed errors: `runner.CLINotFoundError`, `ProcessError` (exit code + stderr),
  `TimeoutError`, with `IsCLINotFound` / `IsProcessError` / `IsTimeout` helpers.
- `runner.WithMaxBufferSize` (configurable stream scan buffer, guards against a
  non-positive size) and `runner.WithStderrCallback` (live stderr line capture).

### Changed
- `runner.WithStderrCallback` now streams stderr in every run mode (`Run`,
  `RunJSON`, `RunStream`), not just `RunStream`, so a run that dies mid-flight has
  already delivered its logs instead of depending on the final buffer.

### Fixed
- `runner.Run` filed stdout under `ProcessError.Stderr`, discarding the real
  stderr entirely. Both streams now land in their own field. **Callers that
  string-match `ProcessError.Stderr` after `Run` will find that content under
  `.Stdout` now**; `RunJSON`/`RunStream` are unaffected.
- `runner.Input.ForkSession` (`--fork-session`).
- `mcp.ServerConfig.AlwaysLoad` (Claude Code 2.1.121+).
- `client.Config.CallbackTimeout` — bounds `CanUseTool`/hook callbacks so a hung
  callback can't stall the agent's turn.
- `client.NewHooks()` fluent hook builder.
- `claudecli`: structured-output capture (`ExecutionMetadata.StructuredOutput`),
  `StreamEvent.Raw`, and `StreamEvent.IsRateLimit()`.

### Notes
- Improvements informed by a scan of the leading Go Claude SDKs; see `todo.md`
  for the remaining roadmap (retry/backoff, budget tracker, fleet, Docker run,
  Kubernetes transport).

## [0.1.0] - 2026-06-30

First public release. A Go SDK for building agents on top of the Claude Code CLI.

### Added
- `runner` — one-shot execution: `Run` (plain), `RunJSON` (cost/session/token
  metadata), `RunStream` (live events + `ProgressFunc`, token-level deltas).
- `client` — interactive multi-turn sessions over the bidirectional stream-json
  protocol, with the control protocol: `can_use_tool` permission callbacks,
  hook callbacks, inline subagents, skills allowlist, and interrupt-with-ack.
- `tools` — expose Go functions as in-process MCP tools: untyped `Serve` and
  typed `Register[In,Out]` with JSON Schema inferred from a struct.
- `transport` — launch the binary locally (`Local`), in a container
  (`DockerExec`), or on a remote host (`SSH`).
- `workspace` — project/session directories, `CLAUDE.md` placement, and git
  worktrees for running agents in isolation/parallel.
- `mcp` — write `--mcp-config` files for external MCP servers.
- `claudecli` — output types and parsers (sessions, cost, tokens, typed content
  blocks, partial-stream deltas).
- `signal` — marker-agnostic outcome detection.
- `Input` flags: resume, continue, permission-mode, add-dir, settings,
  partial-messages, hook-events, and verbatim passthrough.
- Examples: basic, stream, tools, interactive, permissions, hooks, agents,
  worktree (+ parallel / client / pr), transport.

### Notes
- Targets Claude CLI `2.1.196`. The `can_use_tool` control path requires the CLI
  to emit `control_request{subtype:"can_use_tool"}` under
  `--permission-prompt-tool stdio`, which was fixed in `2.1.196`.
- Zero external runtime dependencies beyond the official MCP Go SDK (used only by
  `tools`). >90% unit coverage on every substantive package; 18 integration tests
  assert behavior against the real binary.

[Unreleased]: https://github.com/tggo/claude-agent-go/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/tggo/claude-agent-go/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/tggo/claude-agent-go/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/tggo/claude-agent-go/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tggo/claude-agent-go/releases/tag/v0.1.0
