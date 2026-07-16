package down

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestAnchoredRef(t *testing.T) {
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	got, err := anchoredRef("cache.local:5000/team/app:1", dg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cache.local:5000/team/app@"+dg {
		t.Errorf("anchoredRef = %q", got)
	}
	already := "cache.local/team/app@" + dg
	if got, _ := anchoredRef(already, dg); got != already {
		t.Errorf("digest ref should pass through, got %q", got)
	}
}

func TestDockerPullAnchoredTags(t *testing.T) {
	var mu sync.Mutex
	var pull_tag, tag_repo, tag_tag string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.Contains(r.URL.Path, "/images/create"):
			mu.Lock()
			pull_tag = r.URL.Query().Get("tag")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"status\":\"Pull complete\",\"id\":\"abc\"}\n"))
		case strings.HasSuffix(r.URL.Path, "/tag"):
			mu.Lock()
			tag_repo, tag_tag = r.URL.Query().Get("repo"), r.URL.Query().Get("tag")
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
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
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	if _, err := eng.Pull(context.Background(), "cache.local/team/app:1", dg, "", nil, nil, nopSink{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if pull_tag != dg {
		t.Errorf("pull tag param = %q, want the digest (anchored pull)", pull_tag)
	}
	if tag_repo != "cache.local/team/app" || tag_tag != "1" {
		t.Errorf("tag call = repo %q tag %q, want the original tag restored", tag_repo, tag_tag)
	}
}
