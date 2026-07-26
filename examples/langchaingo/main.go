// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Command langchaingo runs a two-node LangChainGo agent ("Control Room Operator")
// as a durable Dapr Workflow on Diagrid Catalyst.
//
// The model is OpenAI. Set OPENAI_API_KEY (and optionally OPENAI_MODEL, default
// gpt-4o). See README.md to run it.
package main

import (
	"context"
	"fmt"
	"os"

	goai "github.com/diagridio/go-ai"
	"github.com/diagridio/go-ai/adapters/langchaingo"
	"github.com/diagridio/go-ai/agent"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

// state is the graph's channel state as a typed struct. The json tags are the
// channel names the nodes read and write.
type state struct {
	Input     string `json:"input,omitempty"`
	Diagnosis string `json:"diagnosis,omitempty"`
	Output    string `json:"output,omitempty"`
}

func main() {
	ctx := context.Background()
	model := pickModel()

	// Build a LangChainGo agent graph:  diagnose -> reboot -> END
	graph := agent.NewGraph("control-room").
		AddNode("diagnose", langchaingo.ModelNode(model,
			langchaingo.WithSystemPrompt("You are Ray Arnold, the Jurassic Park control room operator."),
			langchaingo.WithInputKey("input"),
			langchaingo.WithOutputKey("diagnosis"),
		)).
		AddNode("reboot", langchaingo.ModelNode(model,
			langchaingo.WithSystemPrompt("Reboot the grid and report the security code once systems are back online."),
			langchaingo.WithInputKey("diagnosis"),
			langchaingo.WithOutputKey("output"),
		)).
		SetEntry("diagnose").
		AddEdge("diagnose", "reboot").
		AddEdge("reboot", agent.END)

	// The runner connects to Catalyst, registers the agent, and runs the graph.
	runner, err := goai.NewRunner(ctx, goai.Config{
		Graph:     graph,
		Name:      "control-room",
		Framework: langchaingo.Framework,
		MaxSteps:  50,
		Role:      "Control Room Operator",
		Goal:      "Diagnose grid failures and bring the park systems back online",
	})
	if err != nil {
		fatal(err)
	}
	defer runner.Close()

	out, err := runner.Invoke(ctx, state{
		Input: "The park grid just went offline. Diagnose the cause.",
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
	fmt.Printf("Diagnosis: %s\n", result.Diagnosis)
	fmt.Printf("Operator : %s\n", result.Output)
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

// pickModel builds an OpenAI model from the environment.
func pickModel() llms.Model {
	if os.Getenv("OPENAI_API_KEY") == "" {
		fatal(fmt.Errorf("OPENAI_API_KEY is not set"))
	}
	m, err := openai.New(openai.WithModel(env("OPENAI_MODEL", "gpt-4o")))
	if err != nil {
		fatal(fmt.Errorf("openai model: %w", err))
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
