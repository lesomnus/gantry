package rpc

import (
	"context"
	"errors"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type storeService struct {
	pb.UnimplementedStoreServiceServer
	s *Server
}

type nopSink struct{}

func (nopSink) Layer(down.LayerUpdate) {}

func errStoresAreConfig() error {
	return status.Error(codes.Unimplemented, "stores are declared in gantry.yaml; the API cannot create, modify, or delete them")
}

func (v *storeService) Add(context.Context, *pb.StoreAddRequest) (*pb.Store, error) {
	return nil, errStoresAreConfig()
}

func (v *storeService) Patch(context.Context, *pb.StorePatchRequest) (*pb.Store, error) {
	return nil, errStoresAreConfig()
}

func (v *storeService) Erase(context.Context, *pb.StoreRef) (*emptypb.Empty, error) {
	return nil, errStoresAreConfig()
}

// storeName pulls the name out of a StoreRef.
func storeName(ref *pb.StoreRef) (string, error) {
	if name := ref.GetName(); name != "" {
		return name, nil
	}
	return "", status.Error(codes.InvalidArgument, "store name is required")
}

// engine resolves a StoreRef to an engine store, mirroring the HTTP layer's
// 404 on both unknown names and non-engine stores.
func (v *storeService) engine(ref *pb.StoreRef) (string, down.Engine, error) {
	name, err := storeName(ref)
	if err != nil {
		return "", nil, err
	}
	eng, err := v.s.stores.Engine(name)
	if err != nil {
		return "", nil, status.Error(codes.NotFound, err.Error())
	}
	return name, eng, nil
}

// gcUnit gates the GC RPCs: the store must be an engine store (NotFound
// otherwise) and retention must be configured (FailedPrecondition otherwise).
// Order matters and mirrors the HTTP gcReady helper.
func (v *storeService) gcUnit(ref *pb.StoreRef) (string, error) {
	name, _, err := v.engine(ref)
	if err != nil {
		return "", err
	}
	if v.s.gc == nil {
		return "", status.Errorf(codes.FailedPrecondition, "retention/gc is not enabled (configure stores.%s.retention for an engine store)", name)
	}
	return name, nil
}

func (v *storeService) status(ctx context.Context, name string) (store.Status, bool) {
	cfg, ok := v.s.stores.Config(name)
	if !ok {
		return store.Status{}, false
	}
	st := store.Status{Name: name, Kind: cfg.Kind}
	if cfg.IsRegistry() {
		st.Host = cfg.Host
		st.Mode = cfg.Mode
		st.Ready = true
		st.Capabilities = store.Caps{Read: true, Write: true}
		return st, true
	}
	st.Address = cfg.Address
	st.Namespace = cfg.Namespace
	eng, err := v.s.stores.Engine(name)
	if err != nil {
		st.Error = err.Error()
		return st, true
	}
	caps := down.Capabilities(eng)
	st.Capabilities = store.Caps{Pull: caps.Pull, Verify: caps.Verify, GC: caps.GC, Reconcile: caps.Reconcile}
	if err := eng.Ready(ctx); err != nil {
		st.Error = err.Error()
	} else {
		st.Ready = true
	}
	return st, true
}

func (v *storeService) Get(ctx context.Context, req *pb.StoreGetRequest) (*pb.Store, error) {
	name, err := storeName(req.GetRef())
	if err != nil {
		return nil, err
	}
	st, ok := v.status(ctx, name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unknown store %q", name)
	}
	return statusToPB(st), nil
}

func (v *storeService) List(ctx context.Context, req *pb.StoreListRequest) (*pb.StoreListResponse, error) {
	all := v.s.stores.StoreStatuses(ctx)
	if req.HasKind() {
		kind, ok := storeKindFromPB[req.GetKind()]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "unknown store kind")
		}
		kept := all[:0]
		for _, st := range all {
			if st.Kind == kind {
				kept = append(kept, st)
			}
		}
		all = kept
	}
	paged, next, err := page(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	items := make([]*pb.Store, 0, len(paged))
	for _, st := range paged {
		items = append(items, statusToPB(st))
	}
	b := pb.StoreListResponse_builder{Items: items}
	if next != "" {
		b.NextPageToken = proto.String(next)
	}
	return b.Build(), nil
}

func (v *storeService) Pull(ctx context.Context, req *pb.StorePullRequest) (*pb.StorePullResponse, error) {
	name, eng, err := v.engine(req.GetStore())
	if err != nil {
		return nil, err
	}
	ref := req.GetRef()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	if _, err := eng.Pull(ctx, ref, "", req.GetPlatform(), nil, nil, nopSink{}); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if v.s.gc != nil {
		// Stamp the retention index so the manual pull is age-GC eligible.
		v.s.gc.Distributed(name, ref, time.Now())
	}
	v.s.rec.ImagePulled(name, ref)
	return pb.StorePullResponse_builder{Ref: proto.String(ref)}.Build(), nil
}

func (v *storeService) Remove(ctx context.Context, req *pb.StoreRemoveRequest) (*pb.StoreRemoveResponse, error) {
	name, eng, err := v.engine(req.GetStore())
	if err != nil {
		return nil, err
	}
	ref := req.GetRef()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	res, err := eng.Remove(ctx, ref)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if v.s.gc != nil {
		// Best effort: keep the retention index in sync with the engine.
		_, _ = v.s.gc.DeleteRecord(name, ref)
	}
	v.s.rec.ImageRemoved(name, ref)
	return pb.StoreRemoveResponse_builder{
		Untagged: res.Untagged,
		Deleted:  res.Deleted,
	}.Build(), nil
}

func (v *storeService) Health(ctx context.Context, ref *pb.StoreRef) (*pb.StoreHealthResponse, error) {
	name, err := storeName(ref)
	if err != nil {
		return nil, err
	}
	rep, err := v.s.health.Check(ctx, name)
	if err != nil {
		if errors.Is(err, health.ErrUnknownStore) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	// An unhealthy store is a report, not an RPC failure.
	return reportToPB(rep), nil
}

func (v *storeService) GcStatus(ctx context.Context, ref *pb.StoreRef) (*pb.StoreGcStatusResponse, error) {
	name, err := v.gcUnit(ref)
	if err != nil {
		return nil, err
	}
	u, ok := v.s.gc.Status().Stores[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "store %q has no retention", name)
	}
	enabled := false
	if d, err := time.ParseDuration(u.Schedule.Interval); err == nil && d > 0 {
		enabled = true
	}
	rules := make([]*pb.GcRule, 0, len(u.Rules))
	for _, r := range u.Rules {
		rb := pb.GcRule_builder{
			MaxAge: durStr(r.MaxAge),
			Pins:   r.Pins,
		}
		if r.Repo != "" {
			rb.Repo = proto.String(r.Repo)
		}
		if r.KeepN != nil {
			rb.KeepN = proto.Int32(int32(*r.KeepN))
		}
		if r.MaxN != nil {
			rb.MaxN = proto.Int32(int32(*r.MaxN))
		}
		rules = append(rules, rb.Build())
	}
	b := pb.StoreGcStatusResponse_builder{
		Enabled:    proto.Bool(enabled),
		Running:    proto.Bool(u.Running),
		Started:    ts(u.Started),
		LastRun:    ts(u.LastRun),
		NextWake:   ts(u.NextWake),
		GraceUntil: ts(u.GraceUntil),
		Schedule: pb.GcSchedule_builder{
			Interval:    durStr(u.Schedule.Interval),
			MinInterval: durStr(u.Schedule.MinInterval),
			Grace:       durStr(u.Schedule.Grace),
		}.Build(),
		Rules:         rules,
		Records:       proto.Int32(int32(u.Records)),
		Pins:          proto.Int32(int32(u.Pins)),
		Untagged:      proto.Int32(int32(u.Untagged)),
		UntaggedAfter: durStr(u.UntaggedAfter),
	}
	if ws, ok := v.s.gc.Watcher(name); ok {
		b.Watcher = watcherToPB(ws)
	}
	return b.Build(), nil
}

// gcOverride builds the one-shot policy override, mirroring the HTTP
// handler's validation.
func gcOverride(o *pb.GcOverride) (*retention.Policy, error) {
	if o == nil {
		return nil, nil
	}
	p := retention.Policy{Pins: o.GetPins()}
	if o.HasMaxAge() {
		p.MaxAge = o.GetMaxAge().AsDuration()
	}
	if o.HasKeepN() {
		p.KeepN = int(o.GetKeepN())
	}
	if o.HasMaxN() {
		p.MaxN = int(o.GetMaxN())
	}
	if o.HasUntaggedAfter() {
		p.UntaggedAfter = o.GetUntaggedAfter().AsDuration()
	}
	for _, pin := range p.Pins {
		if !doublestar.ValidatePattern(pin) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid pin pattern %q", pin)
		}
	}
	if p.MaxN < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_n must not be negative")
	}
	if p.MaxN > 0 && p.KeepN > p.MaxN {
		return nil, status.Error(codes.InvalidArgument, "max_n must be >= keep_n")
	}
	if p.UntaggedAfter < 0 {
		return nil, status.Error(codes.InvalidArgument, "untagged_after must not be negative")
	}
	return &p, nil
}

func (v *storeService) gcPlan(ctx context.Context, req *pb.StoreGcRequest) (string, retention.Decision, error) {
	name, err := v.gcUnit(req.GetStore())
	if err != nil {
		return "", retention.Decision{}, err
	}
	override, err := gcOverride(req.GetOverride())
	if err != nil {
		return "", retention.Decision{}, err
	}
	dec, err := v.s.gc.Plan(ctx, name, override)
	if err != nil {
		return "", retention.Decision{}, status.Error(codes.Unavailable, err.Error())
	}
	return name, dec, nil
}

func (v *storeService) GcPlan(ctx context.Context, req *pb.StoreGcRequest) (*pb.StoreGcPlanResponse, error) {
	_, dec, err := v.gcPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	return decisionToPB(dec), nil
}

func (v *storeService) GcApply(ctx context.Context, req *pb.StoreGcRequest) (*pb.StoreGcApplyResponse, error) {
	name, dec, err := v.gcPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := v.s.gc.Apply(ctx, name, dec)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return applyResultToPB(res), nil
}
