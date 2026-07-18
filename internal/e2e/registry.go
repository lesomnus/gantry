package e2e

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// quietRegistry is a ggcr in-memory registry with its request logging silenced.
func quietRegistry() http.Handler {
	return registry.New(
		registry.WithReferrersSupport(true),
		registry.Logger(log.New(io.Discard, "", 0)),
	)
}

// newRegistry starts an in-memory OCI registry (referrers API enabled) on
// loopback and returns its host:port. It is a real registry served over plain
// HTTP — the source/cache backing for the hermetic tier.
func newRegistry(t *testing.T) (host string, close func()) {
	t.Helper()
	srv := httptest.NewServer(quietRegistry())
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

// newCountingRegistry is newRegistry plus a counter of completed blob uploads
// (the final PUT to /blobs/uploads/), for asserting incremental copy skips
// already-present blobs.
func newCountingRegistry(t *testing.T) (host string, uploads *int32, close func()) {
	t.Helper()
	var n int32
	inner := quietRegistry()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			atomic.AddInt32(&n, 1)
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	return strings.TrimPrefix(srv.URL, "http://"), &n, srv.Close
}

func insecureTag(t *testing.T, host, repo, tag string) name.Tag {
	t.Helper()
	ref, err := name.NewTag(fmt.Sprintf("%s/%s:%s", host, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("parse tag: %v", err)
	}
	return ref
}

// seedImage pushes a random single-platform image to host/repo:tag and returns
// its manifest digest.
func seedImage(t *testing.T, host, repo, tag string) v1.Hash {
	t.Helper()
	img, err := random.Image(2048, 3)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	if err := remote.Write(insecureTag(t, host, repo, tag), img); err != nil {
		t.Fatalf("push seed image: %v", err)
	}
	h, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedIndex pushes a random multi-platform index and returns its digest.
func seedIndex(t *testing.T, host, repo, tag string) v1.Hash {
	t.Helper()
	idx, err := random.Index(2048, 2, 2)
	if err != nil {
		t.Fatalf("random index: %v", err)
	}
	if err := remote.WriteIndex(insecureTag(t, host, repo, tag), idx); err != nil {
		t.Fatalf("push seed index: %v", err)
	}
	h, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedPlatformIndex pushes an index with one child image per given platform
// ("os/arch") and returns its digest, so platform selection can be exercised.
func seedPlatformIndex(t *testing.T, host, repo, tag string, platforms ...string) v1.Hash {
	t.Helper()
	idx := v1.ImageIndex(empty.Index)
	var adds []mutate.IndexAddendum
	for _, p := range platforms {
		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatal(err)
		}
		os, arch, _ := strings.Cut(p, "/")
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}},
		})
	}
	idx = mutate.AppendManifests(idx, adds...)
	if err := remote.WriteIndex(insecureTag(t, host, repo, tag), idx); err != nil {
		t.Fatalf("push platform index: %v", err)
	}
	h, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// hasTag reports whether host holds repo:tag.
func hasTag(t *testing.T, host, repo, tag string) bool {
	t.Helper()
	_, err := remote.Head(insecureTag(t, host, repo, tag))
	return err == nil
}

// digestByRef resolves host/repo@digest, proving a manifest is present by digest.
func digestByRef(t *testing.T, host, repo, digest string) (v1.Hash, error) {
	t.Helper()
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", host, repo, digest), name.Insecure)
	if err != nil {
		return v1.Hash{}, err
	}
	d, err := remote.Head(ref)
	if err != nil {
		return v1.Hash{}, err
	}
	return d.Digest, nil
}

// digestOf returns the manifest digest of host/repo:tag.
func digestOf(t *testing.T, host, repo, tag string) (v1.Hash, error) {
	t.Helper()
	d, err := remote.Head(insecureTag(t, host, repo, tag))
	if err != nil {
		return v1.Hash{}, err
	}
	return d.Digest, nil
}
