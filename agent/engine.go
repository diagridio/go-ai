// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"context"
	"errors"
	"fmt"
)

// ErrMaxSteps is returned when a run exceeds RunOptions.MaxSteps.
var ErrMaxSteps = errors.New("agent: exceeded max steps")

// StepRecord is emitted after each node runs, for observability in tests/tools.
type StepRecord struct {
	Step    int      `json:"step"`
	Node    string   `json:"node"`
	Updated []string `json:"updated"`
	Next    string   `json:"next"`
}

// RunOptions configures a single in-process execution.
type RunOptions struct {
	MaxSteps int              // node executions, default 100
	OnStep   func(StepRecord) // called after each node
}

// Execute runs a compiled graph to completion in-process and returns the final
// state. It is the reference interpreter for unit-testing graph wiring without a
// sidecar, not the durable path. Production runs go through durable.DaprBackend,
// which shares the same routing and state-merge logic so behavior matches.
func Execute(ctx context.Context, cg *CompiledGraph, input State, opts RunOptions) (State, error) {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 100
	}

	current := cg.entry
	state := input.Clone()
	if state == nil {
		state = State{}
	}

	for step := 0; current != END; step++ {
		if step >= opts.MaxSteps {
			return state, fmt.Errorf("%w (%d) in graph %q", ErrMaxSteps, opts.MaxSteps, cg.name)
		}
		fn, ok := cg.nodes[current]
		if !ok {
			return state, fmt.Errorf("agent: no function registered for node %q", current)
		}

		updates, err := fn(ctx, state)
		if err != nil {
			return state, fmt.Errorf("agent: node %q: %w", current, err)
		}
		updated := merge(state, updates, cg.reducers)

		next, err := cg.Next(ctx, current, state)
		if err != nil {
			return state, err
		}
		if opts.OnStep != nil {
			opts.OnStep(StepRecord{Step: step + 1, Node: current, Updated: updated, Next: next})
		}
		current = next
	}
	return state, nil
}
