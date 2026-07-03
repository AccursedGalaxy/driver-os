// Package cache implements a small fixed-capacity cache used by a downstream
// service. It must never hold more than Max entries.
package cache

// LRU is a fixed-capacity cache. Put evicts the oldest entry once the cache
// would otherwise exceed Max.
type LRU struct {
	Max   int
	order []string
	data  map[string]int
}

// New returns an empty cache with the given capacity.
func New(max int) *LRU {
	return &LRU{Max: max, data: map[string]int{}}
}

// Put stores val under key, evicting the oldest entry if the cache is full.
func (l *LRU) Put(key string, val int) {
	if _, exists := l.data[key]; exists {
		l.order = append(l.order, key)
	}
	l.data[key] = val
	for len(l.order) > l.Max {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.data, oldest)
	}
}

// Get returns the value stored under key, if present.
func (l *LRU) Get(key string) (int, bool) {
	v, ok := l.data[key]
	return v, ok
}

// Len reports how many entries the cache currently holds.
func (l *LRU) Len() int { return len(l.data) }
