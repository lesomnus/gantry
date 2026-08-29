// Package store resolves the image stores a job references and reports them on
// GET /v1/store. A store is a registry (gantry reads/writes blobs) or an engine
// daemon (gantry triggers a pull). It builds the engine clients and exposes the
// registry configs the copy engine needs.
package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
		Name: ref,
		Kind: "oci",
		Host: ref,
		Mode: "copy",
	}, nil
}

// Engine resolves a reference to a declared engine store.
//
// The reference is the store's name, or a SELECTOR naming the daemon to reach:
//
//	docker:192.168.10.34        kind and host
//	192.168.10.34               host alone
//	192.168.10.34:2376          host and port, when two daemons share a host
//
// A selector resolves to a store that is ALREADY DECLARED and whose address
// points at that daemon. It is a second way to say the same store, never a way
// to reach an undeclared one -- so it carries no credentials of its own and
// adds no retention index. That is the whole reason it is a lookup and not a
// constructor: an engine store owns a client certificate, a CA to verify the
// daemon, and a GC index over the images on it. A second store standing on the
// same daemon would be a second GC scheduler that cannot see the first one's
// usage records, and the two would disagree about what is safe to delete.
//
// # Why a caller would want this
//
// Because the name is the caller's problem. A store's name is chosen here, and
// whoever asks for it has to learn it and keep agreeing with it -- through a
// node label, an operator's config, a convention two repositories share. The
// address is not chosen: it is where the daemon is, and the caller already had
// to know it to place work there at all. `Registry` has resolved by host for
// this reason since it was written; this is the same courtesy for engines.
//
// An ambiguous selector is an error rather than a pick. Two stores on one
// daemon is a configuration somebody meant something by, and guessing which was
// intended would be a warm that silently lands in the wrong place.
func (s *Set) Engine(ref string) (down.Engine, error) {
	if e, ok := s.engines[ref]; ok {
		return e, nil
	}
	if c, declared := s.byName[ref]; declared {
		return nil, fmt.Errorf("store %q is a %s, not an engine", ref, c.Kind)
	}

	kind, host := parseEngineSelector(ref)
	if host != "" {
		var hits []string
		for _, name := range s.order {
			c := s.byName[name]
			if !c.IsEngine() {
				continue
			}
			if kind != "" && c.Kind != kind {
				continue
			}
			if engineHostMatches(c.Address, host) {
				hits = append(hits, name)
			}
		}
		switch len(hits) {
		case 1:
			return s.engines[hits[0]], nil
		case 0:
			// fall through to the not-found error below
		default:
			return nil, fmt.Errorf("engine selector %q matches %d stores (%s); name one of them", ref, len(hits), strings.Join(hits, ", "))
		}
	}

	var declaredEngines []string
	for _, name := range s.order {
		if s.byName[name].IsEngine() {
			declaredEngines = append(declaredEngines, name)
		}
	}
	if len(declaredEngines) == 0 {
		return nil, fmt.Errorf("unknown engine store %q (no engine stores are declared)", ref)
	}
	return nil, fmt.Errorf("unknown engine store %q (declared: %s)", ref, strings.Join(declaredEngines, ", "))
}

// parseEngineSelector splits "docker:host", "host:port" or "host" into an
// optional kind and a host, which may itself carry a port.
//
// The ambiguity is real -- "192.168.10.34:2376" and "docker:192.168.10.34" have
// the same shape -- and it is settled by only ever reading a KNOWN kind before
// the colon. Anything else is a host, so a host that happens to be named like a
// kind is the one thing this cannot express; naming the store is the answer
// there, and it is the answer the caller had already.
func parseEngineSelector(ref string) (kind, host string) {
	if ref == "" {
		return "", ""
	}
	if k, rest, ok := strings.Cut(ref, ":"); ok && isEngineKind(k) {
		return k, rest
	}
	return "", ref
}

func isEngineKind(s string) bool { return s == "docker" || s == "containerd" }

// engineHostMatches reports whether a store's configured address points at the
// daemon a selector names.
//
// A selector with no port matches on host alone, because the port is part of
// how the store was configured rather than part of which machine it is -- the
// caller naming a node knows its address and usually not the port somebody
// chose for the daemon. A selector WITH a port must match both, which is how
// two daemons on one host stay tellable apart.
func engineHostMatches(addr, want string) bool {
	h, p := splitEngineAddr(addr)
	if h == "" {
		return false // a unix socket or a named pipe names no host
	}
	wh, wp := splitEngineAddr(want)
	if wh == "" {
		return false
	}
	if wh != h {
		return false
	}
	return wp == "" || wp == p
}

// splitEngineAddr reduces "tcp://host:port", "host:port" or "host" to its host
// and port. A unix socket or npipe address has no host and answers "".
func splitEngineAddr(addr string) (host, port string) {
	if addr == "" {
		return "", ""
	}
	if scheme, rest, ok := strings.Cut(addr, "://"); ok {
		if scheme != "tcp" && scheme != "http" && scheme != "https" {
			return "", ""
		}
		addr = rest
	} else if strings.HasPrefix(addr, "/") {
		return "", "" // a bare filesystem path is a socket
	}
	addr = strings.TrimSuffix(addr, "/")
	if h, p, ok := strings.Cut(addr, ":"); ok {
		return h, p
	}
	return addr, ""
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
