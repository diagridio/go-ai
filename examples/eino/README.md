# Eino example — Perimeter Watch

A two-node Eino agent (`scan → alert`) that runs as a durable Dapr Workflow. It
uses the same Runner and durable backend as the LangChainGo example — only the
node constructors differ. That's the point: one durable runner, many frameworks.

The model is OpenAI, so export `OPENAI_API_KEY` before running (optionally
`OPENAI_MODEL`, default `gpt-4o`).

## Run it on Catalyst

You need the [`diagrid` CLI](https://docs.diagrid.io/catalyst/references/cli-reference/)
and a Catalyst account.

Set `PROJECT` to your Catalyst project (create one if needed).

```bash
# Log in to Catalyst.
diagrid login

# Enable agent infrastructure on the project. This provisions the managed
# workflow engine and the agent-registry component the Agents view reads —
# without it the agent won't show up.
diagrid project update $PROJECT --enable-agent-infrastructure

# Create the Agent — it appears under Agents in the console and scopes its
# identity into agent-registry, which the runtime must have before it can
# register. Use the name your app runs as (`appID` in catalyst.yaml); it must be
# unused (don't `appid create` it first, or they collide).
diagrid agent create perimeter-watch --project $PROJECT --wait

# Resolve dependencies once.
go mod tidy

# Run it. diagrid injects the Catalyst endpoint + API token into the process and
# starts it as defined in catalyst.yaml (command: go run .). The app inherits your
# shell env, so export the model key here.
export OPENAI_API_KEY=sk-...
diagrid dev run -f catalyst.yaml --project $PROJECT
```

Or skip the run file and pass the same settings inline:

```bash
diagrid dev run --project $PROJECT --app-id perimeter-watch -e GOWORK=off -- go run .
```

A successful run prints the two node outputs and exits 0 (wording varies with the
model):

```
Agent "perimeter-watch" (framework=eino) ran as workflow dapr.eino.perimeter_watch.workflow
Scan : <the model's anomaly report>
Alert: <the model's alarm + containment code>
```

The agent registers itself into the managed `agent-registry` component (auto-
discovered from the sidecar) and appears under **Agents** in the Catalyst console.

## Prove it's durable

Give the run a fixed instance ID, kill it partway, and start it again:

```bash
GOAI_RUN=demo-1 diagrid dev run -f catalyst.yaml --project $PROJECT
# Ctrl-C mid-run, then run the same command. Catalyst resumes the workflow from
# the last completed node instead of restarting it.
```

## Run it locally instead

Same code, against a local Dapr sidecar rather than Catalyst. Needs
[`dapr init`](https://docs.dapr.io/getting-started/) (installs the sidecar + a
local Redis) once:

```bash
export OPENAI_API_KEY=sk-...
dapr run --app-id perimeter-watch --resources-path ./resources -- go run .
```

`./resources/statestore.yaml` is the local Redis component that stands in for the
managed registry store.

> Running `go run .` directly (not through `diagrid`/`dapr`) from a checkout that
> sits under a parent `go.work`? Prefix it with `GOWORK=off` — this example is a
> self-contained module and shouldn't be pulled into an outer workspace.

## Test

The engine and registry unit tests run offline, from the repo root:

```bash
cd ../.. && make test
```

## What it does

`NewRunner` compiles the graph and registers the agent in the `agent-registry`
component so Catalyst and peers can find it. `Invoke` schedules the graph as a
workflow: each node (`scan`, `alert`) runs as a checkpointed `execute-node`
activity, and routing runs in the orchestrator. Requires Go 1.26.4+.
