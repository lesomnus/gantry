package down

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

// fakeDaemon is an httptest docker API answering the reap choreography:
// image list, image inspect, container list, and image delete.
type fakeDaemon struct {
	mu         sync.Mutex
	listJSON   string            // GET /images/json
	inspect    map[string]string // image id -> GET /images/{id}/json body ("" => 404)
	containers string            // GET /containers/json
	deleted    []string
	deleteCode map[string]int // ref -> status for DELETE (default 200 with an untag entry)
}

func (f *fakeDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/images/json"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(f.listJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/json"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(f.containers))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/json"): // /images/{id}/json
			id := strings.TrimSuffix(p[strings.Index(p, "/images/")+len("/images/"):], "/json")
			body := f.inspect[id]
			w.Header().Set("Content-Type", "application/json")
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such image"}`))
				return
			}
			w.Write([]byte(body))
		case r.Method == http.MethodDelete:
			ref := p[strings.Index(p, "/images/")+len("/images/"):]
			f.deleted = append(f.deleted, ref)
			w.Header().Set("Content-Type", "application/json")
			if code := f.deleteCode[ref]; code != 0 {
				w.WriteHeader(code)
				w.Write([]byte(`{"message":"cannot delete"}`))
				return
			}
			w.Write([]byte(`[{"Untagged":"` + ref + `"}]`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func (f *fakeDaemon) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func fakeDockerEngine(t *testing.T, d *fakeDaemon) *dockerEngine {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+srv.Listener.Addr().String()),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &dockerEngine{name: "d", cli: cli, inflight: map[string]int{}}
}

func TestDockerImagesInventory(t *testing.T) {
	eng := fakeDockerEngine(t, &fakeDaemon{
		listJSON: `[
			{"Id":"sha256:aaa","RepoTags":["r/a:1","r/a:2"],"RepoDigests":["r/a@sha256:1"]},
			{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":["r/b@sha256:2"]},
			{"Id":"sha256:ccc","RepoTags":["<none>:<none>"],"RepoDigests":["<none>@<none>"]}
		]`,
	})
	inv, err := eng.Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Refs) != 2 || inv.Refs[0] != "r/a:1" || inv.Refs[1] != "r/a:2" {
		t.Errorf("refs = %v", inv.Refs)
	}
	if len(inv.Untagged) != 2 {
		t.Fatalf("untagged = %+v", inv.Untagged)
	}
	if inv.Untagged[0].ID != "sha256:bbb" || len(inv.Untagged[0].RepoDigests) != 1 {
		t.Errorf("digest-ref'd untagged image = %+v", inv.Untagged[0])
	}
	if inv.Untagged[1].ID != "sha256:ccc" || len(inv.Untagged[1].RepoDigests) != 0 {
		t.Errorf("<none> placeholders must be dropped: %+v", inv.Untagged[1])
	}
}

func TestDockerReapRemovesDigestRefsOnly(t *testing.T) {
	d := &fakeDaemon{
		inspect: map[string]string{
			"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":["r/a@sha256:x","r/b@sha256:x"]}`,
		},
		containers: `[]`,
	}
	eng := fakeDockerEngine(t, d)
	rr, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb")
	if err != nil || !ok {
		t.Fatalf("reap = ok %v err %v, want ok", ok, err)
	}
	// Content is freed with the last digest ref; no by-ID delete follows.
	want := []string{"r/a@sha256:x", "r/b@sha256:x"}
	got := d.removed()
	if len(got) != len(want) {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deletes = %v, want %v", got, want)
		}
	}
	if len(rr.Untagged) != 2 {
		t.Errorf("rr = %+v, want both digest refs untagged", rr)
	}
}

func TestDockerReapRefLessRemovesByID(t *testing.T) {
	d := &fakeDaemon{
		inspect:    map[string]string{"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":[]}`},
		containers: `[]`,
	}
	eng := fakeDockerEngine(t, d)
	_, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb")
	if err != nil || !ok {
		t.Fatalf("reap = ok %v err %v, want ok", ok, err)
	}
	if got := d.removed(); len(got) != 1 || got[0] != "sha256:bbb" {
		t.Errorf("deletes = %v, want the bare ID only", got)
	}
}

func TestDockerReapConflictIsTransientSkip(t *testing.T) {
	d := &fakeDaemon{
		inspect:    map[string]string{"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":["r/a@sha256:x"]}`},
		containers: `[]`,
		// e.g. dependent child images: the daemon answers 409.
		deleteCode: map[string]int{"r/a@sha256:x": http.StatusConflict},
	}
	eng := fakeDockerEngine(t, d)
	_, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb")
	if err != nil || ok {
		t.Fatalf("reap = ok %v err %v, want a transient skip (no error) on 409", ok, err)
	}
}

func TestDockerReapSkipsRetagged(t *testing.T) {
	d := &fakeDaemon{
		inspect:    map[string]string{"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":["r/a:rollback"],"RepoDigests":[]}`},
		containers: `[]`,
	}
	eng := fakeDockerEngine(t, d)
	_, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb")
	if err != nil || ok {
		t.Fatalf("reap = ok %v err %v, want a skip", ok, err)
	}
	if got := d.removed(); len(got) != 0 {
		t.Errorf("a re-tagged image must not be touched: %v", got)
	}
}

func TestDockerReapSkipsContainerRef(t *testing.T) {
	d := &fakeDaemon{
		inspect:    map[string]string{"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":[]}`},
		containers: `[{"Id":"c1","Image":"sha256:bbb","ImageID":"sha256:bbb","State":"exited"}]`,
	}
	eng := fakeDockerEngine(t, d)
	_, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb")
	if err != nil || ok {
		t.Fatalf("reap = ok %v err %v, want a skip", ok, err)
	}
	if got := d.removed(); len(got) != 0 {
		t.Errorf("an image referenced by a (stopped) container must not be touched: %v", got)
	}
}

func TestDockerReapSkipsInFlightPull(t *testing.T) {
	// The pull registers gantry's canonical spelling; the daemon reports the
	// familiar form. The registry must match them (a Docker Hub rollback pull
	// racing the reaper is exactly this shape).
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	d := &fakeDaemon{
		inspect:    map[string]string{"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":["nginx@` + dg + `"]}`},
		containers: `[]`,
	}
	eng := fakeDockerEngine(t, d)
	done := eng.trackPull("index.docker.io/library/nginx@" + dg)
	if _, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb"); err != nil || ok {
		t.Fatalf("reap = ok %v err %v, want a skip while the pull is in flight", ok, err)
	}
	if got := d.removed(); len(got) != 0 {
		t.Errorf("no delete while a pull holds the digest: %v", got)
	}
	done()
	if _, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb"); err != nil || !ok {
		t.Fatalf("reap after the pull = ok %v err %v, want ok", ok, err)
	}
}

func TestDockerReapGoneIsConverged(t *testing.T) {
	eng := fakeDockerEngine(t, &fakeDaemon{inspect: map[string]string{}})
	rr, ok, err := eng.ReapUntagged(context.Background(), "sha256:gone")
	if err != nil || !ok {
		t.Fatalf("reap = ok %v err %v, want ok for an image already gone", ok, err)
	}
	if len(rr.Deleted)+len(rr.Untagged) != 0 {
		t.Errorf("rr = %+v, want empty", rr)
	}
}

func TestReconcilerCapability(t *testing.T) {
	if caps := Capabilities(&dockerEngine{}); !caps.Reconcile {
		t.Error("docker must report the reconcile capability")
	}
	if caps := Capabilities(&containerdEngine{}); caps.Reconcile {
		t.Error("containerd must not report the reconcile capability (it self-heals)")
	}
}
