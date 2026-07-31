// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package goai runs an agent graph as a durable agent on Diagrid Catalyst.
// NewRunner compiles a graph, registers it as an agent, and runs it as a Dapr
// workflow where each node is a checkpointed activity.
package goai

import (
	"context"
	"fmt"
	"os"

	"github.com/diagridio/go-ai/agent"
	"github.com/diagridio/go-ai/durable"
	"github.com/diagridio/go-ai/registry"

	daprclient "github.com/dapr/go-sdk/client"
)

// Config for NewRunner. Graph, Name, and Framework are required.
type Config struct {
	Graph     *agent.Graph
	Name      string
	Framework string // ex "langchaingo", "eino". Shapes the workflow name.
	MaxSteps  int    // node executions per run, default 100
	Role      string
	Goal      string
	Team      string // discovery group, default "default"
	AppID     string // read from the sidecar when empty
	// RegistryStore backs the agent registry. Empty picks $GOAI_REGISTRY_STORE,
	// then "agent-registry" if the sidecar has it, then "kvstore".
	RegistryStore string
	// Tools lists the agent's tools for the Catalyst view. Declaration only, wire
	// them into the graph separately.
	Tools []ToolInfo
	// NodeRetry retries a failed node instead of ending the run. Nil fails on
	// the first error.
	NodeRetry *durable.NodeRetryPolicy
}

// ToolInfo declares a tool an agent exposes. Args holds the arguments JSON Schema.
type ToolInfo struct {
	Name        string
	Description string
	Args        string
}

type Runner struct {
	cg           *agent.CompiledGraph
	backend      *durable.DaprBackend
	client       daprclient.Client
	record       registry.AgentRecord
	workflowName string
	nodeRetry    *durable.NodeRetryPolicy
}

// NewRunner compiles the graph, connects to Catalyst, registers the agent, and
// returns a ready Runner. Call Close when done.
func NewRunner(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Graph == nil {
		return nil, fmt.Errorf("goai: Graph is required")
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("goai: Name is required")
	}
	if cfg.Framework == "" {
		return nil, fmt.Errorf("goai: Framework is required")
	}
	cg, err := cfg.Graph.Compile()
	if err != nil {
		return nil, fmt.Errorf("goai: compile graph: %w", err)
	}

	workflowName := registry.BuildWorkflowName(cfg.Framework, cfg.Name)

	backend, err := durable.NewDaprBackend(workflowName)
	if err != nil {
		return nil, fmt.Errorf("goai: durable backend: %w", err)
	}
	client, err := daprclient.NewClient()
	if err != nil {
		backend.Close()
		return nil, fmt.Errorf("goai: dapr client: %w", err)
	}

	md, _ := client.GetMetadata(ctx)

	appID := cfg.AppID
	if appID == "" && md != nil {
		appID = md.ID
	}
	if appID == "" {
		appID = cfg.Name
	}

	regStore := cfg.RegistryStore
	if regStore == "" {
		regStore = envOr("GOAI_REGISTRY_STORE", "")
	}
	if regStore == "" {
		regStore = discoverRegistryStore(md)
	}
	// The Agents view only reads agent-registry, and only for app ids backed by an
	// Agent resource. Log the target so a silent fallback to kvstore is visible.
	if regStore == agentRegistryComponent {
		fmt.Fprintf(os.Stderr, "goai: registering agent %q into %q\n", cfg.Name, regStore)
	} else {
		fmt.Fprintf(os.Stderr, "goai: registering agent %q into %q - %q not in sidecar scope, "+
			"create an Agent resource for this app id so it appears in the Catalyst Agents view\n",
			cfg.Name, regStore, agentRegistryComponent)
	}
	reg := registry.New(registry.NewDaprStore(client, regStore))
	record := registry.NewAgentRecord(registry.AgentInfo{
		Name:      cfg.Name,
		Framework: cfg.Framework,
		Team:      cfg.Team,
		AppID:     appID,
		Role:      cfg.Role,
		Goal:      cfg.Goal,
		MaxSteps:  cfg.MaxSteps,
		Nodes:     cg.NodeNames(),
		Tools:     toolMetadata(cfg.Tools),
	})
	if err := reg.Register(ctx, &record); err != nil {
		backend.Close()
		client.Close()
		return nil, fmt.Errorf("goai: register agent: %w", err)
	}

	backend.Register(cg)
	return &Runner{
		cg:           cg,
		backend:      backend,
		client:       client,
		record:       record,
		workflowName: workflowName,
		nodeRetry:    cfg.NodeRetry,
	}, nil
}

// Close releases the workflow worker and the Dapr client.
func (r *Runner) Close() {
	if r.backend != nil {
		r.backend.Close()
	}
	if r.client != nil {
		r.client.Close()
	}
}

// InvokeOptions tunes a single run. All fields optional.
type InvokeOptions struct {
	InstanceID string // reuse to resume an interrupted run
	MaxSteps   int    // overrides Config.MaxSteps
}

// Invoke runs the graph to completion (or resumes it) and returns the final
// state. input may be a State, a map, or a struct with json tags matching the
// channels. Decode the result with State.Into.
func (r *Runner) Invoke(ctx context.Context, input any, opts ...InvokeOptions) (agent.State, error) {
	var opt InvokeOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	state, err := agent.ToState(input)
	if err != nil {
		return nil, err
	}
	maxSteps := opt.MaxSteps
	if maxSteps == 0 {
		maxSteps = r.record.Agent.MaxIterations
	}
	return r.backend.Run(ctx, r.cg, state, durable.RunOptions{
		InstanceID:   opt.InstanceID,
		MaxSteps:     maxSteps,
		WorkflowName: r.workflowName,
		NodeRetry:    r.nodeRetry,
	})
}

func (r *Runner) Name() string         { return r.record.Name }
func (r *Runner) Framework() string    { return r.record.Agent.Framework }
func (r *Runner) WorkflowName() string { return r.workflowName }

func toolMetadata(tools []ToolInfo) []registry.ToolMetadata {
	if len(tools) == 0 {
		return nil
	}
	out := make([]registry.ToolMetadata, len(tools))
	for i, t := range tools {
		out[i] = registry.ToolMetadata{Name: t.Name, Description: t.Description, Args: t.Args}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// discoverRegistryStore returns "agent-registry" when the sidecar has it, else
// "kvstore" for local Dapr.
func discoverRegistryStore(md *daprclient.GetMetadataResponse) string {
	if md != nil {
		for _, c := range md.RegisteredComponents {
			if c.Name == agentRegistryComponent {
				return agentRegistryComponent
			}
		}
	}
	return "kvstore"
}

const agentRegistryComponent = "agent-registry"
