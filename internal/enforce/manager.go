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
	policy    string
	self      selfGuard
	now       func() time.Time
	wg        sync.WaitGroup
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
	for _, s := range allStores {
		if s.IsRegistry() && s.Host != "" {
			byHost[s.Host] = s
		}
	}
	m := &Manager{
		cache:     cache,
		verifier:  verifier,
		ociByHost: byHost,
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

// Stop waits for the watcher goroutines to exit. Call after cancelling the
// context passed to StartWatchers.
func (m *Manager) Stop() { m.wg.Wait() }

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

// imageToRemove prefers the digest reference (exact content) and falls back to
// the event image ref.
func imageToRemove(ev down.StartEvent, dec decision) string {
	if dec.digest != "" && ev.Image != "" {
		// name@digest targets the exact content regardless of tag movement.
		if i := indexOfTagSep(ev.Image); i >= 0 {
			return ev.Image[:i] + "@" + dec.digest
		}
		return ev.Image + "@" + dec.digest
	}
	return ev.Image
}

// indexOfTagSep returns the index of the tag separator ':' in a ref (after the
// last '/'), or -1 if none — so a registry host:port is not mistaken for a tag.
func indexOfTagSep(ref string) int {
	slash := -1
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			slash = i
		}
	}
	for i := len(ref) - 1; i > slash; i-- {
		if ref[i] == ':' {
			return i
		}
		if ref[i] == '@' {
			return -1
		}
	}
	return -1
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
