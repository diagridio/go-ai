# Durable execution for Go AI agents

[![Go Reference](https://pkg.go.dev/badge/github.com/diagridio/go-ai.svg)](https://pkg.go.dev/github.com/diagridio/go-ai)
[![ci](https://github.com/diagridio/go-ai/actions/workflows/ci.yaml/badge.svg)](https://github.com/diagridio/go-ai/actions/workflows/ci.yaml)

Durable AI agents in Go, running on [Diagrid](https://www.diagrid.io), built on
[Dapr](https://dapr.io).

Build an agent as a small graph of nodes with a Go AI framework (LangChainGo,
Eino, …) and run it as a Dapr Workflow: every node becomes a checkpointed
activity, so if the process dies it resumes from the last completed node instead
of starting over. Activities run at least once, so a node that was interrupted
runs again on resume: keep its side effects idempotent.

A `Runner` takes the graph, registers it as an agent, and runs it on Diagrid —
no backend or registry to wire up yourself. A framework plugs in through its node
constructors (`langchaingo.ModelNode`, `eino.ChatModelNode`), so swapping
LangChainGo for Eino doesn't touch your orchestration code.

## Install

```bash
go get github.com/diagridio/go-ai
```

The framework adapters are separate modules:
`go get github.com/diagridio/go-ai/adapters/langchaingo` or
`go get github.com/diagridio/go-ai/adapters/eino`.

## Packages

| Package | What it does |
|---|---|
| `agent` | The graph model — `Graph`, `NodeFunc`, conditional edges — plus an in-process interpreter (`Execute`) for testing wiring without a sidecar. Standard library only. |
| `durable` | `DaprBackend`: runs a graph as a Dapr Workflow on Diagrid. One generic workflow, one execute-node activity. This is the only backend — there's no non-durable path. |
| `registry` | The agent directory — the `agents:<team>:<name>` key scheme that lets agents find each other in a shared store. |
| `identity` | Verified inbound identity: `net/http` middleware that checks the caller's `X-Diagrid-User-Token` against the sidecar's JWKS, plus an identity-aware `http.Client` that propagates the caller on outbound calls. |
| `goai` (root) | The `Runner`. Give it a graph and a name; it connects to Diagrid, registers the agent, and runs it. |
| `adapters/langchaingo`, `adapters/eino` | Node constructors that turn a framework's models/chains into graph nodes. |

Adapter and example packages are separate modules so each framework's
dependencies stay out of the core.

## Quickstart

**Prerequisites:** Go 1.26.4+, the Diagrid CLI (`diagrid`), and a
[Diagrid account](https://catalyst.diagrid.io). Diagrid has a permanent free tier.

Install the Diagrid CLI on macOS or Linux with:

```bash
curl -o- https://downloads.diagrid.io/cli/install.sh | bash
```

For Windows and other options, see the
[Diagrid CLI install instructions](https://docs.diagrid.io/catalyst/references/cli-reference/).

Diagrid runs the Dapr sidecar for you; you run your Go program against it. You need a
project with the managed workflow engine and the `agent-registry` component the Agents
view reads. On a cloud project, both come with the managed key-value store — the
`default` project created for you at signup already has them, so you can skip straight
to creating the Agent.

```bash
diagrid login

# Only if you want a project of your own. Agent infrastructure is provisioned
# automatically for cloud projects with the managed KV store, so do NOT pass
# --enable-agent-infrastructure here — the CLI rejects it on a managed-KV cloud
# project. That flag is for BYOC/private-region projects and for cloud projects
# created without the managed KV store.
diagrid project create <your-project> \
  --deploy-managed-kv --deploy-managed-pubsub --enable-managed-workflow \
  --wait --use --ignore-if-exists

# Create the Agent — it appears under Agents in the console and scopes its
# identity into the managed agent-registry, which the runtime must have before it
# is allowed to register. Use the name your app runs as (the appID in
# catalyst.yaml); it must be unused (don't `appid create` it first, or they collide).
diagrid agent create control-room --project <your-project> --wait

cd examples/langchaingo && go mod tidy
diagrid dev run -f catalyst.yaml --project <your-project>
```

The bundled `catalyst.yaml` runs `go run .`. The run file is optional —
`diagrid dev run --project <p> --id control-room -e GOWORK=off -- go run .`
does the same thing inline. On startup the runner registers into `agent-registry`
(it logs the store it chose), and the agent shows up under **Agents** in the
Diagrid Console. Kill the process mid-run and start it again with the same instance ID —
Diagrid picks the workflow back up where it left off.

Each example has a README with the full story (local Dapr, Diagrid, crash
recovery): [`examples/langchaingo`](https://github.com/diagridio/go-ai/blob/main/examples/langchaingo/README.md),
[`examples/eino`](https://github.com/diagridio/go-ai/blob/main/examples/eino/README.md).

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

`NewRunner` connects to Diagrid, registers the agent, and runs the graph —
there's no backend or registry to construct. Reuse an `InstanceID` to resume an
interrupted run.

## Usage analytics

`NewRunner` reports one anonymous usage event when it is called, once per
process. The Go module proxy publishes no download counts at all, so this is
how Diagrid sees which versions run on which platforms. Because it is one event per process, a
Kubernetes deployment produces one event per replica per restart: the numbers
count process starts, not deployments or users.

**What is sent:** the package name and this module's own version, operating
system, architecture, Go version, the agent framework and the installed
version of its library (`framework`, `framework_version`), `kind=agent`,
whether the process points at Diagrid or at a plain Dapr sidecar (`target`),
and whether it runs under a CI variable (`ci`). Nothing else: no application
data, configuration, app IDs, prompts, or hostnames. The receiving service is
[Scarf](https://scarf.sh). It derives coarse company and location information
from the request IP. How Scarf handles that data is described in
[Scarf's privacy policy](https://about.scarf.sh/privacy-policy).

**It never gets in the way:** the request runs on a background goroutine with
a one second timeout on the whole HTTP round trip, every failure is swallowed,
and the calling goroutine never waits. Blocked egress and air-gapped clusters
behave normally. Nothing is written to your application's output; a `DEBUG`
line on the default `log/slog` logger records whether the event was sent or
skipped, for operators who want to confirm an opt-out.

To opt out, set any of these environment variables before starting your
application:

```bash
export DO_NOT_TRACK=1
# or
export SCARF_NO_ANALYTICS=1
# or
export DIAGRID_NO_ANALYTICS=1
```

## Verified identity

The `identity` package verifies the caller's `X-Diagrid-User-Token` on every
inbound request and propagates it on outbound MCP and sub-agent calls:

```go
oauth := identity.OAuthConfig{Scopes: []string{"agent.invoke"}}
http.ListenAndServe(":8080", identity.Middleware(oauth)(mux))
```

Discovery precedence, JWKS rules, the status codes and the outbound client are
covered in [docs/identity.md](https://github.com/diagridio/go-ai/blob/main/docs/identity.md).

## Adding a framework

A framework needs one thing: node constructors that turn its models/chains into
`agent.NodeFunc` (see `adapters/eino/eino.go` — about 40 lines). Everything else
— the runner, durability, the registry — already works against `agent.Graph`.

## Requirements

Go 1.26.4+ (the Dapr Go SDK needs it) and the Dapr CLI for local runs. Tests are
offline — `make test` runs the engine and registry with no sidecar.

## Learn more

- [Diagrid documentation](https://docs.diagrid.io/catalyst/)
- Sibling SDKs for other languages:
  [python-ai](https://github.com/diagridio/python-ai),
  [typescript-ai](https://github.com/diagridio/typescript-ai),
  [dotnet-ai](https://github.com/diagridio/dotnet-ai),
  [java-ai](https://github.com/diagridio/java-ai),
  [rust-ai](https://github.com/diagridio/rust-ai)

## Layout

```
go-ai/
├── runner.go        # goai.Runner
├── agent/           # graph model + in-process interpreter
├── registry/        # agent directory → Diagrid agent-registry component
├── identity/        # inbound token verification + identity-aware http.Client
├── durable/         # DaprBackend
├── docs/            # reference docs (identity)
├── adapters/        # langchaingo, eino (own modules)
└── examples/        # runnable samples by framework (own modules)
```
