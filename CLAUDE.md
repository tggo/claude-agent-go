# CLAUDE.md — claude-agent-go

A Go SDK that drives the **Claude Code CLI** (`claude`) as a subprocess: a thin,
typed client around the binary, plus an interactive session client, in-process
Go tools, permission/hook callbacks, and inline subagents.

Module: `github.com/tggo/claude-agent-go` · Go 1.25.

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
| `claudecli` | Output types + parsers: `ParseOutput`, `ParseStreamLine`, `ExtractTextFromStream`, typed content blocks (`ToolUses`, `ThinkingText`), partial-stream deltas, escaping diagnostics. No deps. |
| `runner` | One-shot execution: `Run` (`--print`), `RunJSON`, `RunStream` (+ `ProgressFunc`). Process-group teardown on cancel/timeout. |
| `client` | Interactive multi-turn session over bidirectional stream-json: `New`/`Query`/`Interrupt`/`Close`, plus the control protocol — `can_use_tool` permission callbacks, hook callbacks, inline agents, skills allowlist. |
| `tools` | Expose Go functions to the agent as MCP tools, in-process: `Serve` (untyped) and `Register[In,Out]` (typed, schema-inferred). |
| `mcp` | Writes `--mcp-config` files (`HTTPServer`/`StdioServer`/`WriteConfig`). |
| `transport` | How the binary is launched: `Local` (default), `DockerExec`, `SSH`. Builds the `*exec.Cmd`; runner/client own piping + teardown. |
| `workspace` | Project/session dirs, `CLAUDE.md` placement, git worktrees, `RunGit`. |
| `signal` | Marker-agnostic outcome detection. |
| `internal/procgroup` | Cross-platform process-group setup/kill. |

## Commands

```sh
go build ./...
go test ./...                      # unit tests — no claude needed (fake binaries)
go test ./... -cover               # per-package coverage
go vet ./... && gofmt -l .         # lint/format gate (gofmt must print nothing)

# Real integration tests — require `claude` on PATH + credentials. Cost money.
go test -tags integration ./...    # uses the cheap haiku model
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

## CLI-version caveats

- `can_use_tool` needs the CLI to emit `control_request{subtype:"can_use_tool"}`
  under `--permission-prompt-tool stdio`. Broken in CLI 2.1.6, fixed in 2.1.196
  (the version this SDK targets). On older CLIs the callback silently won't fire.
- The `skills` allowlist filters skills out of the model's **context**, not the
  installed-skills list reported in `system/init`.
