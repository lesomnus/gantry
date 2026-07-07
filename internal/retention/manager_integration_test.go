package retention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
)

func dockerAddr() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// TestManagerWatchPlanLive wires the manager to a real docker engine: the
// watcher should stamp the index when a container starts, and an in-use image
// must be protected by Plan while an unused throwaway tag is a delete candidate.
// Skips when no daemon is reachable.
func TestManagerWatchPlanLive(t *testing.T) {
	eng, err := down.New(config.StoreConfig{Name: "docker", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	// A raw client for fixture setup (run a container, make a throwaway tag).
	cli, err := client.NewClientWithOpts(client.WithHost(dockerAddr()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cli.Close()

	const ref = "busybox:latest"
	if err := eng.Pull(ctx, ref, "", "", nil, nopSink{}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	ix, err := Open(filepath.Join(t.TempDir(), "retn.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer ix.Close()

	m := mgr1("docker", eng, ix, Policy{MaxAge: time.Hour, KeepN: 0}, Schedule{Interval: time.Hour, MinInterval: time.Second})

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	m.StartWatchers(wctx)
	time.Sleep(500 * time.Millisecond) // let the watcher seed + attach

	// Start a busybox container; the watcher should record a usage stamp.
	const cname = "gantry-mgr-test"
	_ = cli.ContainerRemove(ctx, cname, container.RemoveOptions{Force: true})
	cc, err := cli.ContainerCreate(ctx,
		&container.Config{Image: ref, Cmd: []string{"sleep", "30"}}, nil, nil, nil, cname)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(ctx, cc.ID, container.RemoveOptions{Force: true})
	if err := cli.ContainerStart(ctx, cc.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The index should pick up the busybox usage within a few seconds.
	if !waitFor(10*time.Second, func() bool {
		recs, _ := ix.List("docker")
		for _, r := range recs {
			if strings.Contains(r.Ref, "busybox") && !r.effLastUsed().IsZero() {
				return true
			}
		}
		return false
	}) {
		recs, _ := ix.List("docker")
		t.Fatalf("watcher never stamped busybox; index = %+v", recs)
	}

	// A throwaway tag with no container is a delete candidate; the running
	// busybox image must be protected as in_use.
	const tmpTag = "gantry-mgr-test:tmp"
	if err := cli.ImageTag(ctx, ref, tmpTag); err != nil {
		t.Fatalf("tag: %v", err)
	}
	defer cli.ImageRemove(ctx, tmpTag, image.RemoveOptions{})
	// Stamp it old so age alone wouldn't save it, then make it a candidate.
	_ = ix.Touch("docker", tmpTag, time.Now().Add(-48*time.Hour))

	dec, err := m.Plan(ctx, "docker", nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if reasonOf(dec.Keep, "busybox:latest") != "in_use" {
		t.Errorf("busybox:latest not protected as in_use; keep=%+v", dec.Keep)
	}
	if !hasDelete(dec.Delete, tmpTag) {
		t.Errorf("stale %s should be a delete candidate; delete=%+v", tmpTag, dec.Delete)
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}

func reasonOf(keep []Kept, ref string) string {
	for _, k := range keep {
		if k.Ref == ref {
			return k.Reason
		}
	}
	return ""
}

func hasDelete(del []Candidate, ref string) bool {
	for _, c := range del {
		if c.Ref == ref {
			return true
		}
	}
	return false
}

type nopSink struct{}

func (nopSink) Layer(down.LayerUpdate) {}
