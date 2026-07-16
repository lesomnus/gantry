package down

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/lesomnus/gantry/cmd/config"
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

// TestContainerdEngineLive exercises the real containerd client against the
// dedicated containerd sidecar. It skips when no socket is reachable.
func TestContainerdEngineLive(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
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

	if _, err := eng.Pull(ctx, "docker.io/library/busybox:latest", "", "", nil, nil, &recSink{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

// TestContainerdAnchoredPull covers the digest-anchored pull path: the
// engine pulls repo@digest, tags it under the requested ref, and — critically —
// does NOT leave the digest-named record behind (which would root the content
// forever, defeating retention GC).
func TestContainerdAnchoredPull(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
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
	if _, err := eng.Pull(ctx, ref, "", "", nil, nil, &recSink{}); err != nil {
		t.Fatalf("seed pull: %v", err)
	}
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
	if _, err := eng.Pull(ctx, ref, digest, "", nil, nil, &recSink{}); err != nil {
		t.Fatalf("anchored pull: %v", err)
	}

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
	if _, err := eng.Pull(ctx, ref, "", "", nil, nil, &recSink{}); err != nil {
		t.Fatalf("seed pull: %v", err)
	}
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
	if _, err := eng.Pull(ctx, ref, digest, "", []string{upstream}, nil, &recSink{}); err != nil {
		t.Fatalf("anchored pull with digest as: %v", err)
	}
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
