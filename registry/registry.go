// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const keyPrefix = "agents:"
const agentsField = "agents" // field in the index doc holding the name list
const maxETagAttempts = 10   // index-update retry cap

// Registry is a durable directory of agents backed by a StateStore. Safe for
// concurrent use, index mutations are serialized per team via ETag retries.
type Registry struct {
	store StateStore
}

func New(store StateStore) *Registry { return &Registry{store: store} }

func effectiveTeam(team string) string {
	if team == "" {
		return DefaultTeam
	}
	return team
}

func agentKey(team, name string) string {
	return fmt.Sprintf("%s%s:%s", keyPrefix, effectiveTeam(team), name)
}

func indexKey(team string) string {
	return fmt.Sprintf("%s%s:_index", keyPrefix, effectiveTeam(team))
}

// partitionMeta co-locates a team's records under one partition.
func partitionMeta(team string) map[string]string {
	return map[string]string{
		"partitionKey": keyPrefix + effectiveTeam(team),
		"contentType":  "application/json",
	}
}

// Register writes the per-agent record, then adds the name to the team index
// under an ETag-protected retry loop. Team and resource name are backfilled.
func (r *Registry) Register(ctx context.Context, rec *AgentRecord) error {
	if rec.Name == "" {
		return errors.New("registry: agent name required")
	}
	if rec.Registry == nil {
		rec.Registry = &RegistryRef{}
	}
	team := effectiveTeam(rec.Registry.Name)
	rec.Registry.Name = team
	if rec.Registry.ResourceName == "" {
		rec.Registry.ResourceName = r.store.Name()
	}

	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("registry: marshal record: %w", err)
	}
	if err := r.store.Set(ctx, agentKey(team, rec.Name), blob, "", partitionMeta(team)); err != nil {
		return fmt.Errorf("registry: save agent record: %w", err)
	}

	return r.updateIndex(ctx, team, func(list []string) ([]string, bool) {
		for _, n := range list {
			if n == rec.Name {
				return list, false
			}
		}
		return append(list, rec.Name), true
	})
}

// Deregister removes an agent record and its index entry.
func (r *Registry) Deregister(ctx context.Context, team, name string) error {
	team = effectiveTeam(team)
	if err := r.store.Delete(ctx, agentKey(team, name), partitionMeta(team)); err != nil {
		return fmt.Errorf("registry: delete agent record: %w", err)
	}
	return r.updateIndex(ctx, team, func(list []string) ([]string, bool) {
		for i, n := range list {
			if n == name {
				return append(list[:i:i], list[i+1:]...), true
			}
		}
		return list, false
	})
}

// List returns the names of all agents registered in a team.
func (r *Registry) List(ctx context.Context, team string) ([]string, error) {
	idx, _, err := r.loadIndex(ctx, team)
	if err != nil {
		return nil, err
	}
	return idx[agentsField], nil
}

// Get returns a single agent's record.
func (r *Registry) Get(ctx context.Context, team, name string) (AgentRecord, bool, error) {
	var rec AgentRecord
	team = effectiveTeam(team)
	blob, _, found, err := r.store.Get(ctx, agentKey(team, name), partitionMeta(team))
	if err != nil || !found {
		return rec, found, err
	}
	if err := json.Unmarshal(blob, &rec); err != nil {
		return rec, true, fmt.Errorf("registry: unmarshal record: %w", err)
	}
	return rec, true, nil
}

// loadIndex reads the team index, returning {"agents": []} when absent.
func (r *Registry) loadIndex(ctx context.Context, team string) (map[string][]string, string, error) {
	blob, etag, found, err := r.store.Get(ctx, indexKey(team), partitionMeta(team))
	if err != nil {
		return nil, "", fmt.Errorf("registry: load index: %w", err)
	}
	idx := map[string][]string{agentsField: {}}
	if found && len(blob) > 0 {
		if err := json.Unmarshal(blob, &idx); err != nil {
			return nil, "", fmt.Errorf("registry: unmarshal index: %w", err)
		}
		if idx[agentsField] == nil {
			idx[agentsField] = []string{}
		}
	}
	return idx, etag, nil
}

// updateIndex read-modify-writes the team index, retrying on ETag conflicts.
// mutate returns the new list and whether a write is needed.
func (r *Registry) updateIndex(ctx context.Context, team string, mutate func([]string) ([]string, bool)) error {
	for attempt := 1; attempt <= maxETagAttempts; attempt++ {
		idx, etag, err := r.loadIndex(ctx, team)
		if err != nil {
			return err
		}
		newList, changed := mutate(idx[agentsField])
		if !changed {
			return nil
		}
		blob, err := json.Marshal(map[string][]string{agentsField: newList})
		if err != nil {
			return fmt.Errorf("registry: marshal index: %w", err)
		}
		err = r.store.Set(ctx, indexKey(team), blob, etag, partitionMeta(team))
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrETagConflict) {
			return fmt.Errorf("registry: save index: %w", err)
		}
		// Concurrent writer won, retry with a fresh etag.
	}
	return fmt.Errorf("registry: index update failed after %d etag retries", maxETagAttempts)
}
