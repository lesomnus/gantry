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

// tagDaemon is an httptest docker API recording pulls, tags, and removals.
type tagDaemon struct {
	mu      sync.Mutex
	pullTag string
	tags    []string // "repo:tag" per /tag call
	deleted []string // per DELETE /images/{name}
}

func (f *tagDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.Contains(r.URL.Path, "/images/create"):
			f.mu.Lock()
			f.pullTag = r.URL.Query().Get("tag")
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"status\":\"Pull complete\",\"id\":\"abc\"}\n"))
		case strings.HasSuffix(r.URL.Path, "/tag"):
			f.mu.Lock()
			f.tags = append(f.tags, r.URL.Query().Get("repo")+":"+r.URL.Query().Get("tag"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/images/"):
			f.mu.Lock()
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path[strings.Index(r.URL.Path, "/images/"):], "/images/"))
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func tagEngine(t *testing.T, d *tagDaemon) *dockerEngine {
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

// An anchored pull with `as` tags the digest-pulled content with exactly the
// requested names — the pull reference never becomes a tag.
func TestDockerPullAnchoredAs(t *testing.T) {
	d := &tagDaemon{}
	eng := tagEngine(t, d)
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	as := []string{"docker.io/team/app:1", "legacy.io/team/app:stable"}
	if err := eng.Pull(context.Background(), "cache.local/team/app:1", dg, "", as, nopSink{}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pullTag != dg {
		t.Errorf("pull tag = %q, want the digest", d.pullTag)
	}
	want := []string{"docker.io/team/app:1", "legacy.io/team/app:stable"}
	if len(d.tags) != 2 || d.tags[0] != want[0] || d.tags[1] != want[1] {
		t.Errorf("tags = %v, want %v", d.tags, want)
	}
	if len(d.deleted) != 0 {
		t.Errorf("nothing to untag on an anchored pull, deleted %v", d.deleted)
	}
}

// An unanchored pull with `as` renames the image away from the pull-created
// tag: the new names are tagged and the pull name is removed.
func TestDockerPullUnanchoredAs(t *testing.T) {
	d := &tagDaemon{}
	eng := tagEngine(t, d)
	if err := eng.Pull(context.Background(), "cache.local/team/app:1", "", "", []string{"docker.io/team/app:1"}, nopSink{}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pullTag != "1" {
		t.Errorf("pull tag = %q, want the tag form", d.pullTag)
	}
	if len(d.tags) != 1 || d.tags[0] != "docker.io/team/app:1" {
		t.Errorf("tags = %v", d.tags)
	}
	if len(d.deleted) != 1 || !strings.Contains(d.deleted[0], "cache.local/team/app") {
		t.Errorf("the pull-created tag must be dropped, deleted %v", d.deleted)
	}

	// Keeping the pull name in `as` skips the removal.
	d2 := &tagDaemon{}
	eng2 := tagEngine(t, d2)
	if err := eng2.Pull(context.Background(), "cache.local/team/app:1", "", "", []string{"cache.local/team/app:1", "docker.io/team/app:1"}, nopSink{}); err != nil {
		t.Fatal(err)
	}
	d2.mu.Lock()
	defer d2.mu.Unlock()
	if len(d2.deleted) != 0 {
		t.Errorf("pull name kept in as must not be removed, deleted %v", d2.deleted)
	}
	if len(d2.tags) != 1 || d2.tags[0] != "docker.io/team/app:1" {
		t.Errorf("tags = %v", d2.tags)
	}
}
