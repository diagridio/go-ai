// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import "sort"

// GraphConfig is the serializable description of a compiled graph, used by the
// registry and Catalyst UI to show an agent's shape.
type GraphConfig struct {
	Name         string       `json:"name"`
	Nodes        []NodeConfig `json:"nodes"`
	Edges        []EdgeConfig `json:"edges"`
	Entry        string       `json:"entry_point"`
	FinishPoints []string     `json:"finish_points"`
}

// NodeConfig describes a single node.
type NodeConfig struct {
	Name string `json:"name"`
}

// EdgeConfig describes an edge. Conditional edges set Conditional and list the
// possible Targets, leaving Target empty since the choice is made at runtime.
type EdgeConfig struct {
	Source      string   `json:"source"`
	Target      string   `json:"target,omitempty"`
	Conditional bool     `json:"conditional,omitempty"`
	Targets     []string `json:"targets,omitempty"`
}

// describe builds the GraphConfig, walking nodes in insertion order and sorting
// conditional targets so the output is stable across runs.
func (cg *CompiledGraph) describe() GraphConfig {
	cfg := GraphConfig{Name: cg.name, Entry: cg.entry}
	finish := map[string]bool{}

	for _, n := range cg.order {
		cfg.Nodes = append(cfg.Nodes, NodeConfig{Name: n})
		switch {
		case cg.edges[n] != "":
			dst := cg.edges[n]
			cfg.Edges = append(cfg.Edges, EdgeConfig{Source: n, Target: dst})
			finish[n] = dst == END
		default:
			c, ok := cg.conds[n]
			if !ok {
				finish[n] = true // no outgoing edge means finish
				continue
			}
			targets := make([]string, 0, len(c.mapping))
			for _, dst := range c.mapping {
				targets = append(targets, dst)
				finish[n] = finish[n] || dst == END
			}
			sort.Strings(targets)
			cfg.Edges = append(cfg.Edges, EdgeConfig{Source: n, Conditional: true, Targets: targets})
		}
	}

	for _, n := range cg.order {
		if finish[n] {
			cfg.FinishPoints = append(cfg.FinishPoints, n)
		}
	}
	return cfg
}
