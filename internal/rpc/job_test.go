package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/lesomnus/gantry/internal/cpx"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func snap(id, ref string, state cpx.JobState, at time.Time) cpx.JobSnapshot {
	return cpx.JobSnapshot{ID: id, Ref: ref, State: state, DateCreated: at}
}

func TestJobAdd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.copier.snap = snap("job_1", "src.local/lib/app:1", cpx.JobPending, time.Now())
	e.copier.created = true

	var trailer metadata.MD
	job, err := e.client.Job().Add(ctx, pb.JobAddRequest_builder{
		Ref:    "src.local/lib/app:1",
		Target: pb.StoreByName("node"),
		As:     []string{"docker.io/lib/app:1"},
		Labels: map[string]string{"team": "infra"},
	}.Build(), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatal(err)
	}
	if job.GetId() != "job_1" {
		t.Errorf("unexpected job: %v", job)
	}
	if got := trailer.Get("gantry-coalesced"); len(got) != 1 || got[0] != "false" {
		t.Errorf("want coalesced=false trailer, got %v", got)
	}
	if len(e.copier.submits) != 1 || e.copier.submits[0].Target != "node" ||
		len(e.copier.submits[0].As) != 1 || e.copier.submits[0].As[0] != "docker.io/lib/app:1" ||
		e.copier.submits[0].Labels["team"] != "infra" {
		t.Errorf("submit not forwarded: %+v", e.copier.submits)
	}

	// Coalesced submit reports through the trailer.
	e.copier.created = false
	_, err = e.client.Job().Add(ctx, pb.JobAddRequest_builder{
		Ref:    "src.local/lib/app:1",
		Target: pb.StoreByName("node"),
	}.Build(), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatal(err)
	}
	if got := trailer.Get("gantry-coalesced"); len(got) != 1 || got[0] != "true" {
		t.Errorf("want coalesced=true trailer, got %v", got)
	}

	_, err = e.client.Job().Add(ctx, &pb.JobAddRequest{})
	wantCode(t, err, codes.InvalidArgument)
}

func TestJobAddErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	req := pb.JobAddRequest_builder{Ref: "x:1"}.Build()

	e.copier.err = cpx.ErrQueueFull
	_, err := e.client.Job().Add(ctx, req)
	wantCode(t, err, codes.ResourceExhausted)

	e.copier.err = fmt.Errorf("admission: %w", verify.ErrUntrusted)
	_, err = e.client.Job().Add(ctx, req)
	wantCode(t, err, codes.FailedPrecondition)

	e.copier.err = errors.New(`unknown store "x"`)
	_, err = e.client.Job().Add(ctx, req)
	wantCode(t, err, codes.InvalidArgument)
}

func TestJobAddIdempotency(t *testing.T) {
	e := newEnv(t)
	e.addJob(t, "job_9", "x:1", cpx.JobRunning, time.Now())
	e.copier.snap = snap("job_9", "x:1", cpx.JobRunning, time.Now())
	e.copier.created = true

	ctx := metadata.AppendToOutgoingContext(context.Background(), "idempotency-key", "k1")
	req := pb.JobAddRequest_builder{Ref: "x:1"}.Build()

	if _, err := e.client.Job().Add(ctx, req); err != nil {
		t.Fatal(err)
	}
	var trailer metadata.MD
	job, err := e.client.Job().Add(ctx, req, grpc.Trailer(&trailer))
	if err != nil {
		t.Fatal(err)
	}
	if job.GetId() != "job_9" {
		t.Errorf("replay returned a different job: %v", job)
	}
	if got := trailer.Get("gantry-coalesced"); len(got) != 1 || got[0] != "true" {
		t.Errorf("replay must report coalesced, got %v", got)
	}
	if len(e.copier.submits) != 1 {
		t.Errorf("replay must not resubmit: %d submits", len(e.copier.submits))
	}
}

func TestJobGetListErase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	e.addJob(t, "job_1", "a:1", cpx.JobDone, t0)
	e.addJob(t, "job_2", "b:1", cpx.JobRunning, t0.Add(time.Minute))
	e.addJob(t, "job_3", "b:2", cpx.JobDone, t0.Add(2*time.Minute))

	job, err := e.client.Job().Get(ctx, pb.JobGetById("job_2"))
	if err != nil || job.GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("get: %v %v", job, err)
	}

	res, err := e.client.Job().List(ctx, pb.JobListRequest_builder{
		State: pb.JobState_JOB_STATE_DONE.Enum(),
	}.Build())
	if err != nil || len(res.GetItems()) != 2 {
		t.Fatalf("state filter: %v %v", res, err)
	}
	// Newest first.
	if res.GetItems()[0].GetId() != "job_3" {
		t.Errorf("order: %v", res.GetItems())
	}

	// Pagination walks the whole set.
	res, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		PageSize: proto.Int32(2),
	}.Build())
	if err != nil || len(res.GetItems()) != 2 || res.GetNextPageToken() == "" {
		t.Fatalf("page 1: %v %v", res, err)
	}
	res, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		PageSize:  proto.Int32(2),
		PageToken: proto.String(res.GetNextPageToken()),
	}.Build())
	if err != nil || len(res.GetItems()) != 1 || res.GetNextPageToken() != "" {
		t.Fatalf("page 2: %v %v", res, err)
	}

	if _, err := e.client.Job().Erase(ctx, pb.JobById("job_1")); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.Job().Erase(ctx, pb.JobById("job_1"))
	wantCode(t, err, codes.NotFound)
}

func TestJobListByLabel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.addLabeledJob(t, "job_1", "a:1", cpx.JobRunning, now, map[string]string{"team": "x", "env": "prod"})
	e.addLabeledJob(t, "job_2", "b:1", cpx.JobRunning, now, map[string]string{"team": "y", "env": "prod"})

	// A single key selects; the matched job echoes its full label set back.
	res, err := e.client.Job().List(ctx, pb.JobListRequest_builder{
		Labels: map[string]string{"team": "x"},
	}.Build())
	if err != nil || len(res.GetItems()) != 1 || res.GetItems()[0].GetId() != "job_1" {
		t.Fatalf("label filter: %v %v", res, err)
	}
	if res.GetItems()[0].GetLabels()["env"] != "prod" {
		t.Errorf("labels not echoed back: %v", res.GetItems()[0].GetLabels())
	}

	// Subset semantics: every pair must match, so a wrong value excludes.
	res, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		Labels: map[string]string{"team": "x", "env": "stage"},
	}.Build())
	if err != nil || len(res.GetItems()) != 0 {
		t.Fatalf("non-matching subset: %v %v", res, err)
	}

	// A shared key matches both.
	res, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		Labels: map[string]string{"env": "prod"},
	}.Build())
	if err != nil || len(res.GetItems()) != 2 {
		t.Fatalf("shared key: %v %v", res, err)
	}
}

func TestJobWatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addJob(t, "job_1", "a:1", cpx.JobDone, time.Now())

	w, err := e.client.Job().Watch(ctx, pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := w.Recv()
	if err != nil || job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("first frame: %v %v", job, err)
	}
	if _, err := w.Recv(); err != io.EOF {
		t.Fatalf("terminal job must end the stream, got %v", err)
	}

	// A running job streams until it turns terminal.
	e.addJob(t, "job_2", "b:1", cpx.JobRunning, time.Now())
	w, err = e.client.Job().Watch(ctx, pb.JobById("job_2"))
	if err != nil {
		t.Fatal(err)
	}
	if job, err := w.Recv(); err != nil || job.GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("first frame: %v %v", job, err)
	}
	e.jobs.Update("job_2", func(j *cpx.Job) { j.State = cpx.JobDone })
	if job, err := w.Recv(); err != nil || job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("terminal frame: %v %v", job, err)
	}
	if _, err := w.Recv(); err != io.EOF {
		t.Fatalf("stream must end, got %v", err)
	}

	w, err = e.client.Job().Watch(ctx, pb.JobById("nope"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Recv()
	wantCode(t, err, codes.NotFound)
}

func TestJobWatchEviction(t *testing.T) {
	e := newEnv(t)
	e.addJob(t, "job_1", "a:1", cpx.JobRunning, time.Now())

	w, err := e.client.Job().Watch(context.Background(), pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); err != nil {
		t.Fatal(err)
	}
	// Evicting the job mid-stream ends the watch like the SSE endpoint:
	// silently, not with an error status.
	e.jobs.Delete("job_1")
	if _, err := w.Recv(); err != io.EOF {
		t.Fatalf("want EOF after eviction, got %v", err)
	}
}

func TestJobWatchClientCancel(t *testing.T) {
	e := newEnv(t)
	e.addJob(t, "job_1", "a:1", cpx.JobRunning, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	w, err := e.client.Job().Watch(ctx, pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = w.Recv()
	wantCode(t, err, codes.Canceled)
}

func TestListPageTokenValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	_, err := e.client.Job().List(ctx, pb.JobListRequest_builder{
		PageToken: proto.String("abc"),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	_, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		PageToken: proto.String("-1"),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	_, err = e.client.Job().List(ctx, pb.JobListRequest_builder{
		PageSize: proto.Int32(-1),
	}.Build())
	wantCode(t, err, codes.InvalidArgument)
}

func TestJobCancelRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addJob(t, "job_1", "a:1", cpx.JobRunning, time.Now())

	job, err := e.client.Job().Cancel(ctx, pb.JobById("job_1"))
	if err != nil {
		t.Fatal(err)
	}
	if job.GetId() != "job_1" {
		t.Errorf("cancel snapshot: %v", job)
	}

	e.jobs.Update("job_1", func(j *cpx.Job) { j.State = cpx.JobCanceled })
	_, err = e.client.Job().Cancel(ctx, pb.JobById("job_1"))
	wantCode(t, err, codes.FailedPrecondition)

	e.copier.snap = snap("job_2", "a:1", cpx.JobPending, time.Now())
	e.copier.created = true
	var trailer metadata.MD
	job, err = e.client.Job().Retry(ctx, pb.JobById("job_1"), grpc.Trailer(&trailer))
	if err != nil || job.GetId() != "job_2" {
		t.Fatalf("retry: %v %v", job, err)
	}
	if got := trailer.Get("gantry-coalesced"); len(got) != 1 || got[0] != "false" {
		t.Errorf("retry trailer: %v", got)
	}

	// A retry can itself coalesce onto an active twin.
	e.copier.created = false
	if _, err := e.client.Job().Retry(ctx, pb.JobById("job_1"), grpc.Trailer(&trailer)); err != nil {
		t.Fatal(err)
	}
	if got := trailer.Get("gantry-coalesced"); len(got) != 1 || got[0] != "true" {
		t.Errorf("coalesced retry trailer: %v", got)
	}

	e.copier.err = fmt.Errorf("%w: job_9 is running", cpx.ErrJobActive)
	_, err = e.client.Job().Retry(ctx, pb.JobById("job_9"))
	wantCode(t, err, codes.FailedPrecondition)

	e.copier.err = cpx.ErrJobNotFound
	_, err = e.client.Job().Retry(ctx, pb.JobById("job_9"))
	wantCode(t, err, codes.NotFound)
}

func TestJobPlan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.copier.planRes = cpx.PlanResult{
		Ref:       "src.local/lib/app:1",
		Source:    "src",
		Target:    "node",
		SourceRef: "src.local/lib/app@sha256:abc",
		TargetRef: "src.local/lib/app:1",
		As:        []string{"docker.io/lib/app:1"},
		Coalesces: "job_7",
	}

	res, err := e.client.Job().Plan(ctx, pb.JobPlanRequest_builder{
		Ref:    proto.String("src.local/lib/app:1"),
		Target: pb.StoreByName("node"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSourceRef() != "src.local/lib/app@sha256:abc" || res.GetCoalesces() != "job_7" ||
		len(res.GetAs()) != 1 || res.GetAs()[0] != "docker.io/lib/app:1" {
		t.Errorf("unexpected plan: %v", res)
	}

	e.copier.planErr = fmt.Errorf("%w", verify.ErrUnsigned)
	_, err = e.client.Job().Plan(ctx, pb.JobPlanRequest_builder{Ref: proto.String("x:1")}.Build())
	wantCode(t, err, codes.FailedPrecondition)
}
