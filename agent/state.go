// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"encoding/json"
	"fmt"
)

// State is the shared working memory that flows through a graph. Each key is a
// channel, and a node returns only the channels it updates. Values must be
// JSON-serializable since the durable backend checkpoints State between steps.
// Use ToState / State.Into to work with typed structs at the boundary.
type State map[string]any

// ToState converts v into a State. v may be a State or map (returned as-is), nil
// (empty State), or any JSON-serializable value such as a struct whose json tags
// match your channel names.
func ToState(v any) (State, error) {
	switch s := v.(type) {
	case nil:
		return State{}, nil
	case State:
		return s, nil
	case map[string]any:
		return State(s), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("agent: encode state: %w", err)
		}
		var m State
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("agent: decode state: %w", err)
		}
		return m, nil
	}
}

// Into decodes the State into v, a pointer to a struct or map, matching channels
// to fields by json tag.
func (s State) Into(v any) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("agent: encode state: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("agent: decode state: %w", err)
	}
	return nil
}

// Reducer combines a channel's existing value with an update. existing is nil on
// the first write. A reducer lets a channel accumulate instead of overwrite.
type Reducer func(existing, update any) any

// AppendReducer appends update onto an existing []any channel, the common reducer
// for message/history channels.
func AppendReducer(existing, update any) any {
	var out []any
	if existing != nil {
		if s, ok := existing.([]any); ok {
			out = append(out, s...)
		} else {
			out = append(out, existing)
		}
	}
	if s, ok := update.([]any); ok {
		out = append(out, s...)
	} else if update != nil {
		out = append(out, update)
	}
	return out
}

// Clone returns a shallow copy of the State map. Treat channel values as
// immutable once written.
func (s State) Clone() State {
	out := make(State, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// merge applies updates onto dst using per-channel reducers, returning the
// written channels.
func merge(dst State, updates State, reducers map[string]Reducer) []string {
	changed := make([]string, 0, len(updates))
	for k, v := range updates {
		if r, ok := reducers[k]; ok {
			dst[k] = r(dst[k], v)
		} else {
			dst[k] = v
		}
		changed = append(changed, k)
	}
	return changed
}
