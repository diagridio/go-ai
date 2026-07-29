// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Command mcp discovers the Model Context Protocol tools exposed by the Dapr
// sidecar and runs a LangChainGo agent that can call them, as a durable Dapr
// Workflow on Diagrid Catalyst.
//
// Tools only appear when an mcpserver resource is loaded in the sidecar. With
// none loaded, discovery returns nothing and the agent just answers directly.
// The model is OpenAI (OPENAI_API_KEY) and decides which tools to call.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	goai "github.com/diagridio/go-ai"
	"github.com/diagridio/go-ai/adapters/langchaingo"
	"github.com/diagridio/go-ai/agent"
	"github.com/diagridio/go-ai/mcp"

	daprclient "github.com/dapr/go-sdk/client"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

type state struct {
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

func main() {
	ctx := context.Background()

	// A Dapr client (for metadata) and a workflow client (to schedule the MCP
	// ListTools/CallTool workflows the sidecar registers).
	dc, err := daprclient.NewClient()
	if err != nil {
		fatal(fmt.Errorf("dapr client: %w", err))
	}
	defer dc.Close()
	wf, err := daprclient.NewWorkflowClient()
	if err != nil {
		fatal(fmt.Errorf("workflow client: %w", err))
	}

	discoverCtx, cancelDiscover := context.WithTimeout(ctx, 30*time.Second)
	tools, err := mcp.DiscoverTools(discoverCtx, dc, wf)
	cancelDiscover()
	if err != nil {
		fatal(fmt.Errorf("discover MCP tools: %w", err))
	}
	fmt.Printf("Discovered %d MCP tool(s):\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  - %s/%s: %s\n", t.Server, t.Name, t.Description)
	}

	graph := agent.NewGraph("mcp-assistant").
		AddNode("assistant", langchaingo.ToolModelNode(newModel(), tools,
			langchaingo.WithSystemPrompt("You are a helpful assistant. Use the available tools when they help."),
			langchaingo.WithInputKey("input"),
			langchaingo.WithOutputKey("output"),
		)).
		SetEntry("assistant").
		AddEdge("assistant", agent.END)

	runner, err := goai.NewRunner(ctx, goai.Config{
		Graph:     graph,
		Name:      "mcp-assistant",
		Framework: langchaingo.Framework,
		Role:      "MCP Assistant",
		Goal:      "Answer questions, calling MCP tools when they are available",
		Tools:     toolInfos(tools),
	})
	if err != nil {
		fatal(err)
	}
	defer runner.Close()

	out, err := runner.Invoke(ctx, state{Input: "What can you do? Use a tool if one is available."})
	if err != nil {
		fatal(err)
	}
	var result state
	if err := out.Into(&result); err != nil {
		fatal(err)
	}
	fmt.Printf("Agent %q ran as workflow %s\n", runner.Name(), runner.WorkflowName())
	fmt.Printf("Answer: %s\n", result.Output)
}

// newModel builds an OpenAI model from the environment. With tools bound by
// ToolModelNode, the model decides when to call them.
func newModel() llms.Model {
	if os.Getenv("OPENAI_API_KEY") == "" {
		fatal(fmt.Errorf("OPENAI_API_KEY is not set"))
	}
	m, err := openai.New(openai.WithModel(env("OPENAI_MODEL", "gpt-4o")))
	if err != nil {
		fatal(fmt.Errorf("openai model: %w", err))
	}
	return m
}

// toolInfos declares the discovered tools for the Catalyst Agents view.
func toolInfos(tools []mcp.Tool) []goai.ToolInfo {
	out := make([]goai.ToolInfo, len(tools))
	for i, t := range tools {
		args, _ := json.Marshal(t.Schema)
		out[i] = goai.ToolInfo{Name: t.Name, Description: t.Description, Args: string(args)}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
