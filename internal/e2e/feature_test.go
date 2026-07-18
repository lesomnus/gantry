package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Feature 7 (hermetic): Plan resolves the stores and the rewritten target ref
// without moving anything.
func TestPlanResolves(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")

	res, err := h.client.Job().Plan(context.Background(), pb.JobPlanRequest_builder{
		Ref: proto.String("lib/app:1"), Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
	}.Build())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if res.GetSource() != "remote" || res.GetTarget() != "cache" {
		t.Errorf("resolved source/target = %q/%q, want remote/cache", res.GetSource(), res.GetTarget())
	}
	if res.GetTargetRef() == "" {
		t.Error("plan did not resolve a target ref")
	}
	if hasTag(t, h.cache, "lib/app", "1") {
		t.Error("plan wrote to the cache; it must be a dry run")
	}
}

// Feature 10: an Idempotency-Key replays the remembered job rather than starting
// a second move, and flags the replay with the coalesced trailer.
func TestIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")

	ctx := metadata.AppendToOutgoingContext(context.Background(), "idempotency-key", "k1")
	j1, err := h.client.Job().Add(ctx, copyReq("remote", "cache"))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	var tr metadata.MD
	j2, err := h.client.Job().Add(ctx, copyReq("remote", "cache"), grpc.Trailer(&tr))
	if err != nil {
		t.Fatalf("replay add: %v", err)
	}
	if j1.GetId() != j2.GetId() {
		t.Errorf("idempotency key produced different jobs: %s vs %s", j1.GetId(), j2.GetId())
	}
	if got := tr.Get("gantry-coalesced"); len(got) != 1 || got[0] != "true" {
		t.Errorf("coalesced trailer = %v, want [true]", got)
	}
	h.waitDone(j1.GetId())
}

// Feature 12: the audit log durably records the job lifecycle and is queryable
// through EventService.
func TestAuditLog(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	h.waitDone(h.add(copyReq("remote", "cache")).GetId())

	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := h.client.Event().List(context.Background(), &pb.EventListRequest{})
		if err != nil {
			t.Fatalf("event list: %v", err)
		}
		var admitted, done bool
		for _, e := range res.GetItems() {
			switch e.GetType() {
			case pb.EventType_EVENT_TYPE_JOB_ADMITTED:
				admitted = true
			case pb.EventType_EVENT_TYPE_JOB_DONE:
				done = true
			}
		}
		if admitted && done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit log missing events: admitted=%v done=%v (%d events)", admitted, done, len(res.GetItems()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Feature 13: per-store health and the standard readiness service.
func TestHealth(t *testing.T) {
	h := newHarness(t)

	rep, err := h.client.Store().Health(context.Background(), pb.StoreByName("cache"))
	if err != nil {
		t.Fatalf("store health: %v", err)
	}
	if !rep.GetHealthy() {
		t.Errorf("cache reported unhealthy: %s", rep.GetError())
	}

	hc := grpc_health_v1.NewHealthClient(h.conn)
	if _, err := hc.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check: %v", err)
	}
}
