package rpc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("want %v, got %v", code, err)
	}
}

func TestStoreGet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	st, err := e.client.Store().Get(ctx, pb.StoreGetByName("src"))
	if err != nil {
		t.Fatal(err)
	}
	if st.GetKind() != pb.StoreKind_STORE_KIND_OCI || !st.GetReady() || !st.GetCapabilities().GetRead() {
		t.Errorf("unexpected registry status: %v", st)
	}

	st, err = e.client.Store().Get(ctx, pb.StoreGetByName("node"))
	if err != nil {
		t.Fatal(err)
	}
	if st.GetKind() != pb.StoreKind_STORE_KIND_DOCKER || !st.GetReady() || !st.GetCapabilities().GetPull() {
		t.Errorf("unexpected engine status: %v", st)
	}

	e.eng.readyErr = errors.New("daemon down")
	st, err = e.client.Store().Get(ctx, pb.StoreGetByName("node"))
	if err != nil {
		t.Fatal(err)
	}
	if st.GetReady() || st.GetError() == "" {
		t.Errorf("engine should be unready: %v", st)
	}

	_, err = e.client.Store().Get(ctx, pb.StoreGetByName("nope"))
	wantCode(t, err, codes.NotFound)

	_, err = e.client.Store().Add(ctx, &pb.StoreAddRequest{})
	wantCode(t, err, codes.Unimplemented)
}

func TestStoreList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	res, err := e.client.Store().List(ctx, &pb.StoreListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetItems()) != 2 {
		t.Fatalf("want 2 stores, got %d", len(res.GetItems()))
	}

	res, err = e.client.Store().List(ctx, pb.StoreListRequest_builder{
		Kind: pb.StoreKind_STORE_KIND_DOCKER.Enum(),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetItems()) != 1 || res.GetItems()[0].GetName() != "node" {
		t.Fatalf("kind filter failed: %v", res.GetItems())
	}
}

func TestStorePull(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	_, err := e.client.Store().Pull(ctx, pb.StorePullRequest_builder{
		Store:    pb.StoreByName("node"),
		Ref:      proto.String("src.local/lib/app:1"),
		Platform: proto.String("linux/arm64"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	// Manual pulls are unanchored (digest "") and pass the platform through.
	want := pullCall{ref: "src.local/lib/app:1", digest: "", platform: "linux/arm64"}
	if len(e.eng.pulled) != 1 || e.eng.pulled[0] != want {
		t.Fatalf("pull not forwarded: %v", e.eng.pulled)
	}
	// The pull stamps the retention index and records an audit event.
	recs, err := e.gc.List("node")
	if err != nil || len(recs) != 1 || recs[0].LastDistributed.IsZero() {
		t.Errorf("retention stamp missing: %v %v", recs, err)
	}
	evs, err := e.events.List(event.Filter{Type: event.ImagePulled})
	if err != nil || len(evs) != 1 {
		t.Errorf("audit event missing: %v %v", evs, err)
	}

	_, err = e.client.Store().Pull(ctx, pb.StorePullRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	_, err = e.client.Store().Pull(ctx, pb.StorePullRequest_builder{
		Store: pb.StoreByName("src"), // registry, not an engine
		Ref:   proto.String("x:1"),
	}.Build())
	wantCode(t, err, codes.NotFound)

	e.eng.pullErr = errors.New("boom")
	_, err = e.client.Store().Pull(ctx, pb.StorePullRequest_builder{
		Store: pb.StoreByName("node"),
		Ref:   proto.String("x:1"),
	}.Build())
	wantCode(t, err, codes.Unavailable)
}

func TestStoreRemove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.ix.Seed("node", "src.local/lib/app:1", time.Now()); err != nil {
		t.Fatal(err)
	}
	e.eng.removeRes = down.RemoveResult{Untagged: []string{"src.local/lib/app:1"}, Deleted: []string{"sha256:aa"}}

	res, err := e.client.Store().Remove(ctx, pb.StoreRemoveRequest_builder{
		Store: pb.StoreByName("node"),
		Ref:   proto.String("src.local/lib/app:1"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetUntagged()) != 1 || len(res.GetDeleted()) != 1 {
		t.Fatalf("remove result not forwarded: %v", res)
	}
	// The index record is purged alongside.
	recs, _ := e.gc.List("node")
	if len(recs) != 0 {
		t.Errorf("index record not purged: %v", recs)
	}

	e.eng.removeErr = errors.New("daemon says no")
	_, err = e.client.Store().Remove(ctx, pb.StoreRemoveRequest_builder{
		Store: pb.StoreByName("node"),
		Ref:   proto.String("src.local/lib/app:1"),
	}.Build())
	wantCode(t, err, codes.Unavailable)
}

func TestStoreHealth(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	rep, err := e.client.Store().Health(ctx, pb.StoreByName("node"))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.GetHealthy() || rep.GetKind() != pb.StoreKind_STORE_KIND_DOCKER {
		t.Errorf("unexpected report: %v", rep)
	}

	_, err = e.client.Store().Health(ctx, pb.StoreByName("nope"))
	wantCode(t, err, codes.NotFound)
}

func TestStoreHealthUnhealthy(t *testing.T) {
	e := newEnv(t)
	e.eng.readyErr = errors.New("daemon down")

	// An unhealthy store is a report, not an RPC failure.
	rep, err := e.client.Store().Health(context.Background(), pb.StoreByName("node"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.GetHealthy() || rep.GetError() == "" {
		t.Errorf("want unhealthy report, got %v", rep)
	}
}

func TestStoreGc(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)
	if err := e.ix.Touch("node", "src.local/lib/app:1", old); err != nil {
		t.Fatal(err)
	}
	if err := e.ix.Touch("node", "src.local/lib/app:2", time.Now()); err != nil {
		t.Fatal(err)
	}

	st, err := e.client.Store().GcStatus(ctx, pb.StoreByName("node"))
	if err != nil {
		t.Fatal(err)
	}
	if st.GetEnabled() || st.GetRecords() != 2 {
		t.Errorf("unexpected gc status: %v", st)
	}

	// Without an override the store has no rules: everything is unmanaged.
	plan, err := e.client.Store().GcPlan(ctx, pb.StoreGcRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.GetDelete()) != 0 || len(plan.GetKeep()) != 2 {
		t.Fatalf("unexpected plan: %v", plan)
	}

	// A max_age override applies a blanket rule.
	req := pb.StoreGcRequest_builder{
		Store: pb.StoreByName("node"),
		Override: pb.GcOverride_builder{
			MaxAge: durationpb.New(time.Hour),
		}.Build(),
	}.Build()
	plan, err = e.client.Store().GcPlan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.GetDelete()) != 1 || plan.GetDelete()[0].GetReason() != pb.GcDeleteReason_GC_DELETE_REASON_AGE_EXCEEDED {
		t.Fatalf("unexpected plan: %v", plan)
	}

	res, err := e.client.Store().GcApply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.GetEvaluated() != 2 || len(e.eng.removed) != 1 {
		t.Fatalf("unexpected apply: %v removed=%v", res, e.eng.removed)
	}
	recs, _ := e.gc.List("node")
	if len(recs) != 1 {
		t.Errorf("record not collected: %v", recs)
	}

	// Invalid overrides.
	_, err = e.client.Store().GcPlan(ctx, pb.StoreGcRequest_builder{
		Store: pb.StoreByName("node"),
		Override: pb.GcOverride_builder{
			Pins: []string{"["},
		}.Build(),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	_, err = e.client.Store().GcPlan(ctx, pb.StoreGcRequest_builder{
		Store: pb.StoreByName("node"),
		Override: pb.GcOverride_builder{
			KeepN: proto.Int32(5),
			MaxN:  proto.Int32(2),
		}.Build(),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	// The fake engine has no reconcile capability, so an untagged_after
	// override is rejected by the manager (mapped like REST's 502).
	_, err = e.client.Store().GcPlan(ctx, pb.StoreGcRequest_builder{
		Store: pb.StoreByName("node"),
		Override: pb.GcOverride_builder{
			UntaggedAfter: durationpb.New(time.Hour),
		}.Build(),
	}.Build())
	wantCode(t, err, codes.Unavailable)
}

func TestStoreGcDisabled(t *testing.T) {
	e := newEnv(t, withoutGC())
	ctx := context.Background()

	_, err := e.client.Store().GcStatus(ctx, pb.StoreByName("node"))
	wantCode(t, err, codes.FailedPrecondition)

	// Unknown/non-engine stores are NotFound even when GC is disabled.
	_, err = e.client.Store().GcStatus(ctx, pb.StoreByName("src"))
	wantCode(t, err, codes.NotFound)
}
