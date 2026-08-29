package down

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/dockertest"
)

func containerdAddr() string {
	if a := os.Getenv("CONTAINERD_ADDRESS"); a != "" {
		return a
	}
	return "/run/containerd/containerd.sock"
}

func containerdNamespace() string {
	if n := os.Getenv("CONTAINERD_NAMESPACE"); n != "" {
		return n
	}
	return "gantry"
}

// pullUpstream runs a pull that is expected to succeed, retrying a few times.
// These tests fetch from Docker Hub's CDN, which resets a connection mid-blob
// often enough to redden CI on a change that never touched this package. The
// retry hides only a fault that does not repeat: a real break in the pull path
// fails every attempt and still fails the test, just later.
func pullUpstream(ctx context.Context, t *testing.T, eng *containerdEngine, ref, digest string, as []string, what string) {
	t.Helper()
	const attempts = 3
	var err error
	for i := 1; i <= attempts; i++ {
		if _, err = eng.Pull(ctx, ref, digest, "", as, nil, &recSink{}); err == nil {
			return
		}
		t.Logf("%s (attempt %d/%d): %v", what, i, attempts, err)
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v (last pull error: %v)", what, ctx.Err(), err)
		case <-time.After(time.Duration(i) * time.Second):
		}
	}
	t.Fatalf("%s failed %d times, last: %v", what, attempts, err)
}

// TestContainerdEngineLive exercises the real containerd client against the
// dedicated containerd sidecar. It skips when no socket is reachable.
func TestContainerdEngineLive(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
	dockertest.Lock(t)
	eng, err := newContainerdEngine(config.StoreConfig{
		Name:      "live",
		Kind:      "containerd",
		Address:   containerdAddr(),
		Namespace: containerdNamespace(),
	})
	if err != nil {
		t.Skipf("containerd client (%s): %v", containerdAddr(), err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable containerd (%s): %v", containerdAddr(), err)
	}

	pullUpstream(ctx, t, eng, "docker.io/library/busybox:latest", "", nil, "pull")
}

// TestContainerdAnchoredPull covers the digest-anchored pull path: the
// engine pulls repo@digest, tags it under the requested ref, and — critically —
// does NOT leave the digest-named record behind (which would root the content
// forever, defeating retention GC).
func TestContainerdAnchoredPull(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
	dockertest.Lock(t)
	ns := containerdNamespace()
	eng, err := newContainerdEngine(config.StoreConfig{
		Name: "live", Kind: "containerd", Address: containerdAddr(), Namespace: ns,
	})
	if err != nil {
		t.Skipf("containerd client: %v", err)
	}
	defer eng.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable containerd: %v", err)
	}

	const ref = "docker.io/library/busybox:latest"
	// Resolve the tag's manifest digest first (via a plain pull), then remove it
	// so the anchored pull actually re-resolves by digest.
	pullUpstream(ctx, t, eng, ref, "", nil, "seed pull")
	nctx := namespaces.WithNamespace(ctx, ns)
	img, err := eng.cli.ImageService().Get(nctx, ref)
	if err != nil {
		t.Fatalf("resolve digest: %v", err)
	}
	digest := img.Target.Digest.String()
	repo := "docker.io/library/busybox"
	_, _ = eng.Remove(ctx, ref)
	_, _ = eng.Remove(ctx, repo+"@"+digest) // clear any digest record from the seed

	// Anchored pull: repo@digest, tagged back to ref.
	pullUpstream(ctx, t, eng, ref, digest, nil, "anchored pull")

	// The tag record exists and points at the anchored digest.
	tagged, err := eng.cli.ImageService().Get(nctx, ref)
	if err != nil {
		t.Fatalf("tag record missing after anchored pull: %v", err)
	}
	if tagged.Target.Digest.String() != digest {
		t.Errorf("tag digest = %s, want %s", tagged.Target.Digest, digest)
	}
	// The digest-named record must NOT survive — it would pin content past GC.
	if _, err := eng.cli.ImageService().Get(nctx, repo+"@"+digest); err == nil {
		t.Errorf("digest-named record %s@%s still present; retention GC could never reclaim it", repo, digest)
	}
	_, _ = eng.Remove(ctx, ref)
}

// TestContainerdDigestAs covers digest-named `as` references: an anchored pull
// with a digest name registers a record with that exact name over the pulled
// content, the unrequested pull-created record is dropped, and a name carrying
// a foreign digest is refused.
func TestContainerdDigestAs(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
	dockertest.Lock(t)
	ns := containerdNamespace()
	eng, err := newContainerdEngine(config.StoreConfig{
		Name: "live", Kind: "containerd", Address: containerdAddr(), Namespace: ns,
	})
	if err != nil {
		t.Skipf("containerd client: %v", err)
	}
	defer eng.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable containerd: %v", err)
	}

	const ref = "docker.io/library/busybox:latest"
	pullUpstream(ctx, t, eng, ref, "", nil, "seed pull")
	nctx := namespaces.WithNamespace(ctx, ns)
	img, err := eng.cli.ImageService().Get(nctx, ref)
	if err != nil {
		t.Fatalf("resolve digest: %v", err)
	}
	digest := img.Target.Digest.String()
	repo := "docker.io/library/busybox"
	_, _ = eng.Remove(ctx, ref)
	_, _ = eng.Remove(ctx, repo+"@"+digest)

	// The upstream digest name: what a digest-pinned jobspec resolves.
	upstream := "cr.invalid/library/busybox@" + digest
	t.Cleanup(func() { _, _ = eng.Remove(ctx, upstream) })
	pullUpstream(ctx, t, eng, ref, digest, []string{upstream}, "anchored pull with digest as")
	rec, err := eng.cli.ImageService().Get(nctx, upstream)
	if err != nil {
		t.Fatalf("digest-named record missing: %v", err)
	}
	if rec.Target.Digest.String() != digest {
		t.Errorf("record digest = %s, want %s", rec.Target.Digest, digest)
	}
	// The unrequested pull-created record must not survive.
	if _, err := eng.cli.ImageService().Get(nctx, repo+"@"+digest); err == nil {
		t.Errorf("unrequested pull record %s@%s still present", repo, digest)
	}

	// A digest name that lies about its content is refused.
	lie := "cr.invalid/library/busybox@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := eng.Pull(ctx, ref, digest, "", []string{lie}, nil, &recSink{}); err == nil {
		_, _ = eng.Remove(ctx, lie)
		t.Error("mismatched digest name must be refused")
	}
}
