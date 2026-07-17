// Package health probes whether each configured store is reachable and caches
// the result for a short TTL. An engine store is probed with its daemon
// ready-check; a registry store is probed with a GET /v2/ ping (a 200 or a 401
// auth challenge both mean "reachable"). Concurrent callers for the same store
// coalesce onto a single probe (singleflight), and every result is cached for
// CacheTTL so repeated polling does not hammer the backends.
package health

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ErrUnknownStore is returned by Check when the store name is not configured.
var ErrUnknownStore = errors.New("unknown store")

// Report is one store's health at DateChecked.
type Report struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Healthy     bool      `json:"healthy"`
	Error       string    `json:"error,omitempty"`
	LatencyMS   int64     `json:"latency_ms"`
	DateChecked time.Time `json:"date_checked"`
	// Cached is true when this report was served from the TTL cache rather than
	// probed for this request.
	Cached bool `json:"cached"`
}

// Options tunes a Checker.
type Options struct {
	CacheTTL     time.Duration // how long a probe result is reused; <=0 -> 5s
	ProbeTimeout time.Duration // per-probe deadline; <=0 -> 3s
	// ReadyStores are the store names readiness gates on; empty means every
	// engine store (a remote upstream must not flap the node's readiness).
	ReadyStores []string
}

// Checker probes and caches store health.
type Checker struct {
	stores *store.Set
	ttl    time.Duration
	probeT time.Duration
	ready  []string

	now   func() time.Time
	probe func(ctx context.Context, c config.StoreConfig) error // overridable in tests

	// probeDur is created lazily from the first Check's context, since the
	// Checker is built before the otel-carrying serve context exists.
	minit    sync.Once
	probeDur metric.Float64Histogram

	mu      sync.Mutex
	entries map[string]*entry
}

// entry holds a store's last result. Its mutex serializes probes for that store
// so concurrent callers coalesce onto one probe instead of stampeding.
type entry struct {
	mu     sync.Mutex
	report Report
	valid  bool
	expiry time.Time
}

// NewChecker builds a Checker over the configured stores.
func NewChecker(stores *store.Set, opts Options) *Checker {
	ck := &Checker{
		stores:  stores,
		ttl:     opts.CacheTTL,
		probeT:  opts.ProbeTimeout,
		ready:   opts.ReadyStores,
		now:     time.Now,
		entries: make(map[string]*entry),
	}
	if ck.ttl <= 0 {
		ck.ttl = 5 * time.Second
	}
	if ck.probeT <= 0 {
		ck.probeT = 3 * time.Second
	}
	ck.probe = ck.probeStore
	return ck
}

// ReadyStores returns the configured readiness gate; empty means the caller
// should fall back to the engine stores.
func (ck *Checker) ReadyStores() []string { return ck.ready }

// Check returns the store's health, probing it unless a cached result is still
// fresh. A probe failure is reported in Report.Healthy/Error, not as an error;
// only an unknown store name returns a non-nil error (ErrUnknownStore).
func (ck *Checker) Check(ctx context.Context, name string) (Report, error) {
	c, ok := ck.stores.Config(name)
	if !ok {
		return Report{}, ErrUnknownStore
	}

	ck.mu.Lock()
	e := ck.entries[name]
	if e == nil {
		e = &entry{}
		ck.entries[name] = e
	}
	ck.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.valid && ck.now().Before(e.expiry) {
		rep := e.report
		rep.Cached = true
		return rep, nil
	}

	rep := ck.runProbe(ctx, c)
	e.report = rep
	e.valid = true
	e.expiry = ck.now().Add(ck.ttl)
	return rep, nil
}

// runProbe probes one store under a bounded, request-independent deadline and
// builds its Report. The probe context is detached from the caller's ctx (only
// its values are kept) so one caller disconnecting cannot abort a probe that
// other callers are waiting on and poison the shared cache; the probe is bounded
// by ProbeTimeout instead.
func (ck *Checker) runProbe(ctx context.Context, c config.StoreConfig) Report {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ck.probeT)
	defer cancel()

	start := ck.now()
	err := ck.probe(pctx, c)
	dur := ck.now().Sub(start)
	rep := Report{
		Name:        c.Name,
		Kind:        c.Kind,
		Healthy:     err == nil,
		LatencyMS:   dur.Milliseconds(),
		DateChecked: start,
	}
	if err != nil {
		rep.Error = err.Error()
	}
	ck.minit.Do(func() {
		ck.probeDur, _ = otx.Meter(ctx).Float64Histogram("gantry.health.probe.duration",
			metric.WithUnit("s"), metric.WithDescription("store health probe duration"))
	})
	ck.probeDur.Record(ctx, dur.Seconds(), metric.WithAttributes(
		attribute.String("store", c.Name),
		attribute.String("kind", c.Kind),
		attribute.Bool("healthy", rep.Healthy),
	))
	return rep
}

// probeStore dispatches to the engine ready-check or the registry ping.
func (ck *Checker) probeStore(ctx context.Context, c config.StoreConfig) error {
	if c.IsEngine() {
		eng, err := ck.stores.Engine(c.Name)
		if err != nil {
			return err
		}
		return eng.Ready(ctx)
	}
	return ck.pingRegistry(ctx, c)
}

// pingRegistry does a GET /v2/ against the registry. A 200 or a 401 auth
// challenge both count as reachable; any other status or a transport error is
// unhealthy. Bearer auth is intentionally not attempted — reachability is the
// signal — but the store's real transport (TPM mTLS, private CA, or self-signed
// skip) IS used, or an mTLS/private-CA registry would be probed with the wrong
// transport and reported unhealthy while being perfectly reachable.
func (ck *Checker) pingRegistry(ctx context.Context, c config.StoreConfig) error {
	var nopts []name.Option
	if c.Insecure {
		nopts = append(nopts, name.Insecure) // lets Ping fall back to plain HTTP
	}
	reg, err := name.NewRegistry(c.Host, nopts...)
	if err != nil {
		return err
	}
	// Shared with the copy/verify paths (pooled, reused). nil = the library
	// default transport for a plain secure registry.
	rt, err := xport.Transport(c)
	if err != nil {
		return err
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	_, err = transport.Ping(ctx, reg, rt)
	return err
}
