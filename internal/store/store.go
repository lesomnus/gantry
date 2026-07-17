// Package store resolves the image stores a job references and reports them on
// GET /v1/store. A store is a registry (gantry reads/writes blobs) or an engine
// daemon (gantry triggers a pull). It builds the engine clients and exposes the
// registry configs the copy engine needs.
package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
)

// Set is the configured stores plus dialed engine clients.
type Set struct {
	order        []string
	byName       map[string]config.StoreConfig
	engines      map[string]down.Engine
	allowUnknown bool
}

func NewSet(stores map[string]config.StoreConfig, allowUnknown bool) (*Set, error) {
	s := &Set{
		byName:       make(map[string]config.StoreConfig, len(stores)),
		engines:      make(map[string]down.Engine, len(stores)),
		allowUnknown: allowUnknown,
	}
	for name, c := range stores {
		c.Name = name
		s.byName[name] = c
		s.order = append(s.order, name)
		if c.IsEngine() {
			eng, err := down.New(c)
			if err != nil {
				return nil, fmt.Errorf("store %q: %w", name, err)
			}
			s.engines[name] = eng
		}
	}
	sort.Strings(s.order) // stable display; store order is not significant
	return s, nil
}

func (s *Set) Close() error {
	var firstErr error
	for _, e := range s.engines {
		if err := e.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Registry resolves a from/to reference (a declared store name or, when allowed,
// a bare registry host) to a registry store config.
func (s *Set) Registry(ref string) (config.StoreConfig, error) {
	if ref == "" {
		return config.StoreConfig{}, fmt.Errorf("empty registry reference")
	}
	if c, ok := s.byName[ref]; ok {
		if !c.IsRegistry() {
			return config.StoreConfig{}, fmt.Errorf("store %q is a %s, not a registry", ref, c.Kind)
		}
		return c, nil
	}
	// Match a declared registry by host (so an image's own host resolves to its store).
	for _, name := range s.order {
		if c := s.byName[name]; c.IsRegistry() && c.Host == ref {
			return c, nil
		}
	}
	if !s.allowUnknown {
		return config.StoreConfig{}, fmt.Errorf("unknown store %q (enable allow_unknown_stores to use a bare host)", ref)
	}
	return config.StoreConfig{
		Name:    ref,
		Kind:    "oci",
		Host:    ref,
		Mode:    "copy",
		Rewrite: config.DefaultRewrite(),
	}, nil
}

// Engine resolves a reference to a declared engine store.
func (s *Set) Engine(name string) (down.Engine, error) {
	e, ok := s.engines[name]
	if !ok {
		if c, declared := s.byName[name]; declared {
			return nil, fmt.Errorf("store %q is a %s, not an engine", name, c.Kind)
		}
		return nil, fmt.Errorf("unknown engine store %q", name)
	}
	return e, nil
}

// Config returns a declared store's config by name.
func (s *Set) Config(name string) (config.StoreConfig, bool) {
	c, ok := s.byName[name]
	return c, ok
}

// PutEngine registers (or replaces) an engine store with a caller-supplied
// Engine, bypassing the daemon dial. Intended for tests that need a fake daemon
// behind the store set.
func (s *Set) PutEngine(c config.StoreConfig, eng down.Engine) {
	if _, exists := s.byName[c.Name]; !exists {
		s.order = append(s.order, c.Name)
		sort.Strings(s.order)
	}
	s.byName[c.Name] = c
	s.engines[c.Name] = eng
}

// Engines returns the dialed engine clients keyed by store name (for retention).
func (s *Set) Engines() map[string]down.Engine {
	out := make(map[string]down.Engine, len(s.engines))
	for k, v := range s.engines {
		out[k] = v
	}
	return out
}

// Names returns every declared store name, sorted.
func (s *Set) Names() []string {
	return append([]string(nil), s.order...)
}

// EngineNames returns every declared engine store name, sorted.
func (s *Set) EngineNames() []string {
	var out []string
	for _, name := range s.order {
		if s.byName[name].IsEngine() {
			out = append(out, name)
		}
	}
	return out
}

// Caps is the capability set reported per store.
type Caps struct {
	Read      bool `json:"read,omitempty"`      // registry: pull blobs
	Write     bool `json:"write,omitempty"`     // registry: push blobs
	Pull      bool `json:"pull,omitempty"`      // engine can be triggered to pull
	Verify    bool `json:"verify,omitempty"`    // engine can verify signatures (phase 2)
	GC        bool `json:"gc,omitempty"`        // engine supports image GC (phase 2)
	Reconcile bool `json:"reconcile,omitempty"` // engine supports inventory scans / untagged reaping (phase 2)
}

// Status is one GET /v1/store row.
type Status struct {
	Name         string `json:"name"`
	Kind         string `json:"kind" enums:"oci,docker,containerd"`
	Host         string `json:"host,omitempty"`
	Address      string `json:"address,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Ready        bool   `json:"ready"`           // registries: always true (from config); engines: live Ready() probe
	Error        string `json:"error,omitempty"` // engine readiness error, if not ready
	Capabilities Caps   `json:"capabilities"`    // what this store can do
}

// Status lists every store. Engine readiness is probed; registries are reported
// from config (not probed, since a remote like docker.io need not be reachable
// from gantry).
func (s *Set) StoreStatuses(ctx context.Context) []Status {
	out := make([]Status, 0, len(s.order))
	for _, name := range s.order {
		c := s.byName[name]
		st := Status{Name: c.Name, Kind: c.Kind}
		if c.IsRegistry() {
			st.Host = c.Host
			st.Mode = c.Mode
			st.Ready = true
			st.Capabilities = Caps{Read: true, Write: true}
		} else {
			st.Address = c.Address
			st.Namespace = c.Namespace
			eng := s.engines[c.Name]
			caps := down.Capabilities(eng)
			st.Capabilities = Caps{Pull: caps.Pull, Verify: caps.Verify, GC: caps.GC, Reconcile: caps.Reconcile}
			if err := eng.Ready(ctx); err != nil {
				st.Error = err.Error()
			} else {
				st.Ready = true
			}
		}
		out = append(out, st)
	}
	return out
}
