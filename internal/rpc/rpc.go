// Package rpc serves the gantry API over gRPC. Write RPCs that have no
// domain operation (stores are declared in configuration, image records and
// audit events are written internally) answer codes.Unimplemented.
package rpc

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/lesomnus/gantry/internal/cpx"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	healthsvc "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Copier is the subset of *cpx.Copier the services call.
type Copier interface {
	Submit(req cpx.Request) (cpx.JobSnapshot, bool, error)
	Retry(id string) (cpx.JobSnapshot, bool, error)
	Plan(ctx context.Context, req cpx.Request) (cpx.PlanResult, error)
}

// GC is the subset of *retention.Manager the services call.
type GC interface {
	Status() retention.Status
	Watcher(engine string) (retention.WatcherStatus, bool)
	List(engine string) ([]retention.Record, error)
	DeleteRecord(engine, ref string) (bool, error)
	ListUntagged(engine string) ([]retention.UntaggedEntry, error)
	Distributed(engine, ref string, t time.Time)
	Pin(engine, ref string, pattern bool) error
	Unpin(engine, ref string) error
	Pins(engine string) ([]retention.PinEntry, error)
	Plan(ctx context.Context, engine string, override *retention.Policy) (retention.Decision, error)
	Apply(ctx context.Context, engine string, dec retention.Decision) (retention.ApplyResult, error)
}

// Health is the subset of *health.Checker the services call.
type Health interface {
	Check(ctx context.Context, name string) (health.Report, error)
	ReadyStores() []string
}

// Server implements pb.Server plus the VerifyService.
type Server struct {
	copier Copier
	jobs   cpx.Store
	stores *store.Set
	gc     GC // nil when retention/GC is disabled
	health Health
	verify verify.Service // nil when verification is disabled
	events *event.Log     // nil when the audit log is disabled
	rec    *event.Recorder
}

func New(copier Copier, jobs cpx.Store, stores *store.Set, gc GC, hc Health, vf verify.Service, ev *event.Log) *Server {
	// A typed-nil pointer inside an interface would bypass the disabled guards.
	if isNil(gc) {
		gc = nil
	}
	if isNil(vf) {
		vf = nil
	}
	return &Server{
		copier: copier,
		jobs:   jobs,
		stores: stores,
		gc:     gc,
		health: hc,
		verify: vf,
		events: ev,
		rec:    event.NewRecorder(ev),
	}
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// retentionStores lists the retention-managed store names, sorted. Callers
// must hold a non-nil gc.
func (s *Server) retentionStores() []string {
	st := s.gc.Status()
	names := make([]string, 0, len(st.Stores))
	for name := range st.Stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var _ pb.Server = (*Server)(nil)

func (s *Server) Store() pb.StoreServiceServer { return &storeService{s: s} }
func (s *Server) Job() pb.JobServiceServer     { return &jobService{s: s} }
func (s *Server) Image() pb.ImageServiceServer { return &imageService{s: s} }
func (s *Server) Pin() pb.PinServiceServer     { return &pinService{s: s} }
func (s *Server) Event() pb.EventServiceServer { return &eventService{s: s} }

// VerifyService is not part of the orm-generated wiring; register it
// alongside pb.RegisterServer.
func (s *Server) VerifyService() pb.VerifyServiceServer { return &verifyService{s: s} }

// Register registers every gantry service plus the standard
// grpc.health.v1.Health service, returned so the caller can drive readiness
// (see WatchReadiness). The overall status starts SERVING.
func (s *Server) Register(g *grpc.Server) *healthsvc.Server {
	pb.RegisterServer(g, s)
	pb.RegisterVerifyServiceServer(g, s.VerifyService())
	hs := healthsvc.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	return hs
}

// WatchReadiness keeps the overall serving status in step with the gated
// stores (serve.health.ready_stores; every engine store when unset), probing
// them every interval until ctx is done. The health checker's TTL cache
// bounds the probe cost.
func (s *Server) WatchReadiness(ctx context.Context, hs *healthsvc.Server, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		st := healthpb.HealthCheckResponse_SERVING
		if !s.ready(ctx) {
			st = healthpb.HealthCheckResponse_NOT_SERVING
		}
		hs.SetServingStatus("", st)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ready probes the gated stores concurrently and reports whether all are
// healthy, like the old /readyz.
func (s *Server) ready(ctx context.Context) bool {
	gate := s.health.ReadyStores()
	if len(gate) == 0 {
		// A flaky remote upstream must not flap node readiness: only the
		// engine stores gate by default.
		gate = s.stores.EngineNames()
	}
	oks := make([]bool, len(gate))
	var wg sync.WaitGroup
	for i, name := range gate {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := s.health.Check(ctx, name)
			oks[i] = err == nil && rep.Healthy
		}()
	}
	wg.Wait()
	for _, ok := range oks {
		if !ok {
			return false
		}
	}
	return true
}
