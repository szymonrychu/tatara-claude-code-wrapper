package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// MemStore is an in-memory Store for tests and local development. It is safe for
// concurrent use and keys blobs verbatim (no prefixing), matching what callers
// pass to the real Client. The upload/restore hooks (subtask 4) reuse it.
type MemStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore { return &MemStore{data: map[string][]byte{}} }

var _ Store = (*MemStore)(nil)

func (m *MemStore) Put(_ context.Context, key string, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return nil
}

func (m *MemStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("storage: key %q not found", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *MemStore) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok, nil
}
