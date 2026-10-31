// Package syncmap is a map by name that many goroutines read while a few
// change it: the connected instances, their webhook secrets and sign-in
// providers, which the dashboard adds and removes while pergent.sh runs.
package syncmap

import (
	"maps"
	"slices"
	"sync"
)

// Map is a string-keyed map safe for concurrent use. Build one with New.
type Map[V any] struct {
	mu sync.RWMutex
	m  map[string]V
}

// New returns a Map holding a copy of init (which may be nil).
func New[V any](init map[string]V) *Map[V] {
	return &Map[V]{m: maps.Clone(init)}
}

// Get returns the value of key; ok is false when there is none. A nil Map
// holds nothing.
func (m *Map[V]) Get(key string) (V, bool) {
	if m == nil {
		var zero V
		return zero, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.m[key]
	return v, ok
}

// Set puts v at key, replacing any value there.
func (m *Map[V]) Set(key string, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]V{}
	}
	m.m[key] = v
}

// Delete removes key; a key that isn't there is not an error.
func (m *Map[V]) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
}

// Keys returns the keys, sorted.
func (m *Map[V]) Keys() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.m))
}
