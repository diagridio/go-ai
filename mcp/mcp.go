// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package mcp discovers and invokes Model Context Protocol tools served by the
// Dapr sidecar. When a mcpserver resource is loaded, Dapr registers each of its
// tools as built-in durable workflows (dapr.internal.mcp.<server>.ListTools and
// .CallTool.<tool>). This package schedules those workflows. No runtime or SDK
// changes are required.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	runtimev1 "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/durabletask-go/workflow"
	daprclient "github.com/dapr/go-sdk/client"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const workflowPrefix = "dapr.internal.mcp."

// Tool is a tool discovered from an MCP server. Call runs it as a durable Dapr
// workflow.
type Tool struct {
	Server      string
	Name        string
	Description string
	Schema      any // arguments JSON Schema
	call        func(ctx context.Context, args map[string]any) (string, error)
}

func (t Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

// DiscoverTools returns every tool from every MCP server loaded in the sidecar,
// or an empty slice when none are loaded.
func DiscoverTools(ctx context.Context, dc daprclient.Client, wf *workflow.Client) ([]Tool, error) {
	servers, err := discoverServers(ctx, dc)
	if err != nil {
		return nil, err
	}
	var tools []Tool
	for _, server := range servers {
		ts, err := listTools(ctx, wf, server)
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools for %q: %w", server, err)
		}
		tools = append(tools, ts...)
	}
	return tools, nil
}

// discoverServers reads the loaded mcpserver names from sidecar metadata. The
// go-sdk's typed GetMetadata omits this, so read it off the raw gRPC response.
func discoverServers(ctx context.Context, dc daprclient.Client) ([]string, error) {
	grpc := dc.GrpcClient()
	if grpc == nil {
		return nil, fmt.Errorf("mcp: dapr gRPC client unavailable")
	}
	resp, err := grpc.GetMetadata(ctx, &runtimev1.GetMetadataRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: get metadata: %w", err)
	}
	names := make([]string, 0, len(resp.GetMcpServers()))
	for _, s := range resp.GetMcpServers() {
		names = append(names, s.GetName())
	}
	return names, nil
}

func listTools(ctx context.Context, wf *workflow.Client, server string) ([]Tool, error) {
	out, err := runWorkflow(ctx, wf, workflowPrefix+server+".ListTools", nil)
	if err != nil {
		return nil, err
	}
	var res mcpsdk.ListToolsResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("mcp: decode tool list: %w", err)
	}
	tools := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		name := t.Name
		tools = append(tools, Tool{
			Server:      server,
			Name:        name,
			Description: t.Description,
			Schema:      t.InputSchema,
			call: func(ctx context.Context, args map[string]any) (string, error) {
				return callTool(ctx, wf, server, name, args)
			},
		})
	}
	return tools, nil
}

func callTool(ctx context.Context, wf *workflow.Client, server, tool string, args map[string]any) (string, error) {
	// Tool name is in the workflow name, arguments are the input.
	out, err := runWorkflow(ctx, wf, workflowPrefix+server+".CallTool."+tool, map[string]any{"arguments": args})
	if err != nil {
		return "", err
	}
	var res mcpsdk.CallToolResult
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("mcp: decode tool result: %w", err)
	}
	text := textContent(res.Content)
	if res.IsError {
		return "", fmt.Errorf("mcp: tool %s/%s failed: %s", server, tool, text)
	}
	return text, nil
}

// runWorkflow schedules a sidecar-registered workflow and returns its JSON output.
func runWorkflow(ctx context.Context, wf *workflow.Client, name string, input any) ([]byte, error) {
	opts := []workflow.NewWorkflowOptions{}
	if input != nil {
		opts = append(opts, workflow.WithInput(input))
	}
	id, err := wf.ScheduleWorkflow(ctx, name, opts...)
	if err != nil {
		return nil, fmt.Errorf("mcp: schedule %q: %w", name, err)
	}
	meta, err := wf.WaitForWorkflowCompletion(ctx, id, workflow.WithFetchPayloads(true))
	if err != nil {
		return nil, fmt.Errorf("mcp: wait for %q: %w", name, err)
	}
	if meta.RuntimeStatus != workflow.StatusCompleted {
		return nil, fmt.Errorf("mcp: workflow %q finished with status %s", name, meta.RuntimeStatus)
	}
	if o := meta.Output; o != nil {
		return []byte(o.GetValue()), nil
	}
	return []byte("null"), nil
}

func textContent(content []mcpsdk.Content) string {
	var b strings.Builder
	for _, c := range content {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
