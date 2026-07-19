package down

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/lesomnus/gantry/cmd/config"
)

// TestDockerAnchoredPlatformPullRecordsIndexDigest proves the daemon-side half of
// how a signed multi-arch image is enforced: when gantry pulls a specific
// platform anchored to the image's INDEX digest, the daemon records that INDEX
// digest as the RepoDigest (not the platform-specific manifest digest), even
// though only the one platform's content is pulled. That index digest is exactly
// what a notation signature is over and what enforcement keys on. Skips without a
// reachable daemon.
func TestDockerAnchoredPlatformPullRecordsIndexDigest(t *testing.T) {
	eng, err := newDockerEngine(config.StoreConfig{Name: "live", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	// alpine is a multi-arch image; a plain pull records its index digest. A pull
	// failure here means no registry access — skip, like a missing daemon.
	const ref = "alpine:latest"
	if _, err := eng.Pull(ctx, ref, "", "", nil, nil, nopSink{}); err != nil {
		t.Skipf("cannot reach a registry for %s: %v", ref, err)
	}
	base, err := eng.cli.ImageInspect(ctx, ref)
	if err != nil || len(base.RepoDigests) == 0 {
		t.Skipf("no repo digest for %s (repoDigests=%v err=%v)", ref, base.RepoDigests, err)
	}
	rd := base.RepoDigests[0] // "alpine@sha256:<index>"
	indexDigest := rd[strings.LastIndex(rd, "@")+1:]

	// Remove, then re-pull ONE platform anchored to the index digest — the shape
	// of a gantry local->docker copy with a `platforms` narrowing. If the re-pull
	// can't reach the registry, restore the image and skip.
	_, _ = eng.cli.ImageRemove(ctx, rd, image.RemoveOptions{Force: true})
	if _, err := eng.Pull(ctx, ref, indexDigest, "linux/amd64", nil, nil, nopSink{}); err != nil {
		_, _ = eng.Pull(ctx, ref, "", "", nil, nil, nopSink{}) // best-effort restore
		t.Skipf("cannot reach a registry for the anchored platform pull: %v", err)
	}

	got, err := eng.cli.ImageInspect(ctx, rd)
	if err != nil {
		t.Fatalf("inspect anchored image: %v", err)
	}
	if !slices.Contains(got.RepoDigests, rd) {
		t.Errorf("anchored platform pull did not record the index digest %s; RepoDigests=%v", rd, got.RepoDigests)
	}
	t.Logf("platform-narrowed pull recorded index digest: %v", got.RepoDigests)
}
