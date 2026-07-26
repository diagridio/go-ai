// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package registry

import (
	"context"
	"strconv"
	"sync"
)

// StateStore is the state-store contract the registry needs. DaprStore backs it
// in production (local Dapr or Catalyst), MemoryStore backs the offline tests.
// Set with the etag from Get for optimistic concurrency, or an empty etag for
// first-write-only.
type StateStore interface {
	Name() string
	Get(ctx context.Context, key string, meta map[string]string) (value []byte, etag string, found bool, err error)
	Set(ctx context.Context, key string, value []byte, etag string, meta map[string]string) error
	Delete(ctx context.Context, key string, meta map[string]string) error
}

type etagConflict struct{}

func (etagConflict) Error() string { return "registry: etag conflict" }

// ErrETagConflict is returned by a StateStore on a concurrent write.
var ErrETagConflict error = etagConflict{}

// MemoryStore is a thread-safe, in-process StateStore for the offline tests.
type MemoryStore struct {
	name string
	mu   sync.Mutex
	data map[string][]byte
	etag map[string]int
}

func NewMemoryStore(name string) *MemoryStore {
	if name == "" {
		name = "memory"
	}
	return &MemoryStore{name: name, data: map[string][]byte{}, etag: map[string]int{}}
}

func (m *MemoryStore) Name() string { return m.name }

func (m *MemoryStore) Get(_ context.Context, key string, _ map[string]string) ([]byte, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, "", false, nil
	}
	return append([]byte(nil), v...), etagString(m.etag[key]), true, nil
}

func (m *MemoryStore) Set(_ context.Context, key string, value []byte, etag string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if etag != "" && etagString(m.etag[key]) != etag {
		return ErrETagConflict
	}
	m.data[key] = append([]byte(nil), value...)
	m.etag[key]++
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, key string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	delete(m.etag, key)
	return nil
}

// etagString renders a version counter as an etag. Version 0 (never written) has
// no etag, matching a missing key.
func etagString(v int) string {
	if v == 0 {
		return ""
	}
	return strconv.Itoa(v)
}
