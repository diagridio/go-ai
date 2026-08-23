# MCP tools example

A LangChainGo agent that discovers the Model Context Protocol tools loaded in the
Dapr sidecar and can call them — as a durable Dapr Workflow on Diagrid Catalyst.

## How it works

Dapr registers each loaded `mcpserver` resource's tools as built-in durable
workflows (`dapr.internal.mcp.<server>.ListTools` and `.CallTool.<tool>`). The
`github.com/diagridio/go-ai/mcp` package:

1. reads the loaded MCP server names from the sidecar metadata,
2. schedules each server's `ListTools` workflow to discover its tools, and
3. exposes each tool with a `Call` that schedules the `CallTool` workflow.

`langchaingo.ToolModelNode` binds those tools to the model and runs the
model↔tool loop inside one durable node; no runtime or SDK changes are needed.

## Run it on Catalyst

Set `PROJECT` to a Catalyst project with agent infrastructure enabled (see the
other examples). This example runs under its own `mcp-assistant` App ID (set in
`catalyst.yaml`), so its MCP server doesn't overlap the other examples.

Create the `mcp-assistant` Agent (it appears under Agents in the console and
scopes its identity into agent-registry), then add an MCP server scoped to that
identity. This example points at DeepWiki, a public, no-auth MCP server:

```bash
diagrid agent create mcp-assistant --project $PROJECT --wait

diagrid mcpserver create deepwiki --project $PROJECT \
  --url https://mcp.deepwiki.com/mcp --transport streamable-http --scope mcp-assistant

go mod tidy
export OPENAI_API_KEY=sk-...
diagrid dev run -f catalyst.yaml --project $PROJECT
```

Or skip the run file and pass the same settings inline:

```bash
diagrid dev run --project $PROJECT --id mcp-assistant -e GOWORK=off -- go run .
```

The sidecar loads the MCP server, so the example discovers its tools and the model
calls the ones it needs. Output varies with the model, e.g.:

```
Discovered 3 MCP tool(s):
  - deepwiki/read_wiki_structure: Get a list of documentation topics for a GitHub repository.
  - deepwiki/read_wiki_contents: View documentation about a GitHub repository.
  - deepwiki/ask_question: Ask any question about a GitHub repository ...
Agent "mcp-assistant" ran as workflow dapr.langchaingo.mcp_assistant.workflow
Answer: I can read a repository's wiki structure, view its docs, and answer questions about it.
```

With no MCP server loaded, discovery returns nothing and the agent answers
directly — the example runs either way. Remove the server with
`diagrid mcpserver delete deepwiki --project $PROJECT`.

## Run it locally

`resources/mcpserver.yaml` defines a local MCP server over stdio
(`npx @modelcontextprotocol/server-everything`); `resources/statestore.yaml` backs
the workflow engine. With `dapr init` done:

```bash
export OPENAI_API_KEY=sk-...
dapr run --app-id mcp-assistant --resources-path ./resources -- go run .
```

## Model

The agent uses OpenAI (`OPENAI_API_KEY`, optionally `OPENAI_MODEL`, default
`gpt-4o`). `ToolModelNode` binds the discovered MCP tools to the model, which
decides which to call.

## What it demonstrates

- MCP tool discovery straight from sidecar metadata (no config).
- A tool call executed as a durable Dapr workflow, checkpointed like any node.
- The agent still registers under **Agents** in the Catalyst console. Requires Go 1.26.4+.
