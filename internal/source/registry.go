package source

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/yusiwen/flowhub/internal/rules"
)

// Factory builds one source adapter from the configuration that belongs to it: the
// trigger policy the file resolved, and the source's own `prompt_file` content.
//
// It is a function rather than an interface so that a source's own constructor
// (which may take more than these) stays its own business: the registry's only job
// is to answer "can this binary build the source this file names?".
//
// The prompt text is passed in rather than read by the adapter because reading and
// validating it is the configuration file's job: a `prompt_file` that is missing,
// empty or oversized has to refuse the start before any adapter exists.
type Factory func(policy rules.Policy, instructions string) Source

// Registry is the compile-time list of the sources this binary can build.
//
// ADR 0001 chose in-process interfaces over plugins, so "registering a source"
// means calling Register from `main`. The registry exists anyway, for one reason:
// a configuration file names sources, and a name that cannot be built has to be
// refused *by name* at startup with the list of what the binary knows, rather than
// surfacing later as a delivery that can never be decoded.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a source under its own name. A duplicate is an error rather than a
// silent override: two adapters answering to one name is a wiring mistake.
func (r *Registry) Register(name string, factory Factory) error {
	name = normalizeName(name)
	if name == "" {
		return fmt.Errorf("source: a name is required")
	}
	if factory == nil {
		return fmt.Errorf("source %s: a factory is required", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.factories[name]; taken {
		return fmt.Errorf("source %s is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

// Names lists the registered names, sorted, for an error message or a report.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether a name can be built.
func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[normalizeName(name)]
	return ok
}

// Build constructs one adapter, or explains that the name is unknown and what is
// known instead. instructions is the source's own `prompt_file`, or "" when the
// configuration declared none.
func (r *Registry) Build(name string, policy rules.Policy, instructions string) (Source, error) {
	normalized := normalizeName(name)
	if r == nil {
		return nil, fmt.Errorf("source %s: this binary has no source registry", name)
	}
	r.mu.RLock()
	factory, ok := r.factories[normalized]
	known := make([]string, 0, len(r.factories))
	for candidate := range r.factories {
		known = append(known, candidate)
	}
	r.mu.RUnlock()
	if !ok {
		sort.Strings(known)
		return nil, fmt.Errorf("source %s is not one this binary can build (it knows: %v)", name, known)
	}
	return factory(policy, instructions), nil
}

// normalizeName keeps a name comparable to what a configuration file writes: source
// names appear in file keys and in the audit record, so they stay lowercase.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
