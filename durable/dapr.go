// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Runs a compiled graph as a Dapr Workflow on Diagrid Catalyst. Orchestration
// (routing, state merge) runs in the workflow and each node runs as a durable
// activity. One generic workflow plus one execute-node activity dispatch by
// graph/node name through a process-local registry, since closures are not
// serializable.
package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dapr/durabletask-go/workflow"
	daprclient "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/diagridio/go-ai/agent"
)

// graphRegistry resolves a graph name to its compiled closures in the worker
// process, since the workflow and activity are addressed by name.
var (
	graphMu       sync.RWMutex
	graphRegistry = map[string]*agent.CompiledGraph{}
)

func lookupGraph(name string) (*agent.CompiledGraph, bool) {
	graphMu.RLock()
	defer graphMu.RUnlock()
	cg, ok := graphRegistry[name]
	return cg, ok
}

// DaprBackend executes graphs as Dapr Workflows. Construct once per process,
// Register the graphs you will run, then Run them. Call Close on shutdown.
type DaprBackend struct {
	client *workflow.Client
	cancel context.CancelFunc
}

// NewDaprBackend registers the graph workflow under workflowName and one activity
// per node name, so each node shows under its own name in the Catalyst execution
// view. It then connects to the sidecar and starts the worker.
func NewDaprBackend(workflowName string, nodeNames []string) (*DaprBackend, error) {
	if workflowName == "" {
		return nil, fmt.Errorf("durable: workflow name is required")
	}
	r := workflow.NewRegistry()
	if err := r.AddWorkflowN(workflowName, graphWorkflow); err != nil {
		return nil, fmt.Errorf("durable: register workflow %q: %w", workflowName, err)
	}
	seen := map[string]bool{}
	for _, name := range nodeNames {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if err := r.AddActivityN(name, executeNodeActivity); err != nil {
			return nil, fmt.Errorf("durable: register node %q: %w", name, err)
		}
	}
	c, err := daprclient.NewWorkflowClient()
	if err != nil {
		return nil, fmt.Errorf("durable: workflow client: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.StartWorker(ctx, r); err != nil {
		cancel()
		return nil, fmt.Errorf("durable: start worker: %w", err)
	}
	return &DaprBackend{client: c, cancel: cancel}, nil
}

// Register makes a compiled graph runnable by this backend.
func (b *DaprBackend) Register(cg *agent.CompiledGraph) {
	graphMu.Lock()
	graphRegistry[cg.Name()] = cg
	graphMu.Unlock()
}

// Close stops the workflow worker.
func (b *DaprBackend) Close() {
	if b.cancel != nil {
		b.cancel()
	}
}

// Run schedules the graph as a Dapr Workflow and waits for completion.
func (b *DaprBackend) Run(ctx context.Context, cg *agent.CompiledGraph, input agent.State, opts RunOptions) (agent.State, error) {
	b.Register(cg)

	if opts.WorkflowName == "" {
		return nil, fmt.Errorf("durable: WorkflowName is required")
	}
	wfName := opts.WorkflowName
	wfIn := graphWorkflowInput{Graph: cg.Name(), State: input, MaxSteps: opts.MaxSteps, NodeRetry: opts.NodeRetry}
	schedOpts := []workflow.NewWorkflowOptions{workflow.WithInput(wfIn)}
	if opts.InstanceID != "" {
		schedOpts = append(schedOpts, workflow.WithInstanceID(opts.InstanceID))
	}

	id, err := b.client.ScheduleWorkflow(ctx, wfName, schedOpts...)
	if err != nil {
		// A caller-pinned ID that already exists is a resume: attach to the
		// existing run instead of failing. An unpinned collision is a real error.
		if opts.InstanceID == "" || status.Code(err) != codes.AlreadyExists {
			return nil, fmt.Errorf("durable: schedule workflow: %w", err)
		}
		id = opts.InstanceID
	}
	meta, err := b.client.WaitForWorkflowCompletion(ctx, id, workflow.WithFetchPayloads(true))
	if err != nil {
		return nil, fmt.Errorf("durable: wait for completion: %w", err)
	}
	if meta.RuntimeStatus != workflow.StatusCompleted {
		return nil, fmt.Errorf("durable: workflow %s finished with status %s", id, meta.RuntimeStatus)
	}

	var out agent.State
	if o := meta.Output; o != nil && o.GetValue() != "" {
		if err := json.Unmarshal([]byte(o.GetValue()), &out); err != nil {
			return nil, fmt.Errorf("durable: decode output: %w", err)
		}
	}
	return out, nil
}

type graphWorkflowInput struct {
	Graph     string           `json:"graph"`
	State     agent.State      `json:"state"`
	MaxSteps  int              `json:"max_steps"`
	NodeRetry *NodeRetryPolicy `json:"node_retry,omitempty"`
}

type executeNodeInput struct {
	Graph string      `json:"graph"`
	Node  string      `json:"node"`
	State agent.State `json:"state"`
}

// graphWorkflow drives a compiled graph node-by-node. Node execution goes to a
// durable activity. Routing and state merge run here and must be deterministic,
// since the workflow is replayed on recovery.
func graphWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var in graphWorkflowInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	cg, ok := lookupGraph(in.Graph)
	if !ok {
		return nil, fmt.Errorf("durable: graph %q not registered on worker", in.Graph)
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 100
	}
	state := in.State
	if state == nil {
		state = agent.State{}
	}

	current := cg.Entry()
	for step := 0; current != agent.END; step++ {
		if step >= maxSteps {
			return nil, fmt.Errorf("durable: exceeded max steps (%d)", maxSteps)
		}
		var updates agent.State
		actIn := executeNodeInput{Graph: in.Graph, Node: current, State: state}
		callOpts := []workflow.CallActivityOption{workflow.WithActivityInput(actIn)}
		if p := in.NodeRetry; p != nil {
			callOpts = append(callOpts, workflow.WithActivityRetryPolicy(&workflow.RetryPolicy{
				MaxAttempts:          p.MaxAttempts,
				InitialRetryInterval: p.InitialInterval,
				BackoffCoefficient:   p.BackoffCoefficient,
				MaxRetryInterval:     p.MaxInterval,
			}))
		}
		// The activity is registered under the node name, so the node shows under
		// its own name in Catalyst.
		if err := ctx.CallActivity(current, callOpts...).Await(&updates); err != nil {
			return nil, fmt.Errorf("durable: node %q: %w", current, err)
		}
		state = cg.ApplyUpdates(state, updates)

		// Background context is fine here - routers must be pure functions of
		// state under replay.
		next, err := cg.Next(context.Background(), current, state)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return state, nil
}

// executeNodeActivity runs a single node's function. This is the
// non-deterministic boundary where LLM/tool/MCP calls happen, checkpointed by Dapr.
func executeNodeActivity(ctx workflow.ActivityContext) (any, error) {
	var in executeNodeInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	cg, ok := lookupGraph(in.Graph)
	if !ok {
		return nil, fmt.Errorf("durable: graph %q not registered on worker", in.Graph)
	}
	updates, err := cg.RunNode(ctx.Context(), in.Node, in.State)
	if err != nil {
		return nil, err
	}
	return updates, nil
}
