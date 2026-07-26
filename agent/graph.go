// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package agent is a framework-agnostic graph engine for AI agents. A compiled
// graph runs one node at a time so a durable backend can checkpoint each node and
// resume from the last completed one. Standard library only.
package agent

import (
	"context"
	"fmt"
)

// Virtual entry and exit node names.
const (
	START = "__start__"
	END   = "__end__"
)

// NodeFunc runs one node. It gets the accumulated state and returns only the
// channels it updates. On error the run aborts and a durable backend retries the
// same node.
type NodeFunc func(ctx context.Context, state State) (State, error)

// RouterFunc picks a branch out of a conditional edge, returning a key looked up
// in the edge mapping.
type RouterFunc func(ctx context.Context, state State) (string, error)

type conditional struct {
	router  RouterFunc
	mapping map[string]string // routing key -> target node (or END)
}

// Graph builds a directed agent graph. Methods chain and record the first error,
// which Compile returns.
type Graph struct {
	name     string
	nodes    map[string]NodeFunc
	order    []string // insertion order
	edges    map[string]string
	conds    map[string]conditional
	reducers map[string]Reducer
	entry    string
	err      error
}

// NewGraph starts a graph with the given name.
func NewGraph(name string) *Graph {
	return &Graph{
		name:     name,
		nodes:    map[string]NodeFunc{},
		edges:    map[string]string{},
		conds:    map[string]conditional{},
		reducers: map[string]Reducer{},
	}
}

func (g *Graph) fail(err error) *Graph {
	if g.err == nil {
		g.err = err
	}
	return g
}

// AddNode registers a node function under name.
func (g *Graph) AddNode(name string, fn NodeFunc) *Graph {
	if name == "" || name == START || name == END {
		return g.fail(fmt.Errorf("agent: invalid node name %q", name))
	}
	if _, exists := g.nodes[name]; exists {
		return g.fail(fmt.Errorf("agent: duplicate node %q", name))
	}
	if fn == nil {
		return g.fail(fmt.Errorf("agent: nil function for node %q", name))
	}
	g.nodes[name] = fn
	g.order = append(g.order, name)
	return g
}

// AddEdge adds a static edge from src to dst. Use END as dst to finish, or
// SetEntry to mark the entry.
func (g *Graph) AddEdge(src, dst string) *Graph {
	if src == START {
		g.entry = dst
		return g
	}
	if _, ok := g.conds[src]; ok {
		return g.fail(fmt.Errorf("agent: node %q already has a conditional edge", src))
	}
	g.edges[src] = dst
	return g
}

// AddConditionalEdge routes dynamically: after src runs, router returns a key
// that mapping resolves to the next node (or END). This is how agents loop.
func (g *Graph) AddConditionalEdge(src string, router RouterFunc, mapping map[string]string) *Graph {
	if router == nil || len(mapping) == 0 {
		return g.fail(fmt.Errorf("agent: conditional edge on %q needs a router and mapping", src))
	}
	if _, ok := g.edges[src]; ok {
		return g.fail(fmt.Errorf("agent: node %q already has a static edge", src))
	}
	g.conds[src] = conditional{router: router, mapping: mapping}
	return g
}

// SetEntry marks the entry node.
func (g *Graph) SetEntry(name string) *Graph {
	g.entry = name
	return g
}

// WithReducer sets a channel's reducer so writes accumulate instead of overwrite.
func (g *Graph) WithReducer(channel string, r Reducer) *Graph {
	g.reducers[channel] = r
	return g
}

// Compile validates the graph and returns an immutable CompiledGraph.
func (g *Graph) Compile() (*CompiledGraph, error) {
	if g.err != nil {
		return nil, g.err
	}
	if len(g.nodes) == 0 {
		return nil, fmt.Errorf("agent: graph %q has no nodes", g.name)
	}
	entry := g.entry
	if entry == "" {
		entry = g.order[0]
	}
	if _, ok := g.nodes[entry]; !ok {
		return nil, fmt.Errorf("agent: entry node %q is not defined", entry)
	}
	validTarget := func(t string) bool { _, ok := g.nodes[t]; return ok || t == END }
	for src, dst := range g.edges {
		if _, ok := g.nodes[src]; !ok {
			return nil, fmt.Errorf("agent: edge from unknown node %q", src)
		}
		if !validTarget(dst) {
			return nil, fmt.Errorf("agent: edge %q -> unknown target %q", src, dst)
		}
	}
	for src, c := range g.conds {
		if _, ok := g.nodes[src]; !ok {
			return nil, fmt.Errorf("agent: conditional edge from unknown node %q", src)
		}
		for key, dst := range c.mapping {
			if !validTarget(dst) {
				return nil, fmt.Errorf("agent: conditional edge %q[%q] -> unknown target %q", src, key, dst)
			}
		}
	}

	cg := &CompiledGraph{
		name:     g.name,
		nodes:    g.nodes,
		order:    g.order,
		edges:    g.edges,
		conds:    g.conds,
		reducers: g.reducers,
		entry:    entry,
	}
	cg.config = cg.describe()
	return cg, nil
}

// CompiledGraph is a validated, executable graph. Safe to share across runs and
// goroutines.
type CompiledGraph struct {
	name     string
	nodes    map[string]NodeFunc
	order    []string
	edges    map[string]string
	conds    map[string]conditional
	reducers map[string]Reducer
	entry    string
	config   GraphConfig
}

func (cg *CompiledGraph) Name() string { return cg.name }

// Config returns the serializable graph description.
func (cg *CompiledGraph) Config() GraphConfig { return cg.config }

// NodeNames returns the node names in insertion order.
func (cg *CompiledGraph) NodeNames() []string {
	names := make([]string, 0, len(cg.config.Nodes))
	for _, n := range cg.config.Nodes {
		names = append(names, n.Name)
	}
	return names
}

func (cg *CompiledGraph) Entry() string { return cg.entry }

// RunNode runs a single node's function, used by durable backends that run one
// node per activity.
func (cg *CompiledGraph) RunNode(ctx context.Context, name string, state State) (State, error) {
	fn, ok := cg.nodes[name]
	if !ok {
		return nil, fmt.Errorf("agent: no function registered for node %q", name)
	}
	return fn(ctx, state)
}

// ApplyUpdates merges a node's partial state onto a copy of state using the
// channel reducers. It does not mutate the input, keeping replay safe.
func (cg *CompiledGraph) ApplyUpdates(state, updates State) State {
	out := state.Clone()
	if out == nil {
		out = State{}
	}
	merge(out, updates, cg.reducers)
	return out
}

// Next returns the node to run after src, or END. It must be deterministic since
// the durable orchestrator replays it on recovery.
func (cg *CompiledGraph) Next(ctx context.Context, src string, state State) (string, error) {
	if dst, ok := cg.edges[src]; ok {
		return dst, nil
	}
	if c, ok := cg.conds[src]; ok {
		key, err := c.router(ctx, state)
		if err != nil {
			return "", fmt.Errorf("agent: router for %q: %w", src, err)
		}
		dst, ok := c.mapping[key]
		if !ok {
			return "", fmt.Errorf("agent: router for %q returned unmapped key %q", src, key)
		}
		return dst, nil
	}
	return END, nil
}
