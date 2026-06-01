package llm

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds named providers so they can be configured once and used
// interchangeably or all at once (decision 7). It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Add registers a provider under a name, overwriting any existing entry. It
// returns the registry so calls can be chained.
func (r *Registry) Add(name string, p Provider) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
	return r
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// MustGet returns the provider registered under name, panicking if absent.
func (r *Registry) MustGet(name string) Provider {
	p, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("llm: no provider registered as %q", name))
	}
	return p
}

// Names returns the registered names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns every registered provider, ordered by name.
func (r *Registry) All() []Provider {
	names := r.Names()
	out := make([]Provider, 0, len(names))
	for _, name := range names {
		p, _ := r.Get(name)
		out = append(out, p)
	}
	return out
}
