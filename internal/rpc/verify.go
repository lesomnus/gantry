package rpc

import (
	"context"
	"errors"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type verifyService struct {
	pb.UnimplementedVerifyServiceServer
	s *Server
}

func (v *verifyService) gate() error {
	if v.s.verify == nil {
		return status.Error(codes.FailedPrecondition, "verification is not enabled (set serve.verify.mode)")
	}
	return nil
}

// srcRef resolves the request's source store and the reference on it, the
// same way job admission does.
func (v *verifyService) srcRef(ref, source string) (config.StoreConfig, name.Reference, error) {
	base, err := name.ParseReference(ref)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	sourceKey := source
	if sourceKey == "" {
		sourceKey = base.Context().RegistryStr()
	}
	sourceStore, err := v.s.stores.Registry(sourceKey)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	id := ":" + base.Identifier()
	if d, ok := base.(name.Digest); ok {
		id = "@" + d.DigestStr()
	}
	src, err := name.ParseReference(sourceStore.Host + "/" + base.Context().RepositoryStr() + id)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	return sourceStore, src, nil
}

// Describe reports the effective policy. With verification disabled it still
// answers (enabled=false), like the REST endpoint.
func (v *verifyService) Describe(ctx context.Context, _ *emptypb.Empty) (*pb.VerifyDescribeResponse, error) {
	if v.s.verify == nil {
		return describeToPB(verify.Description{Enabled: false, Mode: string(config.VerifyOff)}, nil), nil
	}
	d := v.s.verify.Describe()
	stores := map[string]pb.VerifyMode{}
	for _, n := range v.s.stores.Names() {
		c, ok := v.s.stores.Config(n)
		if !ok || !c.IsRegistry() {
			continue
		}
		mode := d.Mode
		if c.Verify != nil && c.Verify.Mode != "" {
			mode = string(c.Verify.Mode)
		}
		if mode == "" {
			mode = string(config.VerifyOff)
		}
		stores[n] = verifyModeToPB[mode]
	}
	return describeToPB(d, stores), nil
}

func (v *verifyService) Check(ctx context.Context, req *pb.VerifyCheckRequest) (*pb.VerifyCheckResponse, error) {
	if err := v.gate(); err != nil {
		return nil, err
	}
	if req.GetRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	source, src, err := v.srcRef(req.GetRef(), req.GetSource().GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	res, err := v.s.verify.Verify(ctx, source, src)
	switch {
	case err == nil:
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, verify.ErrNotFound):
		return nil, status.Error(codes.NotFound, err.Error())
	default:
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	b := pb.VerifyCheckResponse_builder{
		Verified: proto.Bool(res.Verified()),
		Ref:      proto.String(src.Name()),
	}
	if m, ok := verifyModeToPB[string(res.Mode)]; ok {
		b.Mode = &m
	}
	if res.Verified() {
		b.Digest = proto.String(res.Digest.String())
	}
	return b.Build(), nil
}

// Reload re-reads the trust store; on failure the previous verifier stays in
// effect. The resulting policy is observable via Describe.
func (v *verifyService) Reload(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if err := v.gate(); err != nil {
		return nil, err
	}
	if _, err := v.s.verify.Reload(); err != nil {
		if errors.Is(err, verify.ErrBadTrustMaterial) {
			return nil, status.Errorf(codes.FailedPrecondition, "reload rejected: %s", err)
		}
		return nil, status.Errorf(codes.Internal, "reload failed: %s", err)
	}
	return &emptypb.Empty{}, nil
}
