// Package down drives engine stores: it triggers docker / containerd daemons to
// pull a reference and reports their per-layer progress through a Sink. A new
// engine kind is one file plus a factory case; phase-2 features (verify, GC) are
// optional capability interfaces discovered by type assertion.
package down

import (
	"context"
	"fmt"

	"github.com/lesomnus/gantry/cmd/config"
)

// LayerUpdate is one progress report for a single blob the engine is pulling.
// Engines report their own layer view (the daemon's layer IDs), not the source
// registry digests.
type LayerUpdate struct {
	Digest string
	Total  int64
	Done   int64
	State  string // pulling | done | exists
}

// Sink receives per-layer progress for one pull.
type Sink interface {
	Layer(u LayerUpdate)
}

// Engine is a daemon gantry triggers to pull from a registry.
type Engine interface {
	Name() string
	Kind() string // docker | containerd
	Ready(ctx context.Context) error
	Pull(ctx context.Context, ref string, sink Sink) error
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

// Caps describes which capabilities an engine implements.
type Caps struct {
	Pull   bool `json:"pull"`
	Verify bool `json:"verify"`
	GC     bool `json:"gc"`
}

// Capabilities discovers an engine's optional capabilities by type assertion.
func Capabilities(e Engine) Caps {
	_, verify := e.(Verifier)
	_, gc := e.(Collector)
	return Caps{Pull: true, Verify: verify, GC: gc}
}

// New builds an Engine for a docker/containerd store config.
func New(c config.StoreConfig) (Engine, error) {
	switch c.Kind {
	case "docker":
		return newDockerEngine(c)
	case "containerd":
		return newContainerdEngine(c)
	default:
		return nil, fmt.Errorf("store %q is not an engine kind (%q)", c.Name, c.Kind)
	}
}
