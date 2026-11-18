package storage

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests of everything above the storage layer. It never serves real URLs.
type Memory struct {
	mu      sync.Mutex
	Objects map[string][]byte
	Types   map[string]string
	Deleted []string
}

func NewMemory() *Memory { return &Memory{Objects: map[string][]byte{}, Types: map[string]string{}} }

// Put simulates the client's direct upload to a presigned URL.
func (m *Memory) Put(key, contentType string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Objects[key] = data
	m.Types[key] = contentType
}

func (m *Memory) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Objects[key]
	return ok
}

func (m *Memory) PresignPut(key, contentType string, size int64, ttl time.Duration) (Upload, error) {
	return Upload{URL: "memory://put/" + key, Headers: map[string]string{"Content-Type": contentType}, Expires: time.Now().Add(ttl)}, nil
}

func (m *Memory) PresignGet(key, name string, ttl time.Duration) (string, time.Time, error) {
	return "memory://get/" + key, time.Now().Add(ttl), nil
}

func (m *Memory) Head(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.Objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Size: int64(len(b)), ContentType: m.Types[key]}, nil
}

func (m *Memory) Range(_ context.Context, key string, n int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.Objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	if int64(len(b)) > n {
		b = b[:n]
	}
	return append([]byte(nil), b...), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Objects, key)
	m.Deleted = append(m.Deleted, key)
	return nil
}
