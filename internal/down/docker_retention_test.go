package down

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/lesomnus/gantry/cmd/config"
)

// TestDockerRetentionLive exercises the retention surface (InUse, SeedUsage,
// WatchUsage, Remove) against a real daemon. It uses busybox (a small, usually
// already-present image) and a throwaway tag so nothing shared is destroyed.
// Skips when no daemon is reachable.
func TestDockerRetentionLive(t *testing.T) {
	eng, err := newDockerEngine(config.StoreConfig{Name: "live", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	const ref = "busybox:latest"
	if err := eng.Pull(ctx, ref, nopSink{}); err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}

	// Subscribe to usage events before starting the container so the start is caught.
	var (
		mu   sync.Mutex
		seen []string
	)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- eng.WatchUsage(wctx, func(r string, _ time.Time) {
			mu.Lock()
			seen = append(seen, r)
			mu.Unlock()
		})
	}()
	time.Sleep(500 * time.Millisecond) // let the event stream attach

	const cname = "gantry-retn-test"
	_ = eng.cli.ContainerRemove(ctx, cname, container.RemoveOptions{Force: true}) // clear any stale
	cc, err := eng.cli.ContainerCreate(ctx,
		&container.Config{Image: ref, Cmd: []string{"sleep", "30"}},
		nil, nil, nil, cname)
	if err != nil {
		t.Fatalf("container create: %v", err)
	}
	defer eng.cli.ContainerRemove(ctx, cc.ID, container.RemoveOptions{Force: true})
	if err := eng.cli.ContainerStart(ctx, cc.ID, container.StartOptions{}); err != nil {
		t.Fatalf("container start: %v", err)
	}

	// WatchUsage should have reported the busybox-backed start.
	deadline := time.After(15 * time.Second)
	for {
		mu.Lock()
		hit := containsImage(seen, "busybox")
		mu.Unlock()
		if hit {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := append([]string(nil), seen...)
			mu.Unlock()
			t.Fatalf("WatchUsage never reported busybox start; saw %v", got)
		case <-time.After(250 * time.Millisecond):
		}
	}

	// InUse reports running images.
	inuse, err := eng.InUse(ctx)
	if err != nil {
		t.Fatalf("InUse: %v", err)
	}
	if !mapHasImage(inuse, "busybox") {
		t.Errorf("InUse = %v, want a busybox entry", inuse)
	}

	// SeedUsage reports the container's image (container exists, running or not).
	var seedHit bool
	if err := eng.SeedUsage(ctx, func(r string, _ time.Time) {
		if strings.Contains(r, "busybox") {
			seedHit = true
		}
	}); err != nil {
		t.Fatalf("SeedUsage: %v", err)
	}
	if !seedHit {
		t.Error("SeedUsage did not report busybox")
	}

	// Remove via a throwaway tag: untags without deleting the shared image.
	const tmpTag = "gantry-retn-test:tmp"
	if err := eng.cli.ImageTag(ctx, ref, tmpTag); err != nil {
		t.Fatalf("image tag: %v", err)
	}
	rr, err := eng.Remove(ctx, tmpTag)
	if err != nil {
		// Clean up the tag we created before failing.
		_, _ = eng.cli.ImageRemove(ctx, tmpTag, image.RemoveOptions{})
		t.Fatalf("Remove: %v", err)
	}
	if len(rr.Untagged) == 0 {
		t.Errorf("Remove(%s) reported no untag: %+v", tmpTag, rr)
	}

	wcancel()
	<-watchErr // WatchUsage returns ctx.Err on cancel
}

func containsImage(refs []string, want string) bool {
	for _, r := range refs {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

func mapHasImage(m map[string]bool, want string) bool {
	for k := range m {
		if strings.Contains(k, want) {
			return true
		}
	}
	return false
}
