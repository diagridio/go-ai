// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Command eino runs a two-node Eino agent ("Perimeter Watch") as a durable Dapr
// Workflow on Diagrid Catalyst. It uses the same runner and backend as the
// LangChainGo example, only the node implementations differ.
//
// The chat model is OpenAI. Set OPENAI_API_KEY (and optionally OPENAI_MODEL,
// default gpt-4o). See README.md to run it.
package main

import (
	"context"
	"fmt"
	"os"

	goai "github.com/diagridio/go-ai"
	"github.com/diagridio/go-ai/adapters/eino"
	"github.com/diagridio/go-ai/agent"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

// state is the graph's channel state as a typed struct. The json tags are the
// channel names the nodes read and write.
type state struct {
	Input  string `json:"input,omitempty"`
	Scan   string `json:"scan,omitempty"`
	Output string `json:"output,omitempty"`
}

func main() {
	ctx := context.Background()
	m := newModel(ctx)

	// Build an Eino agent graph:  scan -> alert -> END
	graph := agent.NewGraph("perimeter-watch").
		AddNode("scan", eino.ChatModelNode(m,
			eino.WithSystemPrompt("You monitor Jurassic Park perimeter sensors. Report anomalies tersely."),
			eino.WithInputKey("input"),
			eino.WithOutputKey("scan"),
		)).
		AddNode("alert", eino.ChatModelNode(m,
			eino.WithSystemPrompt("Raise the alarm and issue the containment code."),
			eino.WithInputKey("scan"),
			eino.WithOutputKey("output"),
		)).
		SetEntry("scan").
		AddEdge("scan", "alert").
		AddEdge("alert", agent.END)

	// The runner connects to Catalyst, registers the agent, and runs the graph.
	runner, err := goai.NewRunner(ctx, goai.Config{
		Graph:     graph,
		Name:      "perimeter-watch",
		Framework: eino.Framework,
		MaxSteps:  50,
		Role:      "Perimeter Watch",
		Goal:      "Detect fence breaches and trigger containment",
	})
	if err != nil {
		fatal(err)
	}
	defer runner.Close()

	out, err := runner.Invoke(ctx, state{
		Input: "Sector 04 fence sensor just dropped offline.",
	}, resumeOpts()...)
	if err != nil {
		fatal(err)
	}

	var result state
	if err := out.Into(&result); err != nil {
		fatal(err)
	}
	fmt.Printf("Agent %q (framework=%s) ran as workflow %s\n",
		runner.Name(), runner.Framework(), runner.WorkflowName())
	fmt.Printf("Scan : %s\n", result.Scan)
	fmt.Printf("Alert: %s\n", result.Output)
}

// resumeOpts pins a fixed workflow instance ID when GOAI_RUN is set, so a re-run
// resumes an interrupted run instead of starting a new one. Without it, Catalyst
// assigns an ID per run.
func resumeOpts() []goai.InvokeOptions {
	if id := os.Getenv("GOAI_RUN"); id != "" {
		return []goai.InvokeOptions{{InstanceID: id}}
	}
	return nil
}

// newModel builds an OpenAI chat model from the environment.
func newModel(ctx context.Context) model.BaseChatModel {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fatal(fmt.Errorf("OPENAI_API_KEY is not set"))
	}
	m, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey: key,
		Model:  env("OPENAI_MODEL", "gpt-4o"),
	})
	if err != nil {
		fatal(fmt.Errorf("openai chat model: %w", err))
	}
	return m
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
