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

// AnchorBlob is the raw manifest/index the digest-named `as` references point
// at: the exact bytes whose sha256 is the pull's anchor digest, fetched from
// the job's source (the cache) — never the origin registry. Engines that
// register digest-named references out-of-band (docker with the containerd
// image store) need the bytes; engines that only need the descriptor
// (containerd) use the digest/size/media type.
type AnchorBlob struct {
	MediaType string
	Digest    string // "sha256:...", equals the pull's anchor digest
	Bytes     []byte // raw manifest/index; sha256(Bytes) == Digest
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
	// platform ("os/arch", OCI form) selects the platform to pull; empty means
	// the daemon's default. The value is passed through as-is — if the image has
	// no such platform, the daemon's error is returned.
	// as may mix tag and digest references; a digest name must carry the anchor
	// digest and is registered out-of-band over the pulled content (anchor
	// supplies the manifest bytes backing it). Digest names require an anchored
	// pull — the Copier admits them only with a non-empty digest.
	// recorded reports the references the daemon actually holds for the image
	// after the pull — the applied names, or the pull-created record when a
	// name could not be applied (classic-store digest skip) — so the caller
	// stamps its retention index with reality, never with a skipped name.
	Pull(ctx context.Context, ref string, digest string, platform string, as []string, anchor *AnchorBlob, sink Sink) (recorded []string, err error)
	// Platform reports the daemon host's platform in OCI form ("linux/amd64").
	Platform(ctx context.Context) (string, error)

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

// Inventory is a snapshot of an engine's image store, taken by the Reconciler
// capability: every tag reference present on the daemon, plus the images that
// have no tag at all.
type Inventory struct {
	Refs     []string        // every tag reference on the daemon
	Untagged []UntaggedImage // images with no tag reference
}

// UntaggedImage is one image that lost (or never had) every tag.
type UntaggedImage struct {
	ID          string   `json:"id"`                     // content-addressed image ID, "sha256:..."
	RepoDigests []string `json:"repo_digests,omitempty"` // repo@digest references still naming the image
}

// Reconciler is the inventory-reconciliation capability (optional): a snapshot
// of the daemon's image store, so retention can seed references it has never
// observed and reap images that have lost every tag. containerd does not
// implement it — gantry drops the pull-created digest record after retagging
// (digest names the caller did NOT ask for would root content forever; the
// digest-named `as` references it DID ask for are stamped into the retention
// index instead), so containerd's own GC reclaims replaced content.
type Reconciler interface {
	// Images snapshots the daemon's image store.
	Images(ctx context.Context) (Inventory, error)
	// ReapUntagged deletes one untagged image by ID, re-checking the daemon
	// immediately before removing. owned (nil = own nothing) is consulted with
	// each of the image's repo@digest references: any owned reference vetoes
	// the reap — the caller's index claimed it between the plan and this apply
	// (e.g. a digest-`as` job that just named the content). ok=false without an
	// error means the image is not reapable right now (re-tagged since the
	// scan, referenced by a container, owned, or one of its references is being
	// pulled) and the caller should retry next pass; an image already gone
	// reports ok=true.
	ReapUntagged(ctx context.Context, id string, owned func(repoDigest string) bool) (rr RemoveResult, ok bool, err error)
}

// StartEvent is a container start/restart observation. Unlike UsageSink (which
// carries only the image ref), it carries the container ID that runtime
// enforcement needs to remove the container.
type StartEvent struct {
	ContainerID string
	Image       string // the image reference the event/container reports
	At          time.Time
}

// ContainerImage is what a running container's image resolves to, for a verdict
// lookup. A container references a platform-specific manifest, but a signature is
// over the top-level index; RepoDigests carry the repo@digest (top-level)
// spellings, so they are the preferred key. ManifestDigest is the
// platform-specific manifest digest, kept for cross-check / fallback.
type ContainerImage struct {
	ConfigImage    string   // the image reference the container was created from
	ImageID        string   // the local image content id ("sha256:...")
	RepoDigests    []string // repo@digest references (top-level digest; preferred key)
	ManifestDigest string   // platform-specific manifest digest (cross-check / fallback)
}

// Enforcer is the optional runtime signature-enforcement capability: observe
// container starts, resolve a container's image to its content digest, and remove
// a container. docker-only in v1 (containerd's kill mechanic differs), following
// the same docker-only precedent as Reconciler.
type Enforcer interface {
	// WatchStarts streams container start/restart events until ctx is done or the
	// stream ends (the caller reconnects on a non-nil return).
	WatchStarts(ctx context.Context, sink func(StartEvent)) error
	// ListRunning returns the currently-running containers, so a cold reconcile on
	// (re)connect catches starts missed during a disconnect gap.
	ListRunning(ctx context.Context) ([]StartEvent, error)
	// ResolveImage resolves a container's image to the identifiers a verdict is
	// keyed by. It does not error when the image is gone: it returns what it has.
	ResolveImage(ctx context.Context, containerID string) (ContainerImage, error)
	// RemoveContainer removes a container; force stops-and-removes a running one
	// (docker rm -f). A container already gone is treated as success.
	RemoveContainer(ctx context.Context, containerID string, force bool) error
}

// Caps describes which capabilities an engine implements.
type Caps struct {
	Pull      bool `json:"pull"`
	Verify    bool `json:"verify"`
	GC        bool `json:"gc"`
	Reconcile bool `json:"reconcile"`
	Enforce   bool `json:"enforce"`
}

// Capabilities discovers an engine's optional capabilities by type assertion.
func Capabilities(e Engine) Caps {
	_, verify := e.(Verifier)
	_, gc := e.(Collector)
	_, recon := e.(Reconciler)
	_, enforce := e.(Enforcer)
	return Caps{Pull: true, Verify: verify, GC: gc, Reconcile: recon, Enforce: enforce}
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

// splitNames separates `as` names into tag and digest references. The Copier
// validated each at admission, so an unparseable name is a tag (verbatim
// passthrough keeps whatever the daemon accepted before).
func splitNames(as []string) (tags, digests []string) {
	for _, n := range as {
		if r, err := name.ParseReference(n); err == nil {
			if _, ok := r.(name.Digest); ok {
				digests = append(digests, n)
				continue
			}
		}
		tags = append(tags, n)
	}
	return tags, digests
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
