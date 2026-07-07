// Package rpc serves the gantry API over gRPC — the counterpart of
// internal/server (HTTP) over the same dependencies. Write RPCs that have no
// domain operation (stores are declared in configuration, image records and
// audit events are written internally) answer codes.Unimplemented.
package rpc

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	healthsvc "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Warmer is the subset of *warm.Warmer the services call.
type Warmer interface {
	Submit(req warm.Request) (warm.JobSnapshot, bool, error)
	Retry(id string) (warm.JobSnapshot, bool, error)
	Plan(ctx context.Context, req warm.Request) (warm.PlanResult, error)
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
}

// Server implements pb.Server plus the VerifyService over the same
// dependencies internal/server takes.
type Server struct {
	warmer Warmer
	jobs   warm.Store
	stores *store.Set
	gc     GC             // nil when retention/GC is disabled
	health Health
	verify verify.Service // nil when verification is disabled
	events *event.Log     // nil when the audit log is disabled
	rec    *event.Recorder
}

func New(warmer Warmer, jobs warm.Store, stores *store.Set, gc GC, hc Health, vf verify.Service, ev *event.Log) *Server {
	// A typed-nil pointer inside an interface would bypass the disabled guards.
	if isNil(gc) {
		gc = nil
	}
	if isNil(vf) {
		vf = nil
	}
	return &Server{
		warmer: warmer,
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
// grpc.health.v1.Health service (SERVING once the server is up — the gRPC
// counterpart of /healthz).
func (s *Server) Register(g *grpc.Server) {
	pb.RegisterServer(g, s)
	pb.RegisterVerifyServiceServer(g, s.VerifyService())
	healthpb.RegisterHealthServer(g, healthsvc.NewServer())
}
