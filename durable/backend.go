// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package durable runs compiled agent graphs as Dapr Workflows on Diagrid
// Catalyst. Each node is a checkpointed activity, so a crash resumes from the
// last completed one.
package durable

// RunOptions configures a durable run.
type RunOptions struct {
	InstanceID   string // reuse to resume a run, empty for a fresh one
	MaxSteps     int    // node executions, default 100
	WorkflowName string // workflow name to schedule under (required)
}
