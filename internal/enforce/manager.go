// Package enforce implements runtime signature enforcement ("quarantine"):
// gantry watches each configured docker engine's container-start stream and
// force-removes any container whose image is not signed by a trusted Root CA,
// then removes the image. It is post-hoc quarantine (the container is already
// running when the start event fires), not admission control.
//
// A verdict is keyed on the image's top-level content digest and resolved in
// precedence: the durable verdict cache (offline), a live verification (which
// also consults the local signature layout), then the on_unavailable policy with
// grace honoring a known-but-expired trusted verdict. gantry never removes its
// own container (a self-identity interlock, not an image-name allowlist).
package enforce

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/otx/log"
)

// Engine is the engine surface enforcement needs: the Enforcer capability plus
// image removal (from the base down.Engine) and Name for logging. *dockerEngine
// satisfies it.
type Engine interface {
	down.Enforcer
	Remove(ctx context.Context, ref string) (down.RemoveResult, error)
	Name() string
}

// Store binds an engine store name to its enforcement-capable engine.
type Store struct {
	Name   string
	Engine Engine
}

// Options configures a Manager.
type Options struct {
	// OnUnavailable is grace (default) | kill | allow.
	OnUnavailable string
	// SelfContainer is gantry's own container id/name (else hostname/cgroup).
	SelfContainer string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Manager runs one watcher goroutine per store, deciding and quarantining as
// container-start events arrive.
type Manager struct {
	units     []*unit
	cache     *verify.Cache
	verifier  verify.Service
	ociByHost map[string]config.StoreConfig
	// cachedBy maps a store name to the registries that declare it as their
	// cache. A routed image is recorded on the node under the cache's host, so
	// this is how enforcement gets back to the registry the job was actually for.
	cachedBy map[string][]config.StoreConfig
	policy   string
	self     selfGuard
	now      func() time.Time
	wg       sync.WaitGroup
	cancel   context.CancelFunc
}

type unit struct {
	m    *Manager
	name string
	eng  Engine
}

// NewManager builds the enforcement manager. stores are the engine stores to
// police; cache and verifier are shared with the copy path (the verifier is the
// caching decorator); allStores is the full store map, used to map a running
// image's RepoDigest host back to the OCI store that holds its signature.
func NewManager(stores []Store, cache *verify.Cache, verifier verify.Service, allStores map[string]config.StoreConfig, opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	policy := opts.OnUnavailable
	if policy == "" {
		policy = "grace"
	}
	byHost := map[string]config.StoreConfig{}
	cachedBy := map[string][]config.StoreConfig{}
	for _, s := range allStores {
		if !s.IsRegistry() {
			continue
		}
		if s.Host != "" {
			byHost[s.Host] = s
		}
		for _, r := range s.Caches {
			cachedBy[r.Store] = append(cachedBy[r.Store], s)
		}
	}
	// Map iteration is unordered and this decides which store is asked second, so
	// fix it: an image killed or spared must not depend on map layout.
	for _, origins := range cachedBy {
		slices.SortFunc(origins, func(a, b config.StoreConfig) int { return strings.Compare(a.Name, b.Name) })
	}
	m := &Manager{
		cache:     cache,
		verifier:  verifier,
		ociByHost: byHost,
		cachedBy:  cachedBy,
		policy:    policy,
		self:      selfGuard{id: resolveSelfID(opts.SelfContainer)},
		now:       now,
	}
	for _, s := range stores {
		m.units = append(m.units, &unit{m: m, name: s.Name, eng: s.Engine})
	}
	return m
}

// StartWatchers launches one joined watcher goroutine per store. Watchers run
// until ctx is cancelled; Stop then joins them so kills cease promptly on
// shutdown (unlike retention's fire-and-forget watchers).
func (m *Manager) StartWatchers(ctx context.Context) {
	// Own a cancellable child so Stop() can terminate the watchers itself and does
	// not deadlock if the caller invokes it before cancelling the parent context.
	ctx, m.cancel = context.WithCancel(ctx)
	l := log.From(ctx)
	if m.self.id == "" {
		l.Warn("enforcement could not identify gantry's own container; self-protection is off — sign gantry's image into the trust layout or set serve.enforce.self_container")
	} else {
		l.Info("runtime enforcement started", slog.String("self_container", m.self.id), slog.String("on_unavailable", m.policy))
	}
	for _, u := range m.units {
		m.wg.Add(1)
		go func(u *unit) {
			defer m.wg.Done()
			u.watch(ctx)
		}(u)
	}
}

// Stop cancels the watchers and waits for them to exit. It is self-sufficient:
// it does not require the caller to have cancelled the context passed to
// StartWatchers first, so it can never deadlock shutdown.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// watch mirrors retention's reconnecting watcher: cold-reconcile the running
// containers on (re)connect (catching starts missed during a disconnect), then
// stream start events; on the stream ending, back off 2s and reconnect.
func (u *unit) watch(ctx context.Context) {
	l := log.From(ctx)
	reconcile := func() {
		running, err := u.eng.ListRunning(ctx)
		if err != nil {
			l.Debug("enforce reconcile: list running failed", slog.String("engine", u.name), slog.Any("err", err))
			return
		}
		for _, ev := range running {
			u.m.handle(ctx, u.eng, ev)
		}
	}
	l.Info("enforcement watcher started", slog.String("engine", u.name))
	reconcile()
	for ctx.Err() == nil {
		err := u.eng.WatchStarts(ctx, func(ev down.StartEvent) { u.m.handle(ctx, u.eng, ev) })
		if ctx.Err() != nil {
			return
		}
		l.Debug("enforcement watcher reconnecting", slog.String("engine", u.name), slog.Any("err", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		reconcile() // re-scan to cover the disconnect gap
	}
}

// handle decides and acts on one start event. It is idempotent: a replayed event
// for an already-removed container simply no-ops on removal.
func (m *Manager) handle(ctx context.Context, eng Engine, ev down.StartEvent) {
	if m.self.isSelf(ev.ContainerID) {
		return
	}
	dec := m.decide(ctx, eng, ev)
	l := log.From(ctx)
	if dec.action == actAllow {
		l.Debug("enforcement allow",
			slog.String("engine", eng.Name()), slog.String("container", short(ev.ContainerID)),
			slog.String("image", ev.Image), slog.String("reason", dec.reason))
		return
	}
	m.quarantine(ctx, eng, ev, dec)
}

// quarantine force-removes the container, then best-effort removes its image. The
// container removal is the enforcement action; image removal is cleanup (a
// shared image conflict is a benign skip).
func (m *Manager) quarantine(ctx context.Context, eng Engine, ev down.StartEvent, dec decision) {
	l := log.From(ctx)
	l.Warn("quarantining untrusted container",
		slog.String("engine", eng.Name()), slog.String("container", short(ev.ContainerID)),
		slog.String("image", ev.Image), slog.String("digest", dec.digest), slog.String("reason", dec.reason))

	if err := eng.RemoveContainer(ctx, ev.ContainerID, true); err != nil {
		l.Error("quarantine: failed to remove container",
			slog.String("container", short(ev.ContainerID)), slog.Any("err", err))
		return
	}
	// Remove the image so the container cannot simply restart from it. Best
	// effort: a still-referenced image errors (conflict) and is left in place.
	if ref := imageToRemove(ev, dec); ref != "" {
		if _, err := eng.Remove(ctx, ref); err != nil {
			l.Debug("quarantine: image not removed (in use or gone)",
				slog.String("image", ref), slog.Any("err", err))
		}
	}
}

// imageToRemove builds the reference to remove: the repository of the event
// image pinned to the resolved content digest (targeting exact content), or the
// event image verbatim when there is no digest or no repository to pin.
func imageToRemove(ev down.StartEvent, dec decision) string {
	if dec.digest != "" {
		if repo := repoName(ev.Image); repo != "" {
			return repo + "@" + dec.digest
		}
	}
	return ev.Image
}

// repoName returns the repository portion of a docker image reference (host/path),
// stripping any existing tag or @digest. It returns "" for a bare image id
// ("<alg>:<hex>", which has no repository) so the caller does not build a
// malformed double-digest reference.
func repoName(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i] // strip an existing digest
	}
	slash := strings.LastIndexByte(ref, '/')
	if slash < 0 {
		// No path component: either "name:tag" or a bare image id "alg:hex".
		if looksLikeDigest(ref) {
			return ""
		}
		if i := strings.IndexByte(ref, ':'); i >= 0 {
			return ref[:i] // name:tag -> name
		}
		return ref // bare repository name
	}
	if i := strings.LastIndexByte(ref, ':'); i > slash {
		return ref[:i] // strip a tag (a ':' after the last '/')
	}
	return ref
}

// looksLikeDigest reports whether s is an "<algo>:<hex>" content id (no path).
func looksLikeDigest(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return false
	}
	hex := s[i+1:]
	if len(hex) < 32 {
		return false
	}
	for _, r := range hex {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
