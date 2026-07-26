// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package registry

import (
	"context"
	"sync"
	"testing"
)

func record(name, framework, team string, nodes []string) *AgentRecord {
	r := NewAgentRecord(AgentInfo{Name: name, Framework: framework, Team: team, AppID: name, Nodes: nodes})
	return &r
}

func TestRegisterAndDiscover(t *testing.T) {
	ctx := context.Background()
	reg := New(NewMemoryStore("jp-registry"))

	reg.Register(ctx, record("mr-dna", "langchaingo", "control-room", []string{"greet"}))
	reg.Register(ctx, record("ray-arnold", "eino", "control-room", []string{"diagnose", "reboot"}))
	// Duplicate register must be idempotent in the index.
	reg.Register(ctx, record("mr-dna", "langchaingo", "control-room", nil))

	names, err := reg.List(ctx, "control-room")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 agents, got %v", names)
	}

	rec, found, err := reg.Get(ctx, "control-room", "ray-arnold")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if rec.Agent.Type != agentTypeDurable {
		t.Fatalf("expected agent type %q, got %q", agentTypeDurable, rec.Agent.Type)
	}
	if rec.WorkflowName() != "dapr.eino.ray_arnold.workflow" {
		t.Fatalf("unexpected workflow name %q", rec.WorkflowName())
	}
	if rec.Registry.ResourceName != "jp-registry" {
		t.Fatalf("expected store name backfilled, got %q", rec.Registry.ResourceName)
	}
}

func TestDeregister(t *testing.T) {
	ctx := context.Background()
	reg := New(NewMemoryStore(""))
	reg.Register(ctx, record("a", "langchaingo", "", nil))
	reg.Register(ctx, record("b", "eino", "", nil))
	if err := reg.Deregister(ctx, "", "a"); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	names, _ := reg.List(ctx, "")
	if len(names) != 1 || names[0] != "b" {
		t.Fatalf("expected [b], got %v", names)
	}
	if _, found, _ := reg.Get(ctx, "", "a"); found {
		t.Fatal("agent a should be gone")
	}
}

// TestConcurrentRegister exercises the ETag-protected index retry loop.
func TestConcurrentRegister(t *testing.T) {
	ctx := context.Background()
	reg := New(NewMemoryStore(""))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := string(rune('A' + n))
			reg.Register(ctx, record(name, "langchaingo", "swarm", nil))
		}(i)
	}
	wg.Wait()
	names, err := reg.List(ctx, "swarm")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 20 {
		t.Fatalf("expected 20 agents after concurrent registration, got %d: %v", len(names), names)
	}
}
