// Package down is the downstream seam: it triggers configured docker and
// containerd daemons to pull a warmed cache reference. New target kinds are one
// file plus a factory case; phase-2 features (verify, GC) are optional
// capability interfaces discovered by type assertion.
package down

import (
	"context"
	"fmt"

	"github.com/lesomnus/gantry/cmd/config"
)

// Target is the minimal contract every downstream kind must satisfy.
type Target interface {
	Name() string
	Kind() string // "docker" | "containerd"
	// Ready reports whether the daemon is reachable.
	Ready(ctx context.Context) error
	// Pull makes the daemon pull ref from the cache registry.
	Pull(ctx context.Context, ref string) error
	Close() error
}

// Verifier is the phase-2 signature-verification capability (optional).
type Verifier interface {
	Verify(ctx context.Context, ref string) error
}

// Collector is the phase-2 image-GC capability (optional).
type Collector interface {
	Collect(ctx context.Context) error
}

// Caps describes which capabilities a target implements; it is serialized on
// GET /v1/target so phase-2 features light up without handler changes.
type Caps struct {
	Pull   bool `json:"pull"`
	Verify bool `json:"verify"`
	GC     bool `json:"gc"`
}

// Capabilities discovers a target's optional capabilities by type assertion.
func Capabilities(t Target) Caps {
	_, verify := t.(Verifier)
	_, gc := t.(Collector)
	return Caps{Pull: true, Verify: verify, GC: gc}
}

// TargetStatus is the GET /v1/target row for one target.
type TargetStatus struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Address   string `json:"address"`
	Namespace string `json:"namespace,omitempty"`
	Ready     bool   `json:"ready"`
	Error     string `json:"error,omitempty"`
	Caps      Caps   `json:"capabilities"`
}

type entry struct {
	cfg    config.TargetConfig
	target Target
}

// Registry is the set of configured downstream targets.
type Registry struct {
	entries []entry
	byName  map[string]Target
}

// NewRegistry dials every configured target. A target that fails to construct
// aborts startup; unreachable ones are tolerated and reported as not-ready.
func NewRegistry(cfgs []config.TargetConfig) (*Registry, error) {
	r := &Registry{byName: make(map[string]Target, len(cfgs))}
	for _, c := range cfgs {
		if c.Name == "" {
			return nil, fmt.Errorf("target with kind %q has no name", c.Kind)
		}
		if _, dup := r.byName[c.Name]; dup {
			return nil, fmt.Errorf("duplicate target name %q", c.Name)
		}
		t, err := NewTarget(c)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", c.Name, err)
		}
		r.entries = append(r.entries, entry{cfg: c, target: t})
		r.byName[c.Name] = t
	}
	return r, nil
}

func (r *Registry) Get(name string) (Target, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.byName[name]
	return t, ok
}

func (r *Registry) All() []Target {
	if r == nil {
		return nil
	}
	out := make([]Target, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.target)
	}
	return out
}

// Status pings every target and reports its readiness and capabilities.
func (r *Registry) Status(ctx context.Context) []TargetStatus {
	if r == nil {
		return []TargetStatus{}
	}
	out := make([]TargetStatus, 0, len(r.entries))
	for _, e := range r.entries {
		s := TargetStatus{
			Name:      e.cfg.Name,
			Kind:      e.cfg.Kind,
			Address:   e.cfg.Address,
			Namespace: e.cfg.Namespace,
			Caps:      Capabilities(e.target),
		}
		if err := e.target.Ready(ctx); err != nil {
			s.Error = err.Error()
		} else {
			s.Ready = true
		}
		out = append(out, s)
	}
	return out
}

func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var firstErr error
	for _, e := range r.entries {
		if err := e.target.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
