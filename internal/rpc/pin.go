package rpc

import (
	"bytes"
	"context"
	"strconv"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type pinService struct {
	pb.UnimplementedPinServiceServer
	s *Server
}

func (v *pinService) gcUnit(ref *pb.StoreRef) (string, error) {
	return (&storeService{s: v.s}).gcUnit(ref)
}

// resolve turns a PinRef into (store, value). Pin identity is (store, value);
// the surrogate uuid id is deterministic over it.
func (v *pinService) resolve(ref *pb.PinRef) (string, string, error) {
	if by := ref.GetValue(); by != nil {
		name, err := v.gcUnit(by.GetStore())
		if err != nil {
			return "", "", err
		}
		if by.GetValue() == "" {
			return "", "", status.Error(codes.InvalidArgument, "value is required")
		}
		return name, by.GetValue(), nil
	}
	if id := ref.GetId(); len(id) > 0 {
		if v.s.gc == nil {
			return "", "", status.Error(codes.FailedPrecondition, "retention/gc is not enabled")
		}
		for _, name := range v.s.retentionStores() {
			pins, err := v.s.gc.Pins(name)
			if err != nil {
				return "", "", status.Error(codes.Internal, err.Error())
			}
			for _, e := range pins {
				if bytes.Equal(pinID(name, e.Value), id) {
					return name, e.Value, nil
				}
			}
		}
		return "", "", status.Error(codes.NotFound, "no pin for the id")
	}
	return "", "", status.Error(codes.InvalidArgument, "pin ref is required")
}

func (v *pinService) find(storeName, value string) (retention.PinEntry, error) {
	pins, err := v.s.gc.Pins(storeName)
	if err != nil {
		return retention.PinEntry{}, status.Error(codes.Internal, err.Error())
	}
	for _, e := range pins {
		if e.Value == value {
			return e, nil
		}
	}
	return retention.PinEntry{}, status.Errorf(codes.NotFound, "no pin %q", value)
}

// Add creates or refreshes a pin: pinning an existing value is an upsert that
// refreshes date_pinned (and may flip pattern-ness), like the REST endpoint.
func (v *pinService) Add(ctx context.Context, req *pb.PinAddRequest) (*pb.Pin, error) {
	name, err := v.gcUnit(req.GetStore())
	if err != nil {
		return nil, err
	}
	value := req.GetValue()
	if value == "" {
		return nil, status.Error(codes.InvalidArgument, "value is required")
	}
	// Fail closed: a malformed pattern would never match and silently let
	// the image GC.
	if req.GetPattern() && !doublestar.ValidatePattern(value) {
		return nil, status.Error(codes.InvalidArgument, "invalid doublestar pattern")
	}
	if err := v.s.gc.Pin(name, value, req.GetPattern()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Echo the pin's current blast radius as response trailers (non-blocking):
	// how many index records it protects, and the refs (capped). Lets a caller
	// notice a careless broad pattern that would neutralize GC.
	if matched, err := v.s.gc.PinMatches(name, value, req.GetPattern()); err == nil {
		md := metadata.Pairs("gantry-pin-matched-count", strconv.Itoa(len(matched)))
		const cap = 50
		if len(matched) > cap {
			matched = matched[:cap]
		}
		if len(matched) > 0 {
			md["gantry-pin-matched"] = matched
		}
		_ = grpc.SetTrailer(ctx, md)
	}
	e, err := v.find(name, value)
	if err != nil {
		return nil, err
	}
	return pinToPB(name, e), nil
}

func (v *pinService) Get(ctx context.Context, req *pb.PinGetRequest) (*pb.Pin, error) {
	name, value, err := v.resolve(req.GetRef())
	if err != nil {
		return nil, err
	}
	e, err := v.find(name, value)
	if err != nil {
		return nil, err
	}
	return pinToPB(name, e), nil
}

// Erase unpins by value; like the REST endpoint it is idempotent — erasing a
// pin that does not exist succeeds.
func (v *pinService) Erase(ctx context.Context, ref *pb.PinRef) (*emptypb.Empty, error) {
	name, value, err := v.resolve(ref)
	if err != nil {
		return nil, err
	}
	if err := v.s.gc.Unpin(name, value); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (v *pinService) List(ctx context.Context, req *pb.PinListRequest) (*pb.PinListResponse, error) {
	name, err := v.gcUnit(req.GetStore())
	if err != nil {
		return nil, err
	}
	pins, err := v.s.gc.Pins(name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	paged, next, err := page(pins, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	items := make([]*pb.Pin, 0, len(paged))
	for _, e := range paged {
		items = append(items, pinToPB(name, e))
	}
	b := pb.PinListResponse_builder{Items: items}
	if next != "" {
		b.NextPageToken = proto.String(next)
	}
	return b.Build(), nil
}
