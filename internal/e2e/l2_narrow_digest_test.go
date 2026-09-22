//go:build e2e

package e2e

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/pb"
	"github.com/notaryproject/notation-core-go/testhelper"
)

// runnableBase is what the daemon's platform child is built from: something a
// container can actually start, since enforcement acts on container starts.
const runnableBase = "docker.io/library/busybox:1.37"

// seedRunnableIndex pushes a two-platform index to host/repo:tag whose child
// for the daemon's own platform is runnable, and returns the index digest, that
// child's digest, and the platform. The other child is random bytes: it exists
// so there is something for a narrowed fill to leave behind.
func seedRunnableIndex(t *testing.T, cli *client.Client, host, repo, tag string) (index, child v1.Hash, platform string) {
	t.Helper()
	info, err := cli.Info(context.Background())
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[info.Architecture]
	if arch == "" {
		arch = info.Architecture
	}
	own := v1.Platform{OS: info.OSType, Architecture: arch}
	other := v1.Platform{OS: "linux", Architecture: "arm64"}
	if arch == "arm64" {
		other.Architecture = "amd64"
	}

	base, err := name.ParseReference(runnableBase)
	if err != nil {
		t.Fatal(err)
	}
	runnable, err := remote.Image(base, remote.WithPlatform(own))
	if err != nil {
		t.Skipf("fetch %s for %s: %v", runnableBase, own.String(), err)
	}
	filler, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(v1.ImageIndex(empty.Index),
		mutate.IndexAddendum{Add: runnable, Descriptor: v1.Descriptor{Platform: &own}},
		mutate.IndexAddendum{Add: filler, Descriptor: v1.Descriptor{Platform: &other}},
	)
	if err := remote.WriteIndex(insecureTag(t, host, repo, tag), idx); err != nil {
		t.Fatalf("push runnable index: %v", err)
	}
	if index, err = idx.Digest(); err != nil {
		t.Fatal(err)
	}
	if child, err = runnable.Digest(); err != nil {
		t.Fatal(err)
	}
	return index, child, own.String()
}

// The whole of what a narrowed routed fill has to preserve for a node that is
// named by digest and policed at run time.
//
// The job is pinned to the INDEX and names the node's image after it — what
// bosun sends for a jobspec that reads `image = repo@sha256:INDEX`. The route
// carries only the daemon's platform into the cache, so the only thing that
// could have held the index for the node is the admission read of the origin.
// What has to come out of that:
//
//   - the cache holds the platform's own manifest and not the index;
//   - the daemon resolves the name the job asked for to the index — locally,
//     which is the pull-skip the name exists for — and holds nothing named
//     after the child;
//   - a container started from that name survives enforcement with only the
//     index signed, as `notation sign <index>` leaves it;
//   - and the same child, started under its own digest, does not — which is
//     what makes the previous point a statement about the node's records rather
//     than about a watcher that never looked.
func TestL2NarrowedDigestNameRunsUnderEnforcement(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trust := writeTrustStore(t, root.Cert)
	h := newL2Harness(t, l2WithRemoteCache("cache"), l2WithEnforce(trust))
	ctx := context.Background()

	index, child, platform := seedRunnableIndex(t, h.cli, h.remote, "lib/run", "1")
	signRef(t, h.remote+"/lib/run@"+index.String(), root, leaf)

	named := h.remote + "/lib/run@" + index.String()
	childRef := h.remote + "/lib/run@" + child.String()
	cachePull := h.cache + "/lib/run@" + child.String()
	for _, ref := range []string{named, childRef, cachePull} {
		h.removeImage(ref)
		t.Cleanup(func() { h.removeImage(ref) })
	}

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    named,
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("edge"),
		As:     []string{named},
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 2 {
		t.Fatalf("transfers = %d [%s], want a fill and a delivery", n, describe(job))
	}

	// The fill carried one platform.
	if _, err := digestByRef(t, h.cache, "lib/run", child.String()); err != nil {
		t.Errorf("the cache does not hold the %s manifest: %v", platform, err)
	}
	if _, err := digestByRef(t, h.cache, "lib/run", index.String()); err == nil {
		t.Error("the cache holds the index; the fill was not narrowed")
	}

	// The node holds the index under the name the job asked for, and nothing
	// named after the child.
	img, err := h.cli.ImageInspect(ctx, named)
	if err != nil {
		t.Fatalf("the daemon does not resolve %s: %v", named, err)
	}
	if img.ID != index.String() {
		t.Errorf("%s is image %s, want the index %s", named, img.ID, index)
	}
	if len(img.RepoDigests) != 1 || img.RepoDigests[0] != named {
		t.Errorf("RepoDigests = %v, want only %s", img.RepoDigests, named)
	}
	if _, err := h.cli.ImageInspect(ctx, cachePull); err == nil {
		t.Errorf("the daemon still holds %s, the record the platform pull made", cachePull)
	}

	start := func(name, ref string) string {
		t.Helper()
		c, err := h.cli.ContainerCreate(ctx,
			&container.Config{Image: ref, Cmd: []string{"sleep", "120"}},
			&container.HostConfig{}, nil, nil, name)
		if err != nil {
			t.Fatalf("create %s from %s: %v", name, ref, err)
		}
		t.Cleanup(func() {
			_ = h.cli.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true})
		})
		if err := h.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return c.ID
	}
	gone := func(id string) bool {
		_, err := h.cli.ContainerInspect(ctx, id)
		return client.IsErrNotFound(err)
	}
	suffix := index.Hex[:12]

	// Created without a pull, so it runs exactly what the job left.
	warmed := start("gantry-e2e-narrowed-"+suffix, named)

	// The control: the same child under its own digest. Pulled directly, so it
	// has the RepoDigest a node records when it keeps the platform manifest.
	rc, err := h.cli.ImagePull(ctx, childRef, image.PullOptions{Platform: platform})
	if err != nil {
		t.Fatalf("pull %s: %v", childRef, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	control := start("gantry-e2e-child-"+suffix, childRef)

	// One watcher decides in start order, so once the control is gone the
	// warmed container has been decided too.
	deadline := time.Now().Add(60 * time.Second)
	for !gone(control) {
		if time.Now().After(deadline) {
			t.Fatal("a container started from the unsigned platform manifest was not quarantined; the watcher is not deciding")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if gone(warmed) {
		t.Fatal("the container started from the name the job left was quarantined")
	}
	ins, err := h.cli.ContainerInspect(ctx, warmed)
	if err != nil {
		t.Fatal(err)
	}
	if !ins.State.Running {
		t.Errorf("the warmed container is %s, want running", ins.State.Status)
	}
}
