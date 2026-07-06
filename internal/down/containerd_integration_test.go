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

	if err := eng.Pull(ctx, "docker.io/library/busybox:latest", "", &recSink{}); err != nil {
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
	if err := eng.Pull(ctx, ref, "", &recSink{}); err != nil {
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
	if err := eng.Pull(ctx, ref, digest, &recSink{}); err != nil {
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
