# Examples

Runnable samples, one folder per framework. Each is its own module and runs as a
durable Dapr Workflow, on Catalyst or a local Dapr sidecar.

| Framework | Directory | Agent |
|---|---|---|
| LangChainGo | [`langchaingo/`](langchaingo/README.md) | Control Room Operator (`diagnose → reboot`) |
| Eino | [`eino/`](eino/README.md) | Perimeter Watch (`scan → alert`) |
| LangChainGo + MCP | [`mcp/`](mcp/README.md) | MCP Assistant: discovers and calls the sidecar's MCP tools |

## Running on Catalyst — declarative vs inline

`diagrid dev run` takes either a run file or inline app args; both do the same
thing. The examples ship a `catalyst.yaml`, so either works:

```bash
# Declarative — reads catalyst.yaml (appID, `go run .`, GOWORK=off)
diagrid dev run -f catalyst.yaml --project <project>

# Inline — no run file; pass the same settings on the command line
diagrid dev run --project <project> --app-id <id> -e GOWORK=off -- go run .
```

Note the `--` separating diagrid's flags from your app command (it's `-- go run .`,
not `diagrid dev run main.go`). The run file just saves retyping the appID, command,
and env; `catalyst.yaml` is committed convenience, not a requirement.

## Running locally

`dapr init` once, then a local sidecar instead of Catalyst:

```bash
dapr run --app-id <id> --resources-path ./resources -- go run .
```

The Go code is the same in all three modes — it connects to whichever sidecar is
configured.

## Prerequisites

- Go 1.26.4+.
- `go mod tidy` in the example directory once (needs the module proxy).
- The [`diagrid` CLI](https://docs.diagrid.io/catalyst/references/cli-reference/)
  for Catalyst, or the [Dapr CLI](https://docs.dapr.io/getting-started/) for
  local runs.
- For Catalyst: a project with agent infrastructure enabled
  (`diagrid project update <project> --enable-agent-infrastructure`) and an Agent
  resource per agent (`diagrid agent create <name> --project <project>`) — an App
  ID must be backed by an Agent resource before its runtime may register into the
  Agents view. Each example's README has the exact commands.
- `OPENAI_API_KEY` (the examples call OpenAI). Optionally `OPENAI_MODEL` (default
  `gpt-4o`). The app inherits your shell env under `diagrid dev run`.

Every example goes through `durable.DaprBackend`, so every run is durable — each
node is a checkpointed workflow activity that resumes after a crash. There's no
non-durable path.
