// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package registry is the agent directory: records in a Dapr state store that let
// agents discover each other and let Catalyst list them as agents. Keys are
// agents:<team>:<name> plus an agents:<team>:_index list, under partition key
// agents:<team>. Team is an optional grouping (Config.Team) that defaults to
// "default", so most keys are agents:default:<name>.
package registry

import (
	"fmt"
	"strings"
	"time"
)

// DefaultTeam is used when an agent does not specify a team.
const DefaultTeam = "default"

const schemaVersion = "edge"
const agentTypeDurable = "durable"

// AgentRecord is the per-agent registry document in the shape the Catalyst Agents
// view reads. Unused sections (memory, llm, pubsub) are omitted.
type AgentRecord struct {
	Version      string         `json:"version"`
	Agent        AgentSpec      `json:"agent"`
	Name         string         `json:"name"`
	RegisteredAt string         `json:"registered_at"`
	Registry     *RegistryRef   `json:"registry,omitempty"`
	Tools        []ToolMetadata `json:"tools,omitempty"`
}

// ToolMetadata describes a tool an agent exposes. Args holds the arguments JSON Schema.
type ToolMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Args        string `json:"args"`
}

type AgentSpec struct {
	AppID         string         `json:"appid"`
	Type          string         `json:"type"`
	Orchestrator  bool           `json:"orchestrator"`
	Framework     string         `json:"framework,omitempty"`
	Role          string         `json:"role,omitempty"`
	Goal          string         `json:"goal,omitempty"`
	MaxIterations int            `json:"max_iterations,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type RegistryRef struct {
	Name         string `json:"name,omitempty"`          // team
	ResourceName string `json:"resource_name,omitempty"` // state store name
}

// AgentInfo is the input to NewAgentRecord.
type AgentInfo struct {
	Name      string
	Framework string
	Team      string
	AppID     string
	Role      string
	Goal      string
	MaxSteps  int
	Nodes     []string
	Tools     []ToolMetadata
}

// NewAgentRecord builds a registry record with the workflow name and timestamp
// filled in.
func NewAgentRecord(info AgentInfo) AgentRecord {
	team := info.Team
	if team == "" {
		team = DefaultTeam
	}
	meta := map[string]any{"workflow_name": BuildWorkflowName(info.Framework, info.Name)}
	if len(info.Nodes) > 0 {
		meta["nodes"] = info.Nodes
	}
	return AgentRecord{
		Version:      schemaVersion,
		Name:         info.Name,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		Agent: AgentSpec{
			AppID:         info.AppID,
			Type:          agentTypeDurable,
			Framework:     info.Framework,
			Role:          info.Role,
			Goal:          info.Goal,
			MaxIterations: info.MaxSteps,
			Metadata:      meta,
		},
		Registry: &RegistryRef{Name: team},
		Tools:    info.Tools,
	}
}

// WorkflowName returns the durable workflow name recorded in the agent metadata.
func (r AgentRecord) WorkflowName() string {
	if r.Agent.Metadata != nil {
		if w, ok := r.Agent.Metadata["workflow_name"].(string); ok {
			return w
		}
	}
	return ""
}

// BuildWorkflowName derives the durable workflow name for an agent:
// dapr.<framework>.<name>.workflow.
func BuildWorkflowName(framework, name string) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		return strings.NewReplacer(" ", "_", "-", "_").Replace(s)
	}
	return fmt.Sprintf("dapr.%s.%s.workflow", norm(framework), norm(name))
}
