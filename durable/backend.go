// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

// Package durable runs compiled agent graphs as Dapr Workflows on Diagrid
// Catalyst. Each node is a checkpointed activity, so a crash resumes from the
// last completed one.
package durable

import "time"

// RunOptions configures a durable run.
type RunOptions struct {
	InstanceID   string           // reuse to resume a run, empty for a fresh one
	MaxSteps     int              // node executions, default 100
	WorkflowName string           // workflow name to schedule under (required)
	NodeRetry    *NodeRetryPolicy // nil fails the run on a node's first error
}

// NodeRetryPolicy retries a failed node. The backend holds the wait between
// attempts, so the run stays recoverable even if the app exits.
//
// Mirrors the durabletask policy minus its Handle hook: this travels through the
// workflow input, so it must be JSON-safe.
type NodeRetryPolicy struct {
	MaxAttempts        int           `json:"max_attempts"`
	InitialInterval    time.Duration `json:"initial_interval"`
	BackoffCoefficient float64       `json:"backoff_coefficient"`
	MaxInterval        time.Duration `json:"max_interval"`
}
