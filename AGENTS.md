# AGENTS.md

Working notes for an AI assistant in this repo: the things that are easy to get
wrong. The README says what the project is; this does not repeat it.

## Six modules, and `./...` does not cross them

| Directory | Module path |
|---|---|
| `.` | `github.com/diagridio/go-ai` — `agent`, `durable`, `registry`, `mcp`, `runner.go` |
| `adapters/langchaingo`, `adapters/eino` | `.../adapters/<name>` |
| `examples/langchaingo`, `examples/eino`, `examples/mcp` | `.../examples/<name>` |

Every nested module has `replace github.com/diagridio/go-ai => ../..`; the
examples also replace their adapter.

- **`go build ./...` at the root builds five packages and stops.** `./...` never
  descends into a nested module — not even in workspace mode. `make build`,
  `make test` and `make vet` are root-only too. To cover the repo you must `cd`
  into each of the six directories and repeat. `.github/workflows/ci.yaml` does
  exactly that: it discovers `go.mod` from the tree and runs build, vet,
  `go test -race` and `gofmt -l` per module. Run the same six loops before
  calling a change green — this is what CI caught on its first run (all three
  `examples/` modules required `go-ai v0.0.0`, a version that does not exist).
- **Keep `GOWORK=off`** (set in the `Makefile`, every `catalyst.yaml`, and the CI
  consumability job). There is no `go.work` here — it is gitignored — so it is
  defensive; do not "tidy it away". If a `go.work` at *any* ancestor directory
  omits the module you are standing in, every go command there dies with
  `pattern ./...: directory prefix . does not contain modules listed in go.work
  or their selected dependencies`. Workspace mode also rejects `-mod=mod`.
  Adding a `go.work` fixes nothing: `./...` still will not descend.
- **Do not `go build ./...` inside `examples/*`.** Each example directory holds a
  *tracked* binary named after the directory (`examples/eino/eino` 28 MB;
  `examples/langchaingo/langchaingo` and `examples/mcp/mcp` 24 MB each,
  committed by accident). A build overwrites them and leaves a multi-megabyte
  diff. Use `go build -o /dev/null ./...` or `go vet ./...`, and never `git add`
  those paths.
- **Only the root module is tagged** (`v0.1.0`, `v0.1.1`). The adapters have no
  tags, so `go get .../adapters/eino@latest` resolves to a pseudo-version of
  `main`. Because of the `replace` directives a local build proves nothing about
  a consumer; if you touch a nested `go.mod`, the required `go-ai` version must
  actually exist on the proxy.

## Runtime model

- **State crosses JSON on every durable step.** `agent.State` is
  `map[string]any`, checkpointed between nodes, so `int` returns as `float64`
  and `[]string` as `[]any`. `agent.Execute` — the in-process interpreter — does
  *not* round-trip, so a graph whose tests pass under `Execute` can still fail
  under `durable.DaprBackend`. Assert the types JSON actually produces.
- **Routers are replayed; nodes are not.** `RouterFunc` and `CompiledGraph.Next`
  run in the workflow orchestrator, which Dapr replays on recovery. No LLM
  calls, no I/O, no clock, no randomness in a router — `graphWorkflow` passes
  `context.Background()` to make that explicit. All non-determinism belongs in a
  `NodeFunc`, which runs as a checkpointed activity.
- **Names are wire identifiers.** The workflow is registered as
  `dapr.<framework>.<name>.workflow` (`registry.BuildWorkflowName`: lowercased,
  spaces and dashes to underscores) and one activity is registered *per node
  name*. Renaming a node, agent or framework changes those names, so in-flight
  instances cannot replay.
- `durable.graphRegistry` is a process-global map keyed by graph name with no
  collision check: two `Runner`s built from `agent.NewGraph("assistant")` in one
  process silently share whichever registered last.

## Dependencies

- The workflow API is **`github.com/dapr/durabletask-go/workflow`** (v0.12.2).
  `github.com/dapr/go-sdk/workflow` existed in go-sdk v1.10.0–v1.13.0 and was
  **removed in v1.14.0**; this repo pins go-sdk v1.15.0, where it is gone. That
  is why stale docs and older answers still reach for it. `go-sdk/client` is
  still used, for `NewClient`, `NewWorkflowClient`, state and metadata.
- **A plain Dapr Workflow needs the Dapr Go SDK, not this repo.** `go-ai` is for
  *agents* — a graph of LLM/tool nodes made durable. Recommending it for a
  workflow sends someone to the wrong dependency.
- Go 1.26.4 in all six `go.mod` files. Keep them aligned.

## Catalyst

**The registry only works through an `Agent` resource. This is the trap.**
The Catalyst Agents view reads a per-project Postgres table, not your state
store. `agents:*` keys reach it only via a sidecar interceptor bound to the
component named exactly `agent-registry`, and that component is deliberately
scoped to a sentinel App ID that matches nothing
(`__diagrid_agent_registry_reserved__`); only the `Agent`/`DurableAgent`
controllers add a real App ID to its scopes. So an App ID with no `Agent`
resource never sees `agent-registry` in sidecar metadata, `runner.go` falls back
to `kvstore`, the write **succeeds with no error**, and the agent never appears.
The only signal is the stderr line the runner prints (`... "agent-registry" not
in sidecar scope ...`) — read it first when an agent is missing. Note that
`diagrid component list` shows `agent-registry` as scoped to "all app
identities" because the sentinel is filtered out of user-facing views; that
display is not evidence your App ID is scoped.

- **Create the Agent first, with `--wait`.** App, Agent, MCP-server and App ID
  names share one namespace per project, and `diagrid agent create <name>`
  provisions its *own* backing App ID under that name. If an App ID already holds
  the name you get a flat 409 — `the name "<name>" is already in use in this
  project; choose a different agent name` — and no Agent is created; delete the
  App ID or pick another name. If instead a bare App ID appears in the window
  before the Agent's controller reconciles, the controller refuses to adopt it and
  parks the Agent in `error` (`... recreate this agent with a different name`);
  it re-checks about every two minutes indefinitely, so removing the conflicting
  App ID clears it without recreating the Agent. `--wait` closes that window.
  Never `diagrid appid create` — it is a hidden legacy command with no such
  guard, which is the one path that can actually wedge an Agent; use `app`,
  `agent`, `mcpserver`.
- **`diagrid dev run` guards this, but not for this repo's run files.** It refuses
  to start when a run file's resource files reference `agent-*` components and any
  app has no matching Agent, and it never creates or patches an App ID an Agent
  owns (it only waits for readiness). The `catalyst.yaml` files here declare no
  resource files, so the first guard never fires — create the Agent yourself.
- **Check before creating a project.** Orgs are normally auto-provisioned a
  `default` project that already has everything, so `diagrid project create` is
  usually wrong. It is not guaranteed: bootstrap runs on org reconcile (not
  literally at signup), is skipped while the region is not ready, and is never
  redone if someone deletes it. Run `diagrid project list` first.
- **Do not assume the agent components exist.** In theory the auto-provisioned
  `default` gets all five (`agent-registry`, `agent-runtime`, `agent-workflow`,
  `agent-memory`, `agent-pubsub`) while a project you create with
  `--deploy-managed-kv` gets `agent-registry` only. In practice three projects in
  one live org gave three different answers, including one with four `agent-*`
  components but no `agent-registry` and no `kvstore` — there `NewRunner` fails
  outright at registration. Always check `diagrid component list --project <p>`.
  `GOAI_REGISTRY_STORE` or `Config.RegistryStore` overrides the discovery.
- **One managed pub/sub and one managed KV store per project, on every plan** —
  free, enterprise and internal all set `number_of_pubsubs_per_project: 1` and
  `number_of_kvstores_per_project: 1`. A plan limit Diagrid can raise per org,
  not an architectural invariant. The gate is `n >= limit` and the auto-created
  managed one counts, so in `default` a *second* create is rejected (`... has
  reached the configured maximum number of pubsubs per project ...`).
  `--ignore-if-exists` saves you only when you pass the name that already exists
  (its 409 short-circuits ahead of the quota check); any new name gets the quota
  400 and exit 1. The managed names are server-side and not yours to choose:
  `pubsub`, `kvstore`, and the workflow engine's reserved `workflows-state`.
- **`--enable-agent-infrastructure` is not on `diagrid project create`** (removed
  Aug 2026), only on `project update` — and *enabling* it on a cloud project with
  managed KV is rejected: `agent infrastructure is managed automatically for
  cloud projects with the managed KV store; ...`. It applies to BYOC/private
  regions or cloud projects without managed KV.
- **`diagrid agent` and `diagrid managed-agent` are different resources.**
  `agent` (kind `Agent`) fronts *your* app: `-e/--endpoint` plus exactly five
  archive flags — `--archive-binding-name`, `--archive-binding-type`,
  `--archive-completed`, `--archive-failed`, `--archive-terminated` (there is no
  `--archive-retention`). `managed-agent` (kind `DurableAgent`) is
  Catalyst-hosted and owns the LLM flags: `-l/--llm-component` **or** all of
  `--llm-provider`/`--llm-model`/`--llm-api-key`, optional `--llm-endpoint`,
  required `-r/--role` (there is no `--temperature`). **`managed-agent` is
  unusable by external users** — the parent is `cmd.Hidden` unconditionally and
  every subcommand is wrapped in `RestrictToDiagridUsers`, failing with `this
  command is only available to Diagrid employees`; `diagrid apply` rejects
  `DurableAgent` manifests the same way. Keep it out of this repo entirely.

## What is not tested

`go test -cover ./...` at the root, today: `agent` 72.9%, `registry` 72.3%
(offline, `MemoryStore` only — `daprstore.go` unexercised), `mcp` 8.1% (JSON
decoding and workflow naming only), **`durable` 0% and root `runner.go` 0% —
neither has a test file at all.** Every sidecar-touching path is uncovered:
`runner.go`, `durable/dapr.go`, `mcp.DiscoverTools`/`callTool`. CI cannot catch a
regression there, so changes to those files must be verified by running an
example against a real sidecar.

## Conventions

- Every `.go` file, the `Makefile` and the example YAML open with the two-line
  BUSL header (`Copyright (c) 2026-Present Diagrid Inc.` /
  `SPDX-License-Identifier: BUSL-1.1`).
- `gofmt` is CI-enforced per module.
- Errors are wrapped with the package name as prefix:
  `fmt.Errorf("durable: schedule workflow: %w", err)`.
- Package doc comments carry the design rationale (one generic workflow, the
  process-local graph registry, why routing must be pure). Update them; do not
  delete them.
- `ApplyUpdates` clones before merging so replay is safe. Do not mutate a `State`
  in place.
- Conventional commit subjects, and `git commit -s` — every non-merge commit in
  history is signed off. `main` is not branch-protected and CI is not a required
  check, so do not lean on the gate.

## Statements in this repo that are wrong — do not propagate

- `Makefile` mentions `GOAI_KVSTORE`; nothing reads it. The real variable is
  `GOAI_REGISTRY_STORE`. (`GOAI_RUN` is read only by the examples' `main.go`.)
- `Makefile` mentions `diagrid dev start`; no such command exists. `diagrid dev`
  has `scaffold`, `run`, `stop`, `status`, `cleanup`.
- `examples/README.md`, `examples/eino/README.md` and
  `examples/langchaingo/README.md` still instruct `diagrid project update <p>
  --enable-agent-infrastructure`. That is rejected on a managed-KV cloud
  project, `default` included; the root README is the correct version.
- The READMEs use `diagrid dev run --app-id`. It still works but is deprecated
  in favour of `-a/--id`.
