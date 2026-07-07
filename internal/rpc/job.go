package rpc

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type jobService struct {
	pb.UnimplementedJobServiceServer
	s *Server
}

// watchTick paces Watch snapshots, mirroring the SSE endpoint.
const watchTick = 250 * time.Millisecond

func jobID(ref *pb.JobRef) (string, error) {
	if id := ref.GetId(); id != "" {
		return id, nil
	}
	return "", status.Error(codes.InvalidArgument, "job id is required")
}

// submitErr maps Submit/Retry errors the way the HTTP layer does.
func submitErr(err error) error {
	switch {
	case errors.Is(err, warm.ErrQueueFull):
		return status.Error(codes.ResourceExhausted, "job queue is full")
	case errors.Is(err, warm.ErrJobNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, warm.ErrJobActive):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.InvalidArgument, err.Error())
	}
}

// setCoalesced reports whether the submit joined an existing job, in a
// response trailer since Add returns the bare Job.
func setCoalesced(ctx context.Context, coalesced bool) {
	_ = grpc.SetTrailer(ctx, metadata.Pairs("gantry-coalesced", strconv.FormatBool(coalesced)))
}

func idemKey(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get("idempotency-key"); len(v) > 0 {
		return v[0]
	}
	return ""
}

func requestFromPB(ref string, from, to *pb.StoreRef, platforms, as []string) warm.Request {
	return warm.Request{
		Ref:       ref,
		Platforms: platforms,
		From:      from.GetName(),
		To:        to.GetName(),
		As:        as,
	}
}

func (v *jobService) Add(ctx context.Context, req *pb.JobAddRequest) (*pb.Job, error) {
	if req.GetRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	r := requestFromPB(req.GetRef(), req.GetFrom(), req.GetTo(), req.GetPlatforms(), req.GetAs())
	if req.HasCopyReferrers() {
		cr := req.GetCopyReferrers()
		r.CopyReferrers = &cr
	}

	// An idempotency key replays the remembered job instead of re-running the
	// move; the key alone wins, like the HTTP Idempotency-Key header.
	key := idemKey(ctx)
	if key != "" {
		if snap, ok := v.s.jobs.Idem(key); ok {
			setCoalesced(ctx, true)
			return jobToPB(snap), nil
		}
	}
	snap, created, err := v.s.warmer.Submit(r)
	if err != nil {
		return nil, submitErr(err)
	}
	if key != "" {
		v.s.jobs.Remember(key, snap.ID)
	}
	setCoalesced(ctx, !created)
	return jobToPB(snap), nil
}

func (v *jobService) Get(ctx context.Context, req *pb.JobGetRequest) (*pb.Job, error) {
	id, err := jobID(req.GetRef())
	if err != nil {
		return nil, err
	}
	snap, ok := v.s.jobs.Snapshot(id)
	if !ok {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	return jobToPB(snap), nil
}

// Erase evicts the job record; evicting a running job also cancels it.
func (v *jobService) Erase(ctx context.Context, ref *pb.JobRef) (*emptypb.Empty, error) {
	id, err := jobID(ref)
	if err != nil {
		return nil, err
	}
	if !v.s.jobs.Delete(id) {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	return &emptypb.Empty{}, nil
}

func (v *jobService) List(ctx context.Context, req *pb.JobListRequest) (*pb.JobListResponse, error) {
	f := warm.Filter{}
	if req.HasState() {
		st, ok := jobStateFromPB[req.GetState()]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "unknown state")
		}
		f.State = st
	}
	f.Ref = req.GetRef()
	if req.HasSince() {
		f.Since = req.GetSince().AsTime()
	}
	paged, next, err := page(v.s.jobs.List(f), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	items := make([]*pb.Job, 0, len(paged))
	for _, snap := range paged {
		items = append(items, jobToPB(snap))
	}
	b := pb.JobListResponse_builder{Items: items}
	if next != "" {
		b.NextPageToken = proto.String(next)
	}
	return b.Build(), nil
}

func (v *jobService) Watch(ref *pb.JobRef, stream grpc.ServerStreamingServer[pb.Job]) error {
	id, err := jobID(ref)
	if err != nil {
		return err
	}
	if _, ok := v.s.jobs.Snapshot(id); !ok {
		return status.Error(codes.NotFound, "job not found")
	}
	ctx := stream.Context()
	tick := time.NewTicker(watchTick)
	defer tick.Stop()
	for {
		snap, ok := v.s.jobs.Snapshot(id)
		if !ok {
			// Evicted mid-stream; end like the SSE endpoint does.
			return nil
		}
		if err := stream.Send(jobToPB(snap)); err != nil {
			return err
		}
		if snap.State.Terminal() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (v *jobService) Plan(ctx context.Context, req *pb.JobPlanRequest) (*pb.JobPlanResponse, error) {
	if req.GetRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	r := requestFromPB(req.GetRef(), req.GetFrom(), req.GetTo(), req.GetPlatforms(), req.GetAs())
	if req.HasCopyReferrers() {
		cr := req.GetCopyReferrers()
		r.CopyReferrers = &cr
	}
	res, err := v.s.warmer.Plan(ctx, r)
	if err != nil {
		if errors.Is(err, verify.ErrUnsigned) || errors.Is(err, verify.ErrUntrusted) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return planToPB(res), nil
}

func (v *jobService) Cancel(ctx context.Context, ref *pb.JobRef) (*pb.Job, error) {
	id, err := jobID(ref)
	if err != nil {
		return nil, err
	}
	job, ok := v.s.jobs.Job(id)
	if !ok {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	if snap, _ := v.s.jobs.Snapshot(id); snap.State.Terminal() {
		return nil, status.Errorf(codes.FailedPrecondition, "job is already %s", snap.State)
	}
	// Outside the store lock: the cancel func is set once at submit time.
	job.Cancel()
	snap, _ := v.s.jobs.Snapshot(id)
	return jobToPB(snap), nil
}

func (v *jobService) Retry(ctx context.Context, ref *pb.JobRef) (*pb.Job, error) {
	id, err := jobID(ref)
	if err != nil {
		return nil, err
	}
	snap, created, err := v.s.warmer.Retry(id)
	if err != nil {
		return nil, submitErr(err)
	}
	setCoalesced(ctx, !created)
	return jobToPB(snap), nil
}
