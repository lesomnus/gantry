package rpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func TestImageListGet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.ix.Touch("node", "src.local/a/app:1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.ix.Distributed("node", "src.local/b/app:2", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.ix.Pin("node", "src.local/a/app:1", false); err != nil {
		t.Fatal(err)
	}
	e.eng.inuse["src.local/b/app:2"] = true

	res, err := e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetItems()) != 2 {
		t.Fatalf("want 2 records, got %v", res.GetItems())
	}
	byRef := map[string]*pb.Image{}
	for _, img := range res.GetItems() {
		byRef[img.GetRef()] = img
	}
	if !byRef["src.local/a/app:1"].GetPinned() || byRef["src.local/a/app:1"].GetInUse() {
		t.Errorf("pinned record wrong: %v", byRef["src.local/a/app:1"])
	}
	if !byRef["src.local/b/app:2"].GetInUse() || byRef["src.local/b/app:2"].GetPinned() {
		t.Errorf("in-use record wrong: %v", byRef["src.local/b/app:2"])
	}

	// Filters.
	res, err = e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store:  pb.StoreByName("node"),
		Pinned: proto.Bool(true),
	}.Build())
	if err != nil || len(res.GetItems()) != 1 {
		t.Fatalf("pinned filter: %v %v", res, err)
	}
	res, err = e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("node"),
		InUse: proto.Bool(true),
	}.Build())
	if err != nil || len(res.GetItems()) != 1 || res.GetItems()[0].GetRef() != "src.local/b/app:2" {
		t.Fatalf("in_use filter: %v %v", res, err)
	}

	// Get by locator and by the synthesized id.
	img, err := e.client.Image().Get(ctx, pb.ImageGetByLocator(pb.StoreByName("node"), "src.local/a/app:1"))
	if err != nil || !img.GetPinned() {
		t.Fatalf("get by locator: %v %v", img, err)
	}
	img2, err := e.client.Image().Get(ctx, pb.ImageGetById(img.GetId()))
	if err != nil || img2.GetRef() != "src.local/a/app:1" {
		t.Fatalf("get by id: %v %v", img2, err)
	}

	// Erase purges the record only.
	if _, err := e.client.Image().Erase(ctx, pb.ImageByLocator(pb.StoreByName("node"), "src.local/a/app:1")); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.Image().Get(ctx, pb.ImageGetByLocator(pb.StoreByName("node"), "src.local/a/app:1"))
	wantCode(t, err, codes.NotFound)

	_, err = e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("src"),
	}.Build())
	wantCode(t, err, codes.NotFound)

	// A dead daemon fails the list: in_use cannot be answered.
	e.eng.inuseErr = context.DeadlineExceeded
	_, err = e.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	wantCode(t, err, codes.Unavailable)
}

func TestImageGcDisabled(t *testing.T) {
	e := newEnv(t, withoutGC())
	_, err := e.client.Image().List(context.Background(), pb.ImageListRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	wantCode(t, err, codes.FailedPrecondition)
}

func TestPin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	pin, err := e.client.Pin().Add(ctx, pb.PinAddRequest_builder{
		Store: pb.StoreByName("node"),
		Value: "src.local/a/app:1",
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if pin.GetPattern() || pin.GetDatePinned() == nil {
		t.Errorf("unexpected pin: %v", pin)
	}

	_, err = e.client.Pin().Add(ctx, pb.PinAddRequest_builder{
		Store:   pb.StoreByName("node"),
		Value:   "*:stable",
		Pattern: true,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.client.Pin().Add(ctx, pb.PinAddRequest_builder{
		Store:   pb.StoreByName("node"),
		Value:   "[",
		Pattern: true,
	}.Build())
	wantCode(t, err, codes.InvalidArgument)

	res, err := e.client.Pin().List(ctx, pb.PinListRequest_builder{
		Store: pb.StoreByName("node"),
	}.Build())
	if err != nil || len(res.GetItems()) != 2 {
		t.Fatalf("list: %v %v", res, err)
	}

	got, err := e.client.Pin().Get(ctx, pb.PinGetByValue(pb.StoreByName("node"), "*:stable"))
	if err != nil || !got.GetPattern() {
		t.Fatalf("get by value: %v %v", got, err)
	}
	got2, err := e.client.Pin().Get(ctx, pb.PinGetById(got.GetId()))
	if err != nil || got2.GetValue() != "*:stable" {
		t.Fatalf("get by id: %v %v", got2, err)
	}

	// Erase is idempotent, like DELETE /pin.
	if _, err := e.client.Pin().Erase(ctx, pb.PinByValue(pb.StoreByName("node"), "*:stable")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.Pin().Erase(ctx, pb.PinByValue(pb.StoreByName("node"), "*:stable")); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.Pin().Get(ctx, pb.PinGetByValue(pb.StoreByName("node"), "*:stable"))
	wantCode(t, err, codes.NotFound)
}

func TestEvent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, ev := range []event.Event{
		{Type: event.ImagePulled, Store: "node", Ref: "a:1"},
		{Type: event.JobDone, Ref: "a:1", State: "done", Detail: []byte(`{"job":"job_1","bytes":42}`)},
		{Type: event.Pinned, Store: "node", Ref: "*:stable"},
	} {
		if err := e.events.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	res, err := e.client.Event().List(ctx, &pb.EventListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetItems()) != 3 || res.GetItems()[0].GetType() != pb.EventType_EVENT_TYPE_PINNED {
		t.Fatalf("list newest-first: %v", res.GetItems())
	}

	res, err = e.client.Event().List(ctx, pb.EventListRequest_builder{
		Type: pb.EventType_EVENT_TYPE_JOB_DONE.Enum(),
	}.Build())
	if err != nil || len(res.GetItems()) != 1 {
		t.Fatalf("type filter: %v %v", res, err)
	}
	got := res.GetItems()[0]
	if got.GetState() != pb.JobState_JOB_STATE_DONE || got.GetDetail().GetJob() != "job_1" || got.GetDetail().GetBytes() != 42 {
		t.Errorf("event mapping: %v", got)
	}

	one, err := e.client.Event().Get(ctx, pb.EventGetBySeq(got.GetSeq()))
	if err != nil || one.GetSeq() != got.GetSeq() {
		t.Fatalf("get: %v %v", one, err)
	}
	_, err = e.client.Event().Get(ctx, pb.EventGetBySeq(999))
	wantCode(t, err, codes.NotFound)
}

func TestEventDisabled(t *testing.T) {
	e := newEnv(t, withoutEvents())
	_, err := e.client.Event().List(context.Background(), &pb.EventListRequest{})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestEventPagination(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := e.events.Append(event.Event{Type: event.ImagePulled, Ref: "a:1"}); err != nil {
			t.Fatal(err)
		}
	}

	// Walk newest-first with page_size=1: seq 3, 2, 1.
	var seqs []uint64
	token := ""
	for {
		req := pb.EventListRequest_builder{PageSize: proto.Int32(1)}
		if token != "" {
			req.PageToken = proto.String(token)
		}
		res, err := e.client.Event().List(ctx, req.Build())
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range res.GetItems() {
			seqs = append(seqs, ev.GetSeq())
		}
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seqs) != 3 || seqs[0] != 3 || seqs[2] != 1 {
		t.Fatalf("pagination walk: %v", seqs)
	}

	// A resumed walk that dropped page_size still honors the token.
	res, err := e.client.Event().List(ctx, pb.EventListRequest_builder{
		PageToken: proto.String("1"),
	}.Build())
	if err != nil || len(res.GetItems()) != 2 {
		t.Fatalf("token without size: %v %v", res, err)
	}
}
