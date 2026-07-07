package rpc

import (
	"context"
	"strconv"

	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type eventService struct {
	pb.UnimplementedEventServiceServer
	s *Server
}

func (v *eventService) gate() error {
	if v.s.events == nil {
		return status.Error(codes.FailedPrecondition, "the audit log is not enabled (set serve.events.path)")
	}
	return nil
}

func (v *eventService) Get(ctx context.Context, req *pb.EventGetRequest) (*pb.Event, error) {
	if err := v.gate(); err != nil {
		return nil, err
	}
	seq := req.GetRef().GetSeq()
	if seq == 0 {
		return nil, status.Error(codes.InvalidArgument, "seq is required")
	}
	e, ok, err := v.s.events.Get(seq)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Error(codes.NotFound, "no such event")
	}
	return eventToPB(e), nil
}

func (v *eventService) List(ctx context.Context, req *pb.EventListRequest) (*pb.EventListResponse, error) {
	if err := v.gate(); err != nil {
		return nil, err
	}
	f := event.Filter{
		Store: req.GetStore(),
		Ref:   req.GetRef(),
	}
	if req.HasType() {
		t, ok := eventTypeFromPB[req.GetType()]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "unknown event type")
		}
		f.Type = t
	}
	if req.HasState() {
		st, ok := jobStateFromPB[req.GetState()]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "unknown state")
		}
		f.State = string(st)
	}
	if req.HasSince() {
		f.Since = req.GetSince().AsTime()
	}
	size := req.GetPageSize()
	if size < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	// A resumed walk keeps a page size even when the client dropped it, so
	// the token cannot run past the fetch window below.
	if size == 0 && req.GetPageToken() != "" {
		size = 100
	}
	// The log lists newest-first with an internal hard cap of 1000; fetch up
	// to the page's end so the offset token stays within one query. page()
	// re-validates the token; this pre-parse only sizes the fetch.
	off := 0
	if tok := req.GetPageToken(); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil && n >= 0 {
			off = n
		}
	}
	if size > 0 {
		f.Limit = off + int(size) + 1
	}
	events, err := v.s.events.List(f)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	paged, next, err := page(events, size, req.GetPageToken())
	if err != nil {
		return nil, err
	}
	items := make([]*pb.Event, 0, len(paged))
	for _, e := range paged {
		items = append(items, eventToPB(e))
	}
	b := pb.EventListResponse_builder{Items: items}
	if next != "" {
		b.NextPageToken = proto.String(next)
	}
	return b.Build(), nil
}
