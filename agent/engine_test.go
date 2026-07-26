// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"context"
	"errors"
	"testing"
)

// buildLinearGraph: start -> incubate -> hatch -> END, accumulating a log.
func buildLinearGraph(t *testing.T) *CompiledGraph {
	t.Helper()
	node := func(label string) NodeFunc {
		return func(_ context.Context, s State) (State, error) {
			return State{"log": []any{label}}, nil
		}
	}
	cg, err := NewGraph("dino").
		WithReducer("log", AppendReducer).
		AddNode("incubate", node("incubate")).
		AddNode("hatch", node("hatch")).
		SetEntry("incubate").
		AddEdge("incubate", "hatch").
		AddEdge("hatch", END).
		Compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return cg
}

func TestExecuteLinear(t *testing.T) {
	cg := buildLinearGraph(t)
	out, err := Execute(context.Background(), cg, State{"log": []any{"start"}}, RunOptions{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	log, _ := out["log"].([]any)
	if len(log) != 3 || log[0] != "start" || log[2] != "hatch" {
		t.Fatalf("unexpected log: %v", log)
	}
}

func TestConditionalRouting(t *testing.T) {
	cg, err := NewGraph("router").
		AddNode("classify", func(_ context.Context, s State) (State, error) {
			return State{"kind": "raptor"}, nil
		}).
		AddNode("contain", func(_ context.Context, s State) (State, error) {
			return State{"result": "contained"}, nil
		}).
		AddNode("release", func(_ context.Context, s State) (State, error) {
			return State{"result": "released"}, nil
		}).
		SetEntry("classify").
		AddConditionalEdge("classify", func(_ context.Context, s State) (string, error) {
			return s["kind"].(string), nil
		}, map[string]string{"raptor": "contain", "brachiosaur": "release"}).
		AddEdge("contain", END).
		AddEdge("release", END).
		Compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := Execute(context.Background(), cg, State{}, RunOptions{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out["result"] != "contained" {
		t.Fatalf("expected contained, got %v", out["result"])
	}
}

// TestApplyUpdatesRouting verifies the shared CompiledGraph primitives used by
// both Execute and the Dapr backend: reducer merge + conditional Next.
func TestApplyUpdatesRouting(t *testing.T) {
	cg, err := NewGraph("shared").
		WithReducer("log", AppendReducer).
		AddNode("a", func(_ context.Context, s State) (State, error) { return State{"log": []any{"a"}}, nil }).
		AddNode("b", func(_ context.Context, s State) (State, error) { return State{"log": []any{"b"}}, nil }).
		SetEntry("a").
		AddEdge("a", "b").
		AddEdge("b", END).
		Compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	st := cg.ApplyUpdates(State{"log": []any{"x"}}, State{"log": []any{"a"}})
	if got, _ := st["log"].([]any); len(got) != 2 {
		t.Fatalf("ApplyUpdates should merge via reducer, got %v", st["log"])
	}
	next, err := cg.Next(context.Background(), "a", st)
	if err != nil || next != "b" {
		t.Fatalf("Next(a) = %q, %v; want b", next, err)
	}
}

// TestConfigStable checks that Config()/NodeNames() are deterministic: nodes in
// insertion order and conditional targets sorted, so registry metadata does not
// churn between runs.
func TestConfigStable(t *testing.T) {
	build := func() *CompiledGraph {
		cg, err := NewGraph("stable").
			AddNode("classify", func(_ context.Context, s State) (State, error) { return nil, nil }).
			AddNode("contain", func(_ context.Context, s State) (State, error) { return nil, nil }).
			AddNode("release", func(_ context.Context, s State) (State, error) { return nil, nil }).
			SetEntry("classify").
			AddConditionalEdge("classify", func(_ context.Context, s State) (string, error) { return "raptor", nil },
				map[string]string{"raptor": "contain", "brachiosaur": "release", "escape": END}).
			AddEdge("contain", END).
			AddEdge("release", END).
			Compile()
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return cg
	}

	want := []string{"classify", "contain", "release"}
	for i := 0; i < 20; i++ { // map iteration order is randomized per run
		cg := build()
		if got := cg.NodeNames(); !equalStrings(got, want) {
			t.Fatalf("NodeNames = %v, want %v", got, want)
		}
		for _, e := range cg.Config().Edges {
			if e.Conditional && !equalStrings(e.Targets, []string{"__end__", "contain", "release"}) {
				t.Fatalf("conditional targets not sorted: %v", e.Targets)
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMaxSteps(t *testing.T) {
	cg, err := NewGraph("loop").
		AddNode("a", func(_ context.Context, s State) (State, error) { return nil, nil }).
		SetEntry("a").
		AddConditionalEdge("a", func(_ context.Context, s State) (string, error) {
			return "again", nil
		}, map[string]string{"again": "a"}).
		Compile()
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := Execute(context.Background(), cg, State{}, RunOptions{MaxSteps: 5}); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("expected ErrMaxSteps, got %v", err)
	}
}
