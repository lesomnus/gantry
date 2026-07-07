package rpc_test

import (
	"context"
	"fmt"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestVerifyDisabled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := pb.NewVerifyServiceClient(e.conn)

	// Describe still answers when verification is off, like GET /v1/verify.
	d, err := c.Describe(ctx, &emptypb.Empty{})
	if err != nil || d.GetEnabled() {
		t.Fatalf("describe: %v %v", d, err)
	}

	_, err = c.Check(ctx, pb.VerifyCheckRequest_builder{Ref: proto.String("x:1")}.Build())
	wantCode(t, err, codes.FailedPrecondition)
	_, err = c.Reload(ctx, &emptypb.Empty{})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestVerify(t *testing.T) {
	e := newEnv(t, withVerify())
	ctx := context.Background()
	c := pb.NewVerifyServiceClient(e.conn)
	e.verify.desc = verify.Description{
		Enabled:  true,
		Provider: "notation",
		Mode:     "require",
		Anchors:  []verify.AnchorInfo{{Subject: "CN=fleet"}},
	}

	d, err := c.Describe(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if !d.GetEnabled() || d.GetMode() != pb.VerifyMode_VERIFY_MODE_REQUIRE || len(d.GetAnchors()) != 1 {
		t.Errorf("describe: %v", d)
	}
	// Per-store effective modes cover the registry stores.
	if d.GetStores()["src"] != pb.VerifyMode_VERIFY_MODE_REQUIRE {
		t.Errorf("stores map: %v", d.GetStores())
	}

	e.verify.res = verify.Result{
		Mode:   config.VerifyRequire,
		Digest: v1.Hash{Algorithm: "sha256", Hex: "0000000000000000000000000000000000000000000000000000000000000000"},
	}
	res, err := c.Check(ctx, pb.VerifyCheckRequest_builder{
		Ref: proto.String("src.local/lib/app:1"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetVerified() || res.GetDigest() == "" || res.GetRef() != "src.local/lib/app:1" {
		t.Errorf("check: %v", res)
	}

	e.verify.err = fmt.Errorf("%w", verify.ErrUnsigned)
	_, err = c.Check(ctx, pb.VerifyCheckRequest_builder{Ref: proto.String("src.local/lib/app:1")}.Build())
	wantCode(t, err, codes.FailedPrecondition)

	e.verify.err = fmt.Errorf("%w", verify.ErrNotFound)
	_, err = c.Check(ctx, pb.VerifyCheckRequest_builder{Ref: proto.String("src.local/lib/app:1")}.Build())
	wantCode(t, err, codes.NotFound)

	e.verify.reloadErr = fmt.Errorf("%w: bad pem", verify.ErrBadTrustMaterial)
	_, err = c.Reload(ctx, &emptypb.Empty{})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestVerifyCheckSrcRef(t *testing.T) {
	e := newEnv(t, withVerify())
	ctx := context.Background()
	c := pb.NewVerifyServiceClient(e.conn)
	e.verify.res = verify.Result{Mode: config.VerifyIfPresent}

	// An explicit from re-homes the reference onto the store's host, the
	// same way job admission resolves it.
	res, err := c.Check(ctx, pb.VerifyCheckRequest_builder{
		Ref:    proto.String("other.io/lib/app:1"),
		Source: pb.StoreByName("src"),
	}.Build())
	if err != nil || res.GetRef() != "src.local/lib/app:1" {
		t.Fatalf("re-homed ref: %v %v", res, err)
	}

	// Digest references keep the digest form.
	const dig = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	res, err = c.Check(ctx, pb.VerifyCheckRequest_builder{
		Ref:    proto.String("other.io/lib/app" + dig),
		Source: pb.StoreByName("src"),
	}.Build())
	if err != nil || res.GetRef() != "src.local/lib/app"+dig {
		t.Fatalf("digest ref: %v %v", res, err)
	}

	// An unknown source is invalid input, not NotFound — like REST's 400.
	_, err = c.Check(ctx, pb.VerifyCheckRequest_builder{
		Ref: proto.String("nowhere.io/lib/app:1"),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)
}

func TestTypedNilGuards(t *testing.T) {
	e := newEnv(t, withTypedNils())
	ctx := context.Background()

	_, err := e.client.Store().GcStatus(ctx, pb.StoreByName("node"))
	wantCode(t, err, codes.FailedPrecondition)
	_, err = e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	wantCode(t, err, codes.FailedPrecondition)

	// A typed-nil verify.Service must read as disabled, not panic.
	d, err := pb.NewVerifyServiceClient(e.conn).Describe(ctx, &emptypb.Empty{})
	if err != nil || d.GetEnabled() {
		t.Fatalf("describe with typed-nil verify: %v %v", d, err)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, withAuth(config.AuthConfig{Tokens: []string{"sekrit"}}))
	ctx := context.Background()

	_, err := e.client.Store().List(ctx, &pb.StoreListRequest{})
	wantCode(t, err, codes.Unauthenticated)

	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer sekrit")
	if _, err := e.client.Store().List(authed, &pb.StoreListRequest{}); err != nil {
		t.Fatal(err)
	}

	bad := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer wrong")
	_, err = e.client.Store().List(bad, &pb.StoreListRequest{})
	wantCode(t, err, codes.Unauthenticated)

	// The health service stays public.
	hres, err := healthpb.NewHealthClient(e.conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || hres.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", hres, err)
	}

	// Streams go through the stream interceptor.
	w, err := e.client.Job().Watch(ctx, pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Recv()
	wantCode(t, err, codes.Unauthenticated)

	w, err = e.client.Job().Watch(authed, pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Recv()
	wantCode(t, err, codes.NotFound) // authed; the job simply does not exist
}
