# go-ai

Durable AI agents in Go, running on [Diagrid Catalyst](https://www.diagrid.io/catalyst).

Build an agent as a small graph of nodes with a Go AI framework (LangChainGo,
Eino, …) and run it as a Dapr Workflow: every node becomes a checkpointed
activity, so if the process dies it resumes from the last completed node instead
of starting over.

A `Runner` takes the graph, registers it as an agent, and runs it on Catalyst —
no backend or registry to wire up yourself. A framework plugs in through its node
constructors (`langchaingo.ModelNode`, `eino.ChatModelNode`), so swapping
LangChainGo for Eino doesn't touch your orchestration code.

## Packages

| Package | What it does |
|---|---|
| `agent` | The graph model — `Graph`, `NodeFunc`, conditional edges — plus an in-process interpreter (`Execute`) for testing wiring without a sidecar. Standard library only. |
| `durable` | `DaprBackend`: runs a graph as a Dapr Workflow on Catalyst. One generic workflow, one execute-node activity. This is the only backend — there's no non-durable path. |
| `registry` | The agent directory — the `agents:<team>:<name>` key scheme that lets agents find each other in a shared store. |
| `goai` (root) | The `Runner`. Give it a graph and a name; it connects to Catalyst, registers the agent, and runs it. |
| `adapters/langchaingo`, `adapters/eino` | Node constructors that turn a framework's models/chains into graph nodes. |

Adapter and example packages are separate modules so each framework's
dependencies stay out of the core.

## Quickstart

Catalyst runs the Dapr sidecar for you; you run your Go program against it. Use
an existing project (or make one) and enable agent infrastructure on it — that
provisions the managed workflow engine and the `agent-registry` component the
Agents view reads.

```bash
diagrid login
diagrid project update <your-project> --enable-agent-infrastructure

# Create the Agent — it appears under Agents in the console and scopes its
# identity into the managed agent-registry, which the runtime must have before it
# is allowed to register. Use the name your app runs as (the appID in
# catalyst.yaml); it must be unused (don't `appid create` it first, or they collide).
diagrid agent create control-room --project <your-project> --wait

cd examples/langchaingo && go mod tidy
diagrid dev run -f catalyst.yaml --project <your-project>
```

The bundled `catalyst.yaml` runs `go run .`. The run file is optional —
`diagrid dev run --project <p> --app-id control-room -e GOWORK=off -- go run .`
does the same thing inline. On startup the runner registers into `agent-registry`
(it logs the store it chose), and the agent shows up under **Agents** in the
console. Kill the process mid-run and start it again with the same instance ID —
Catalyst picks the workflow back up where it left off.

Each example has a README with the full story (local Dapr, Catalyst, crash
recovery): [`examples/langchaingo`](examples/langchaingo/README.md),
[`examples/eino`](examples/eino/README.md).

Prefer a local sidecar?

```bash
cd examples/langchaingo
dapr run --app-id control-room --resources-path ./resources -- go run .
```

The examples call OpenAI, so export `OPENAI_API_KEY` before running (the app
inherits your shell env). Set `OPENAI_MODEL` to override the default `gpt-4o`.

## The API

Build a graph, hand it to a `Runner`:

```go
graph := agent.NewGraph("control-room").
    AddNode("diagnose", langchaingo.ModelNode(model, langchaingo.WithOutputKey("diagnosis"))).
    AddNode("reboot", langchaingo.ModelNode(model, langchaingo.WithInputKey("diagnosis"))).
    SetEntry("diagnose").
    AddEdge("diagnose", "reboot").
    AddEdge("reboot", agent.END)

runner, err := goai.NewRunner(ctx, goai.Config{
    Graph:     graph,
    Name:      "control-room",
    Framework: langchaingo.Framework,
    MaxSteps:  50,
})
defer runner.Close()

out, err := runner.Invoke(ctx, agent.State{"input": "..."},
    goai.InvokeOptions{InstanceID: "control-room-001"})
```

`NewRunner` connects to Catalyst, registers the agent, and runs the graph —
there's no backend or registry to construct. Reuse an `InstanceID` to resume an
interrupted run.

## Adding a framework

A framework needs one thing: node constructors that turn its models/chains into
`agent.NodeFunc` (see `adapters/eino/eino.go` — about 40 lines). Everything else
— the runner, durability, the registry — already works against `agent.Graph`.

## Requirements

Go 1.26.4+ (the Dapr Go SDK needs it) and the Dapr CLI for local runs. Tests are
offline — `make test` runs the engine and registry with no sidecar.

## Layout

```
go-ai/
├── runner.go        # goai.Runner
├── agent/           # graph model + in-process interpreter
├── registry/        # agent directory → Catalyst agent-registry component
├── durable/         # DaprBackend
├── adapters/        # langchaingo, eino (own modules)
└── examples/        # runnable samples by framework (own modules)
```
