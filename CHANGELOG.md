# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `runner.WithObserver` / `RunRecord` — a dependency-free tracing/metrics seam
  emitting cost/tokens/turns/transport/duration per run (bridge to OpenTelemetry
  or Prometheus in ~10 lines; core stays dep-free).
- Remote reachability for in-process tools: `tools.Server.Port` / `ConfigForHost`
  plus `tools.DockerHostGateway()` and `tools.SSHReverseTunnel()` so a container
  or ssh agent can reach the host tools server (docker path verified live).
- `cag` CLI (`cmd/cag`) — run agents without writing Go: `cag run` (with
  --transport local/ssh/docker/docker-run) and `cag fleet <config.yaml>` (a worker
  pool from YAML, with dependencies, worktrees, retry, and a spend cap).
- goreleaser config + release workflow — cross-platform `cag` binaries and a
  Homebrew formula on tagged releases.
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

[Unreleased]: https://github.com/tggo/claude-agent-go/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tggo/claude-agent-go/releases/tag/v0.1.0
