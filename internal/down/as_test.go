package down

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

// tagDaemon is an httptest docker API recording pulls, tags, loads, and
// removals. classic switches the reported image store from containerd
// (containerd-snapshotter) to the classic graph store.
type tagDaemon struct {
	classic bool

	mu      sync.Mutex
	pullTag string
	tags    []string // "repo:tag" per /tag call
	deleted []string // per DELETE /images/{name}
	loads   [][]byte // request body per /images/load call
}

func (f *tagDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.HasSuffix(r.URL.Path, "/info"):
			status := [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}
			if f.classic {
				status = nil
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"Driver": "overlayfs", "DriverStatus": status,
				"OSType": "linux", "Architecture": "x86_64",
			})
		case strings.Contains(r.URL.Path, "/images/create"):
			f.mu.Lock()
			f.pullTag = r.URL.Query().Get("tag")
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"status\":\"Pull complete\",\"id\":\"abc\"}\n"))
		case strings.HasSuffix(r.URL.Path, "/images/load"):
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.loads = append(f.loads, body)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"stream\":\"Loaded image\"}\n"))
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
	if _, err := eng.Pull(context.Background(), "cache.local/team/app:1", dg, "", as, nil, nopSink{}); err != nil {
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
	if _, err := eng.Pull(context.Background(), "cache.local/team/app:1", "", "", []string{"docker.io/team/app:1"}, nil, nopSink{}); err != nil {
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
	if _, err := eng2.Pull(context.Background(), "cache.local/team/app:1", "", "", []string{"cache.local/team/app:1", "docker.io/team/app:1"}, nil, nopSink{}); err != nil {
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

// A digest `as` name on a containerd-store daemon is registered by loading a
// thin OCI archive over the pulled content: the anchor bytes as the only blob,
// one index entry per name — nothing is fetched, nothing else travels.
func TestDockerPullDigestAs(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	sum := sha256.Sum256(raw)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	anchor := &AnchorBlob{MediaType: "application/vnd.oci.image.index.v1+json", Digest: dg, Bytes: raw}

	d := &tagDaemon{}
	eng := tagEngine(t, d)
	as := []string{"cr.example.com/team/app@" + dg, "docker.io/team/app:1"}
	if _, err := eng.Pull(context.Background(), "cache.local/team/app@"+dg, dg, "", as, anchor, nopSink{}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// The tag name is tagged; the digest name is loaded, not tagged.
	if len(d.tags) != 1 || d.tags[0] != "docker.io/team/app:1" {
		t.Errorf("tags = %v", d.tags)
	}
	if len(d.loads) != 1 {
		t.Fatalf("loads = %d, want 1", len(d.loads))
	}
	// The caller renamed the image away from the pull-created digest record.
	if len(d.deleted) != 1 || !strings.Contains(d.deleted[0], "cache.local/team/app") {
		t.Errorf("pull-created record must be dropped, deleted %v", d.deleted)
	}
	// The archive names exactly the digest reference over the anchor bytes.
	names, blob := parseThinArchive(t, d.loads[0])
	if len(names) != 1 || names[0] != as[0] {
		t.Errorf("loaded names = %v, want [%s]", names, as[0])
	}
	if !bytes.Equal(blob, raw) {
		t.Error("anchor blob does not round-trip")
	}
}

// The classic graph store cannot represent a digest reference over local
// content, so a digest `as` name is rejected BEFORE the pull rather than
// silently dropped: a caller that asked for a digest name would otherwise have
// a node quietly pull through to the origin later.
func TestDockerPullDigestAsClassic(t *testing.T) {
	raw := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(raw)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	anchor := &AnchorBlob{MediaType: "application/vnd.oci.image.index.v1+json", Digest: dg, Bytes: raw}

	d := &tagDaemon{classic: true}
	eng := tagEngine(t, d)
	_, err := eng.Pull(context.Background(), "cache.local/team/app@"+dg, dg, "", []string{"cr.example.com/team/app@" + dg}, anchor, nopSink{})
	if err == nil {
		t.Fatal("expected a fail-fast error on the classic graph store, got nil")
	}
	if !strings.Contains(err.Error(), "classic") {
		t.Errorf("error should name the classic image store as the reason, got: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Fail-fast means no side effects: nothing pulled, loaded, or tagged.
	if d.pullTag != "" {
		t.Errorf("must fail before pulling; pullTag = %q", d.pullTag)
	}
	if len(d.loads) != 0 || len(d.tags) != 0 {
		t.Errorf("no side effects expected on rejection; loads=%d tags=%d", len(d.loads), len(d.tags))
	}
}

// The recorded names reported by Pull mirror what the daemon holds: the tags
// tagged, the digest names loaded — never a name the daemon does not hold.
func TestDockerPullRecorded(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	sum := sha256.Sum256(raw)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	anchor := &AnchorBlob{MediaType: "application/vnd.oci.image.index.v1+json", Digest: dg, Bytes: raw}

	d := &tagDaemon{}
	eng := tagEngine(t, d)
	as := []string{"cr.example.com/team/app@" + dg, "docker.io/team/app:1"}
	recorded, err := eng.Pull(context.Background(), "cache.local/team/app@"+dg, dg, "", as, anchor, nopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[0] != as[1] || recorded[1] != as[0] {
		t.Errorf("recorded = %v, want tags then digest names %v", recorded, as)
	}

	// An anchored pull with no `as` records the tag form of the pull ref.
	d2 := &tagDaemon{}
	eng2 := tagEngine(t, d2)
	anchor2dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	recorded, err = eng2.Pull(context.Background(), "cache.local/team/app:1", anchor2dg, "", nil, nil, nopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0] != "cache.local/team/app:1" {
		t.Errorf("recorded = %v, want the pull ref tag form", recorded)
	}
}

// A digest reference the retention index owns vetoes the reap: a digest-`as`
// job may have named the content between the GC plan and this apply, and the
// loaded name is invisible to the RepoTags re-check.
func TestDockerReapSkipsOwnedDigest(t *testing.T) {
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	d := &fakeDaemon{
		inspect: map[string]string{
			"sha256:bbb": `{"Id":"sha256:bbb","RepoTags":[],"RepoDigests":["cr.example.com/team/app@` + dg + `"]}`,
		},
		containers: `[]`,
	}
	eng := fakeDockerEngine(t, d)
	_, ok, err := eng.ReapUntagged(context.Background(), "sha256:bbb", func(ref string) bool {
		return strings.Contains(ref, "cr.example.com/team/app@")
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("owned digest reference must veto the reap")
	}
	if got := d.removed(); len(got) != 0 {
		t.Errorf("nothing must be deleted, got %v", got)
	}
}

// A digest name that does not carry the anchored digest — or arrives without
// the anchor bytes — is refused before anything reaches the daemon.
func TestDockerPullDigestAsValidation(t *testing.T) {
	raw := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(raw)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	other := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	d := &tagDaemon{}
	eng := tagEngine(t, d)
	_, err := eng.Pull(context.Background(), "cache.local/team/app@"+dg, dg,
		"", []string{"cr.example.com/team/app@" + other},
		&AnchorBlob{MediaType: "m", Digest: dg, Bytes: raw}, nopSink{})
	if err == nil || !strings.Contains(err.Error(), "does not carry") {
		t.Errorf("mismatched digest name must be refused, got %v", err)
	}

	_, err = eng.Pull(context.Background(), "cache.local/team/app@"+dg, dg,
		"", []string{"cr.example.com/team/app@" + dg}, nil, nopSink{})
	if err == nil || !strings.Contains(err.Error(), "anchor") {
		t.Errorf("missing anchor bytes must be refused, got %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.loads) != 0 {
		t.Errorf("nothing must be loaded on refusal, got %d", len(d.loads))
	}
}

// parseThinArchive extracts the registered names and the anchor blob from a
// thin OCI archive.
func parseThinArchive(t *testing.T, b []byte) (names []string, blob []byte) {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = data
	}
	var idx struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	for _, m := range idx.Manifests {
		names = append(names, m.Annotations["io.containerd.image.name"])
		blob = files["blobs/sha256/"+strings.TrimPrefix(m.Digest, "sha256:")]
	}
	return names, blob
}
