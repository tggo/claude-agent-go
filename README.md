# claude-agent-go

A Go SDK for building agents on top of the **Claude Code CLI** — the Go
counterpart to the official Python `claude-agent-sdk`.

It drives the `claude` binary as a subprocess: builds the CLI argv, streams the
prompt over stdin, and parses the output back into typed Go structs. It does
**not** reimplement the agent loop — that lives in the CLI. What it adds is a
clean, typed, dependency-light Go surface over everything the CLI can do:
one-shot runs, interactive sessions, in-process Go tools, permission and hook
callbacks, inline subagents, and a skills allowlist.

```go
r := runner.New(runner.WithModel("sonnet"))
res, _ := r.RunJSON(ctx, runner.Input{Prompt: "Summarize this repo."})
fmt.Println(res.ResultText, res.TotalCostUSD, res.SessionID)
```

- **Zero external runtime dependencies** beyond the official MCP Go SDK (used
  only by the `tools` package). testify is test-only.
- **Behaviorally verified** against the real `claude` binary — 17 integration
  tests assert effects, not just that flags were passed.
- **>90% unit coverage** across every substantive package.

## Install

```sh
go get github.com/tggo/claude-agent-go@latest
```

Requires the [`claude`](https://docs.claude.com/en/docs/claude-code) CLI on
`PATH` and valid credentials.

## Packages

| Package | What it does |
|---|---|
| `runner` | One-shot runs: `Run` (plain), `RunJSON` (metadata), `RunStream` (live + `ProgressFunc`). |
| `client` | Interactive multi-turn session + the control protocol (permissions, hooks, agents, skills). |
| `tools` | Expose Go functions to the agent as MCP tools, in-process — untyped or typed with schema inference. |
| `claudecli` | Output types and parsers (sessions, cost, tokens, content blocks, partial deltas). |
| `mcp` | Write `--mcp-config` files for external MCP servers. |
| `workspace` | Project/session dirs, `CLAUDE.md`, git worktrees. |
| `signal` | Marker-agnostic outcome detection. |

## What you can build

### In-process Go tools

The agent calls your Go functions during a run — no separate server to deploy.

```go
srv, _ := tools.Serve("weather", tools.Tool{
    Name:        "get_weather",
    Description: "Returns current weather for a city.",
    Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
        var in struct{ City string `json:"city"` }
        json.Unmarshal(args, &in)
        return "Sunny in " + in.City, nil
    },
})
defer srv.Close()

name, cfg := srv.Config()
mcpPath := filepath.Join(dir, "mcp.json")
mcp.WriteConfig(mcpPath, map[string]mcp.ServerConfig{name: cfg})

r := runner.New(runner.WithAllowedTools("mcp__" + name + "__get_weather"))
res, _ := r.RunJSON(ctx, runner.Input{Prompt: "Weather in Kyiv?", MCPConfigPath: mcpPath})
```

Typed variant with compile-time-checked I/O and inferred JSON Schema:

```go
reg := tools.NewRegistry("calc")
tools.Register(reg, "add", "Add two integers",
    func(ctx context.Context, in struct{ A, B int }) (struct{ Sum int }, error) {
        return struct{ Sum int }{in.A + in.B}, nil
    })
srv, _ := reg.Serve()
```

### Interactive sessions

```go
c, _ := client.New(ctx, client.Config{Model: "sonnet"})
defer c.Close()

c.Query(ctx, "Remember the number 7.", nil)
t2, _ := c.Query(ctx, "What number did I say?", nil) // t2.Text recalls "7"
```

### Permission callbacks

Decide allow/deny for every tool call, in Go:

```go
c, _ := client.New(ctx, client.Config{
    Model: "sonnet",
    CanUseTool: func(ctx context.Context, tool string, input json.RawMessage, _ client.PermissionContext) (client.PermissionResult, error) {
        if tool == "Bash" {
            return client.Deny("no shell in this session"), nil
        }
        return client.Allow(), nil
    },
})
```

### Hook callbacks

Run Go logic before/after tool use — and block it:

```go
c, _ := client.New(ctx, client.Config{
    Hooks: map[string][]client.HookMatcher{
        "PreToolUse": {{
            Matcher: "Bash",
            Callbacks: []client.HookCallback{
                func(ctx context.Context, input json.RawMessage, _ string) (json.RawMessage, error) {
                    return json.RawMessage(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`), nil
                },
            },
        }},
    },
})
```

### Inline subagents

Define subagents from Go (no `.claude/agents` files); the main agent delegates
to them via the Task tool:

```go
c, _ := client.New(ctx, client.Config{
    Agents: map[string]client.AgentDefinition{
        "security-reviewer": {
            Description: "Reviews diffs for vulnerabilities.",
            Prompt:      "You are a security auditor. Be terse.",
            Tools:       []string{"Read", "Grep"},
            Model:       "opus",
        },
    },
})
```

## Examples

See [`examples/`](./examples):

| Example | Shows |
|---|---|
| `basic` | one-shot `RunJSON` with cost/session metadata |
| `stream` | `RunStream` with a progress callback |
| `tools` | the agent calling an in-process Go tool |
| `interactive` | a multi-turn `client` session with memory |
| `permissions` | a `can_use_tool` allow/deny callback |
| `hooks` | a `PreToolUse` hook that blocks a command |
| `agents` | an inline subagent delegated to via Task |
| `worktree` | run an agent in an isolated git worktree; changes land on a throwaway branch |

Run one:

```sh
go run ./examples/basic -prompt "Say hi"
```

### Parallel agents with git worktrees

The `workspace` package gives each run an isolated checkout, so many agents can
work the same repo at once without colliding:

```go
ws := workspace.New(baseDir, nil)
wt, branch, _ := ws.CreateWorktree(ctx, repoDir, runID) // git worktree add -b temp/wt-<id>
defer ws.RemoveWorktree(ctx, repoDir, wt)

r.Run(ctx, runner.Input{WorkDir: wt, Prompt: "...implement the change and commit it..."})
// the agent's edits and commits land on `branch`; the main checkout stays clean
```

Spawn one worktree per goroutine to fan out work; `RemoveWorktree` tears each
down. See [`examples/worktree`](./examples/worktree).

## Feature coverage vs the Python SDK

| Capability | Status |
|---|---|
| `query` / JSON / stream + cost/session/token metadata | ✅ |
| Interactive multi-turn client | ✅ |
| In-process tools (untyped + typed generics) | ✅ |
| `can_use_tool` permission callbacks | ✅ |
| Hook callbacks (PreToolUse decisions, blocking) | ✅ |
| Inline subagents + skills allowlist | ✅ |
| Partial / token streaming + interrupt with ack | ✅ |
| resume / continue / permission-mode / add-dir / settings | ✅ |
| External MCP servers | ✅ |

Every ✅ has an integration test that asserts the behavior against the real
binary. **Go-native extras:** typed tools with schema inference, a single static
binary with no runtime, and `context.Context` cancellation throughout.

## Testing

```sh
go test ./...                    # unit (fake CLI, no credentials)
go test -tags integration ./...  # real claude (needs PATH + credentials; costs money)
```

## License

MIT
