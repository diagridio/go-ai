// Copyright (c) 2026-Present Diagrid Inc.
// SPDX-License-Identifier: BUSL-1.1

package registry

import (
	"context"
	"strings"

	daprclient "github.com/dapr/go-sdk/client"
)

// DaprStore implements StateStore over a Dapr state store component.
type DaprStore struct {
	client daprclient.Client
	name   string
}

func NewDaprStore(client daprclient.Client, storeName string) *DaprStore {
	return &DaprStore{client: client, name: storeName}
}

func (s *DaprStore) Name() string { return s.name }

func (s *DaprStore) Get(ctx context.Context, key string, meta map[string]string) ([]byte, string, bool, error) {
	item, err := s.client.GetState(ctx, s.name, key, meta)
	if err != nil {
		return nil, "", false, err
	}
	if item == nil || len(item.Value) == 0 {
		return nil, "", false, nil
	}
	return item.Value, item.Etag, true, nil
}

func (s *DaprStore) Set(ctx context.Context, key string, value []byte, etag string, meta map[string]string) error {
	var err error
	if etag == "" {
		err = s.client.SaveState(ctx, s.name, key, value, meta)
	} else {
		err = s.client.SaveStateWithETag(ctx, s.name, key, value, etag, meta)
	}
	if err != nil && isETagConflict(err) {
		return ErrETagConflict
	}
	return err
}

func (s *DaprStore) Delete(ctx context.Context, key string, meta map[string]string) error {
	return s.client.DeleteState(ctx, s.name, key, meta)
}

// isETagConflict recognizes Dapr's concurrency failure (gRPC ABORTED).
func isETagConflict(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "etag") || strings.Contains(msg, "aborted")
}

var _ StateStore = (*DaprStore)(nil)
