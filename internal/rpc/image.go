package rpc

import (
	"bytes"
	"context"

	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type imageService struct {
	pb.UnimplementedImageServiceServer
	s *Server
}

func (v *imageService) gcUnit(ref *pb.StoreRef) (string, error) {
	return (&storeService{s: v.s}).gcUnit(ref)
}

// resolve turns an ImageRef into (store, ref). The surrogate uuid id is
// deterministic over (store, ref), so an id lookup scans the engine stores'
// records for the match.
func (v *imageService) resolve(ref *pb.ImageRef) (string, string, error) {
	if loc := ref.GetLocator(); loc != nil {
		name, err := v.gcUnit(loc.GetStore())
		if err != nil {
			return "", "", err
		}
		if loc.GetRef() == "" {
			return "", "", status.Error(codes.InvalidArgument, "ref is required")
		}
		return name, loc.GetRef(), nil
	}
	if id := ref.GetId(); len(id) > 0 {
		if v.s.gc == nil {
			return "", "", status.Error(codes.FailedPrecondition, "retention/gc is not enabled")
		}
		for _, name := range v.s.retentionStores() {
			recs, err := v.s.gc.List(name)
			if err != nil {
				return "", "", status.Error(codes.Internal, err.Error())
			}
			for _, rec := range recs {
				if bytes.Equal(imageID(name, rec.Ref), id) {
					return name, rec.Ref, nil
				}
			}
		}
		return "", "", status.Error(codes.NotFound, "no index record for the id")
	}
	return "", "", status.Error(codes.InvalidArgument, "image ref is required")
}

func (v *imageService) find(storeName, ref string) (retention.Record, error) {
	recs, err := v.s.gc.List(storeName)
	if err != nil {
		return retention.Record{}, status.Error(codes.Internal, err.Error())
	}
	for _, rec := range recs {
		if rec.Ref == ref {
			return rec, nil
		}
	}
	return retention.Record{}, status.Errorf(codes.NotFound, "no index record for %s", ref)
}

// inUseSet fetches the live in-use set from the engine daemon.
func (v *imageService) inUseSet(ctx context.Context, storeName string) (map[string]bool, error) {
	eng, err := v.s.stores.Engine(storeName)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	m, err := eng.InUse(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return m, nil
}

func (v *imageService) Get(ctx context.Context, req *pb.ImageGetRequest) (*pb.Image, error) {
	storeName, ref, err := v.resolve(req.GetRef())
	if err != nil {
		return nil, err
	}
	rec, err := v.find(storeName, ref)
	if err != nil {
		return nil, err
	}
	inUse, err := v.inUseSet(ctx, storeName)
	if err != nil {
		return nil, err
	}
	return recordToPB(storeName, rec, inUse[rec.Ref]), nil
}

func (v *imageService) List(ctx context.Context, req *pb.ImageListRequest) (*pb.ImageListResponse, error) {
	name, err := v.gcUnit(req.GetStore())
	if err != nil {
		return nil, err
	}
	recs, err := v.s.gc.List(name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// The live set both answers the in_use filter and fills the derived
	// in_use field, so it is fetched unconditionally.
	inUse, err := v.inUseSet(ctx, name)
	if err != nil {
		return nil, err
	}

	filtered := recs[:0]
	for _, rec := range recs {
		if req.HasRepo() && rec.Repo != req.GetRepo() {
			continue
		}
		if req.HasRef() && rec.Ref != req.GetRef() {
			continue
		}
		if req.HasPinned() && rec.Pinned != req.GetPinned() {
			continue
		}
		if req.HasInUse() && inUse[rec.Ref] != req.GetInUse() {
			continue
		}
		filtered = append(filtered, rec)
	}
	paged, next, err := page(filtered, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	items := make([]*pb.Image, 0, len(paged))
	for _, rec := range paged {
		items = append(items, recordToPB(name, rec, inUse[rec.Ref]))
	}
	b := pb.ImageListResponse_builder{Items: items}
	if next != "" {
		b.NextPageToken = proto.String(next)
	}
	// The untagged reap clocks ride along only on unfiltered lists,
	// mirroring the REST endpoint (the entries are IDs, not refs).
	if !req.HasRepo() && !req.HasRef() && !req.HasPinned() && !req.HasInUse() {
		unt, err := v.s.gc.ListUntagged(name)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		items := make([]*pb.UntaggedImage, 0, len(unt))
		for _, u := range unt {
			ub := pb.UntaggedImage_builder{DateFirstSeen: ts(u.FirstSeen)}
			if u.ID != "" {
				ub.Id = proto.String(u.ID)
			}
			items = append(items, ub.Build())
		}
		b.Untagged = items
	}
	return b.Build(), nil
}

// Erase purges the index record (or an untagged reap clock when the ref is an
// image ID); it never touches the engine.
func (v *imageService) Erase(ctx context.Context, ref *pb.ImageRef) (*emptypb.Empty, error) {
	storeName, imgRef, err := v.resolve(ref)
	if err != nil {
		return nil, err
	}
	existed, err := v.s.gc.DeleteRecord(storeName, imgRef)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !existed {
		return nil, status.Errorf(codes.NotFound, "no index record for %s", imgRef)
	}
	return &emptypb.Empty{}, nil
}

var _ = timestamppb.Timestamp{}
