// Package down drives engine stores: it triggers docker / containerd daemons to
// pull a reference and reports their per-layer progress through a Sink. A new
// engine kind is one file plus a factory case; phase-2 features (verify, GC) are
// optional capability interfaces discovered by type assertion.
package down

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
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

// UsageSink receives "ref was used at t" reports from an engine's usage watcher.
type UsageSink func(ref string, at time.Time)

// RemoveResult reports what an image removal did. A tag removal may only Untag
// (disk is freed only when the last referencing tag goes / its content GCs).
type RemoveResult struct {
	Untagged []string `json:"untagged,omitempty"` // tag refs removed; disk freed only when the last tag/content GCs
	Deleted  []string `json:"deleted,omitempty"`  // content IDs whose bytes were actually deleted
}

// Engine is a daemon gantry triggers to pull from — and, for retention, observes
// usage on and deletes images from.
type Engine interface {
	Name() string
	Kind() string // docker | containerd
	Ready(ctx context.Context) error
	// Pull makes the engine pull ref. A non-empty digest anchors the pull: the
	// engine pulls repo@digest and then tags it as ref locally, so a mutable tag
	// re-resolved by a pull-through cache cannot substitute different bytes.
	Pull(ctx context.Context, ref string, digest string, sink Sink) error

	// InUse returns the references and image IDs currently held by live containers.
	InUse(ctx context.Context) (map[string]bool, error)
	// SeedUsage reports existing containers' images to bootstrap the index at startup.
	SeedUsage(ctx context.Context, sink UsageSink) error
	// WatchUsage streams "image used" events until ctx is done or the stream ends
	// (the caller reconnects on a non-nil return).
	WatchUsage(ctx context.Context, sink UsageSink) error
	// Remove deletes one image by reference.
	Remove(ctx context.Context, ref string) (RemoveResult, error)

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

// anchoredRef rewrites a tag reference to pull by digest; a reference that is
// already digest-anchored is returned unchanged.
func anchoredRef(ref, digest string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse ref %q: %w", ref, err)
	}
	if _, ok := r.(name.Digest); ok {
		return ref, nil
	}
	return r.Context().Name() + "@" + digest, nil
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
