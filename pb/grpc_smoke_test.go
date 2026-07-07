package pb_test

import (
	"context"
	"net"
	"testing"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Sanity check for the generated stubs and wiring: every service registers
// through RegisterServer and round-trips over gRPC. UnimplementedServer is
// what the real server will embed for RPCs it does not support.
func TestGeneratedWiring(t *testing.T) {
	l := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	pb.RegisterServer(g, pb.UnimplementedServer{})
	// Non-entity services are not part of the orm-generated wiring and
	// register individually.
	pb.RegisterGcServiceServer(g, pb.UnimplementedGcServiceServer{})
	pb.RegisterVerifyServiceServer(g, pb.UnimplementedVerifyServiceServer{})
	go g.Serve(l)
	defer g.Stop()

	// An unregistered service would also answer Unimplemented below, so
	// prove registration itself first.
	info := g.GetServiceInfo()
	for _, name := range []string{
		"gantry.StoreService",
		"gantry.JobService",
		"gantry.ImageService",
		"gantry.PinService",
		"gantry.EventService",
		"gantry.GcService",
		"gantry.VerifyService",
	} {
		if _, ok := info[name]; !ok {
			t.Errorf("service %s is not registered", name)
		}
	}

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return l.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	c := pb.NewClient(conn)
	ctx := context.Background()

	// Ref/Pick helpers build the requests; the composite unique index is
	// addressable through the generated RefBy message.
	img := pb.ImageByLocator(pb.StoreByName("cache"), "cache.local/team/app:1.2")
	if _, err := c.Image().Get(ctx, img.Pick()); status.Code(err) != codes.Unimplemented {
		t.Errorf("Image.Get: want Unimplemented, got %v", err)
	}
	if _, err := c.Store().Get(ctx, pb.StoreGetByName("cache")); status.Code(err) != codes.Unimplemented {
		t.Errorf("Store.Get: want Unimplemented, got %v", err)
	}
	if _, err := c.Job().List(ctx, pb.JobListRequest_builder{
		State: pb.JobState_JOB_STATE_RUNNING.Enum(),
	}.Build()); status.Code(err) != codes.Unimplemented {
		t.Errorf("Job.List: want Unimplemented, got %v", err)
	}
	if _, err := c.Pin().Erase(ctx, pb.PinByValue(pb.StoreByName("cache"), "*:stable")); status.Code(err) != codes.Unimplemented {
		t.Errorf("Pin.Erase: want Unimplemented, got %v", err)
	}
	if _, err := c.Event().List(ctx, &pb.EventListRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Event.List: want Unimplemented, got %v", err)
	}
	if w, err := c.Job().Watch(ctx, pb.JobWatchRequest_builder{
		Ref: pb.JobById("job_0"),
	}.Build()); err != nil {
		t.Errorf("Job.Watch: %v", err)
	} else if _, err := w.Recv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("Job.Watch recv: want Unimplemented, got %v", err)
	}
	if _, err := pb.NewGcServiceClient(conn).Status(ctx, &pb.GcStatusRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Gc.Status: want Unimplemented, got %v", err)
	}
	if _, err := pb.NewVerifyServiceClient(conn).Describe(ctx, &pb.VerifyDescribeRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Verify.Describe: want Unimplemented, got %v", err)
	}
}
