# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- Cancel/timeout no longer hangs on a CLI that traps SIGTERM: the process group gets SIGTERM, then SIGKILL after 2s (`runner`, and `client.Close`). `RunStream` now returns the partial `Result` (text so far, session id) together with the cancel/`TimeoutError`; `TimeoutError` also matches `errors.Is(err, context.DeadlineExceeded)`, and `Run`/`RunJSON` report cancellation as `context.Canceled` (ADK #2425).
- `runner.DefaultRetryable` retries a failed process whose stdout ends in a transient error result (429/529/overloaded). The CLI exits 1 after such a result and leaves stderr empty, so it was not retried (ADK #1214).
- `client`: a panicking `OnAskUser`/`CanUseTool` callback now gets a deny reply (hook callbacks an error reply) instead of crashing the host (ADK #3645).
- `claudecli.ExtractTextFromStream` and `eval` ignore sub-agent assistant text when building the answer (ADK #3984).

### Added
- `runner.Result.Subtype` and `runner.IsMaxTurns`: detect a run cut off by `--max-turns` (`error_max_turns`), also when the CLI reports `is_error=false` (ADK #5054).
- `claudecli.StreamEvent.ParentToolUseID` / `IsSubagent`, and `eval.ToolCall.ParentToolUseID` (ADK #3984).
- `eval.JudgeSamples(n)`: grade each response n times and take the median score, damping a flaky LLM judge (ADK #3593).

### Added
- `graph`: typed execution graphs over runs and Go steps.
  - Static and conditional edges (loops), and parallel fan-out with merge.
  - `Compile`-time validation and a `MaxSteps` guard.
  - `Interrupt` → JSON checkpoint → `Resume(answer)` for human-in-the-loop pauses.
  - `Agent` nodes over `runner`, with optional retry/resume.
  - A per-node cost/duration `Report`, `Observer` hooks and `Mermaid()` export.
- `eval`: agent evaluation.
  - Tool-trajectory matching (`Exact`/`InOrder`/`AnyOrder`, `IgnoreArgs`, `ArgsSubset`, `MCPNames`).
  - ROUGE-1/ROUGE-L, `Contains`/regex checks, and a `ClaudeJudge` LLM judge.
  - Cost/tool-call/duration budgets and repeats with parallelism.
  - JSON eval sets, `CaseFromRecording` golden cases and `RunT` for `go test`.
  - Metrics: tool calls, inferences, tokens, cost, model vs tool time. `Combine`/`FromRecordingDir` evaluate a multi-node run.
- `recording`: a JSONL format for stream runs (header + raw lines stamped with `_cag_t`).
  - `Dir` writes one file per run.
  - `ReplayTransport` replays a recording through a runner, offline and deterministic.
- `runner.WithRecorder`, `Input.Label` (also in `RunRecord.Label`).
- `runner.RunStreamWithRetry` and `RetryPolicy.Resume` / `ResumePrompt`: after a failure that carries a session ID, the retry continues the session via `--resume` instead of rerunning.
- `runner.TruncatedStreamError`, `IsTruncatedStream` and `SessionIDOf`. `ProcessError.SessionID` and `TimeoutError.SessionID`.
- `client.Config.OnAskUser` / `Question` / `AnswerFromStdin`: answer the CLI's `AskUserQuestion` tool from Go. Verified against CLI 2.1.318.
- `cag trace <recording|dir>`: a self-contained HTML view of recorded runs.
- `examples/pipeline`: a plan → code → review/fix → docs graph of real agents, with an eval integration test.
- `examples/pizza`: an order-taking agent over mocked pizzeria/delivery/payment Go tools. A human confirms the priced draft before a plain Go step places it. It shows a live event view, can `-replay` recorded runs, loads `ANTHROPIC_*` from a gitignored `.env`, and has a 4-scenario eval integration test.

### Changed
- Minimum Go version is now 1.27 (was 1.25); CI and release build on 1.27.
- **Behavior change:** `RunStream` used to return success with an empty result when the stream ended without a `result` event. It now returns the partial `Result` *and* a `*TruncatedStreamError`. `DefaultRetryable` treats it as retryable.

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

[Unreleased]: https://github.com/tggo/claude-agent-go/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/tggo/claude-agent-go/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/tggo/claude-agent-go/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tggo/claude-agent-go/releases/tag/v0.1.0
