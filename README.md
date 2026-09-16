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
| `identity` | Verified inbound identity: `net/http` middleware that checks the caller's `X-Diagrid-User-Token` against the sidecar's JWKS, plus an identity-aware `http.Client` that propagates the caller on outbound calls. |
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

## Verified identity

Wrap your handler to verify the caller's `X-Diagrid-User-Token` on every request:

```go
oauth := identity.OAuthConfig{Scopes: []string{"agent.invoke"}}
http.ListenAndServe(":8080", identity.Middleware(oauth)(mux))
```

The issuer, JWKS endpoint and audience are discovered from the sidecar, so
there's nothing else to configure. A handler reads the verified caller with
`identity.UserFromContext(r.Context())`; a request that fails verification never
reaches it, and comes back as `{"error": "oauth.<reason>"}` with 401, 403 or 503.
`VerifiedUser.Scopes` arrives deduplicated and ordinally sorted, which is what
every Diagrid SDK hands the handler for the same token.

`RequireAuth` governs the no-token case alone. It is unset by default, which
resolves to true, so `OAuthConfig{}` rejects a request carrying no token with
401 `oauth.missing_token`. Set it to false (it's a `*bool`) when health and
readiness endpoints have to share the app — a token that *is* present is always
verified either way, and an invalid one is always rejected.

To check the token yourself, supply a `TokenVerifier` to the middleware rather
than to the policy:

```go
handler := identity.Middleware(oauth, identity.WithVerifier(myVerifier))(mux)
```

### Discovery precedence

Coordinates are resolved from four sources, first answer winning:

1. **Explicit** `OAuthConfig` fields (`Issuer`, `JWKSURI`, `Audience`).
2. **The local sidecar** `/v1.0/metadata`, reached on loopback at the port in
   `CATALYST_DAPR_HTTP_PORT` or `DAPR_HTTP_PORT`.
3. **The remote sidecar** `/v1.0/metadata`, at `DAPR_HTTP_ENDPOINT`, with
   `DAPR_API_TOKEN` sent as the `dapr-api-token` header when it is set. This is
   what `diagrid dev run` gives an app on a developer machine, where nothing is
   listening on loopback.
4. **The environment**: `DIAGRID_DP_SENTRY_ISSUER` and
   `DIAGRID_DP_SENTRY_AUDIENCE`.

Local precedes remote deliberately: a deployed in-cluster app keeps answering
from the loopback call rather than paying for a network round trip, and the
remote endpoint is not contacted at all when the local one answered. The JWKS
endpoint itself resolves explicit > published by the sidecar for the issuer
being verified > derived as `<issuer>/jwks.json`.

A JWKS endpoint must be https. Loopback is exempt, because that is where the
local sidecar publishes its keys. `AllowInsecureJWKS` is the deliberate opt-out
for everything else, and it relaxes the rule to plain **http only** — a `file://`
or otherwise non-http(s) URI stays refused with the flag set. The key set is the
entire root of trust: an on-path attacker who rewrites a plaintext response
mints tokens this package would accept.

### Status and code

| Status | Code | When |
| --- | --- | --- |
| 401 | `oauth.missing_token` | No `X-Diagrid-User-Token` and `RequireAuth` resolves to true |
| 503 | `oauth.not_configured` | No identity coordinates could be resolved, so no verifier could be built |
| 503 | `oauth.verifier_unavailable` | The verifier could not answer: JWKS key material has not loaded, or verification failed unexpectedly, so the token was neither accepted nor rejected |
| 401 | `oauth.expired` | The `exp` claim is in the past (120s clock skew allowed) |
| 401 | `oauth.invalid_issuer` | The `iss` claim does not match the resolved issuer |
| 401 | `oauth.invalid_audience` | An audience is configured and the `aud` claim does not contain it |
| 401 | `oauth.invalid_signature` | The signature does not verify against the JWKS key the token names |
| 401 | `oauth.decode_error` | The token is not a well-formed JWT |
| 401 | `oauth.invalid_token` | Well-formed and correctly signed but unacceptable: a signing algorithm outside the RS256/ES256 allowlist, or a missing `exp`/`iss`/`sub` |
| 403 | `oauth.missing_scope` | The verified token lacks a scope `OAuthConfig.Scopes` requires |

Every rejection carries `Cache-Control: no-store`, because an authorization
decision is good for exactly one request.

Outbound MCP and sub-agent calls carry the caller on behalf of whom you're
acting. Construct the client once and make ordinary calls with the request
context — there are no identity headers to assemble:

```go
client := identity.NewHTTPClient(nil)   // once, at startup

req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, mcpURL, nil)
resp, err := client.Do(req)
```

It's a plain `*http.Client`, so it goes wherever one is expected — an MCP
client, a generated API client, anything. The caller's token is read from the
request context at send time, which is what makes one long-lived shared client
safe: concurrent requests each carry their own caller. Outside a verified
request (a cron or pub/sub trigger) the call goes out unauthenticated with the
header omitted rather than sent empty, and a redirect off the addressed origin
drops the token instead of handing it to the host the redirect names.

If you already own a client you can't replace, install the same behaviour on it:

```go
owned.Transport = &identity.Transport{Base: owned.Transport}
```

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
├── identity/        # inbound token verification + identity-aware http.Client
├── durable/         # DaprBackend
├── adapters/        # langchaingo, eino (own modules)
└── examples/        # runnable samples by framework (own modules)
```
