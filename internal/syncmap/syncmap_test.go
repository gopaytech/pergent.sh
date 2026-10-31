package syncmap

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// Many goroutines set, get, delete and list at once; run with -race.
func TestConcurrentSetGet(t *testing.T) {
	m := New[int](nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			for j := range 200 {
				m.Set(key, j)
				if v, ok := m.Get(key); !ok || v < 0 {
					t.Errorf("Get(%q) = %d, %v", key, v, ok)
				}
				m.Keys()
				if j%50 == 49 {
					m.Delete(key)
				}
			}
		}()
	}
	wg.Wait()
}

func TestMapBasics(t *testing.T) {
	init := map[string]string{"b": "2", "a": "1"}
	m := New(init)
	init["c"] = "3" // New copied init
	if got := m.Keys(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Keys = %v, want [a b]", got)
	}
	if v, ok := m.Get("a"); !ok || v != "1" {
		t.Errorf("Get(a) = %q, %v", v, ok)
	}
	m.Set("a", "9")
	if v, _ := m.Get("a"); v != "9" {
		t.Errorf("Get(a) after Set = %q", v)
	}
	m.Delete("a")
	m.Delete("missing")
	if _, ok := m.Get("a"); ok {
		t.Error("a still there after Delete")
	}

	var nilMap *Map[string]
	if _, ok := nilMap.Get("a"); ok || nilMap.Keys() != nil {
		t.Error("a nil Map holds something")
	}
	var zero Map[int]
	zero.Set("x", 1)
	if v, ok := zero.Get("x"); !ok || v != 1 {
		t.Errorf("zero Map Get(x) = %d, %v", v, ok)
	}
}
