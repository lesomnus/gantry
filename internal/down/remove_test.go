package down

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/docker/docker/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Removing an image that is already gone (deleted out-of-band) must report
// success with an empty result so the caller can sync its retention index;
// otherwise the orphan record fails every GC cycle forever.

func TestDockerRemoveNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("API-Version", "1.44")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/images/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such image: team/app:1"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+srv.Listener.Addr().String()),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Fatal(err)
	}
	eng := &dockerEngine{name: "d", cli: cli}
	res, err := eng.Remove(context.Background(), "team/app:1")
	if err != nil {
		t.Fatalf("remove of a missing image should succeed, got %v", err)
	}
	if len(res.Deleted)+len(res.Untagged) != 0 {
		t.Fatalf("res = %+v, want empty", res)
	}
}

type notFoundImages struct {
	imagesapi.UnimplementedImagesServer
}

func (notFoundImages) Delete(context.Context, *imagesapi.DeleteImageRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.NotFound, `image "team/app:1": not found`)
}

func TestContainerdRemoveNotFound(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	imagesapi.RegisterImagesServer(srv, notFoundImages{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	cli, err := containerd.NewWithConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	eng := &containerdEngine{name: "c", namespace: "default", cli: cli}
	res, err := eng.Remove(context.Background(), "team/app:1")
	if err != nil {
		t.Fatalf("remove of a missing image should succeed, got %v", err)
	}
	if len(res.Deleted)+len(res.Untagged) != 0 {
		t.Fatalf("res = %+v, want empty", res)
	}
}
