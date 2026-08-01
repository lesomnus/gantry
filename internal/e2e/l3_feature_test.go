//go:build e2e

// The L3 black-box tier for the routing features: the shipped `gantry serve`
// binary, configured the way an operator configures it — a YAML file — against
// real registries and the real docker daemon.
//
// The in-process tiers build config.Config in Go, so they never exercise the
// YAML surface the new fields actually ship with: `stores.<s>.cache`, its
// scoped `caches:` form, and the `worker.*` defaults. A field that parses
// nowhere is a feature that exists nowhere.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/gantry/pb"
)

func TestL3RoutedCopyFromYAMLConfig(t *testing.T) {
	cli := dockerClientOrSkip(t)
	daemonHost, needFwd := remoteDaemon()
	remote := startRegistryContainer(t, cli, daemonHost, needFwd)
	scoped := startRegistryContainer(t, cli, daemonHost, needFwd)
	cache := startRegistryContainer(t, cli, daemonHost, needFwd)
	cold := startRegistryContainer(t, cli, daemonHost, needFwd)
	seedImage(t, remote, "lib/app", "1")
	seedImage(t, scoped, "team/app", "1")
	seedImage(t, scoped, "lib/other", "1")

	bin := buildGantry(t)
	dir := t.TempDir()
	addr := "127.0.0.1:" + freePort(t)
	cfgPath := filepath.Join(dir, "gantry-e2e.yaml")
	// Both spellings of the route, and the worker defaults the branch adds. The
	// strict decoder rejects an unknown key, so this file failing to load IS the
	// assertion that the fields exist under these names.
	cfg := fmt.Sprintf(`serve:
  addr: %q
  events:
    path: %q
worker:
  fallback_to_origin: true
  source_wait: 45s
  admission_timeout: 10s
stores:
  remote: { kind: "oci", host: %q, insecure: true, cache: "cache" }
  scoped:
    kind: "oci"
    host: %q
    insecure: true
    caches:
      - store: "cache"
        for_repos: ["team/**"]
  cache: { kind: "oci", host: %q, insecure: true, mode: "copy" }
  cold: { kind: "oci", host: %q, insecure: true }
  edge: { kind: "docker", address: %q }
`, addr, filepath.Join(dir, "events.db"), remote, scoped, cache, cold, dockerAddr())
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, stop := runGantry(t, bin, cfgPath, addr)
	defer stop()

	pull := func(t *testing.T, ref, source string) *pb.Job {
		t.Helper()
		job, err := client.Job().Add(context.Background(), pb.JobAddRequest_builder{
			Ref:    ref,
			Source: pb.StoreByName(source),
			Target: pb.StoreByName("edge"),
		}.Build())
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		waitTerminal(t, client, job.GetId())
		got, err := client.Job().Get(context.Background(), pb.JobGetById(job.GetId()))
		if err != nil {
			t.Fatalf("job get: %v", err)
		}
		return got
	}

	t.Run("cache shorthand routes the pull", func(t *testing.T) {
		job := pull(t, remote+"/lib/app:1", "remote")
		tr := job.GetTransfers()
		if len(tr) != 2 {
			t.Fatalf("transfers = %d [%s], want a fill and a delivery", len(tr), describe(job))
		}
		if tr[0].GetStore() != "cache" || tr[0].GetSource() != "remote" {
			t.Errorf("hop 0 = %s, want cache ◀── remote", describe(job))
		}
		if tr[1].GetStore() != "edge" || tr[1].GetSource() != "cache" {
			t.Errorf("hop 1 = %s, want edge ◀── cache", describe(job))
		}
		if !hasTag(t, cache, "lib/app", "1") {
			t.Error("the cache does not hold the routed image")
		}
	})

	t.Run("caches list scopes the route", func(t *testing.T) {
		routed := pull(t, scoped+"/team/app:1", "scoped")
		if n := len(routed.GetTransfers()); n != 2 {
			t.Errorf("team/app transfers = %d [%s], want the scoped route to apply", n, describe(routed))
		}
		direct := pull(t, scoped+"/lib/other:1", "scoped")
		if n := len(direct.GetTransfers()); n != 1 {
			t.Errorf("lib/other transfers = %d [%s], want no route outside the scope", n, describe(direct))
		}
		if hasTag(t, cache, "lib/other", "1") {
			t.Error("a scoped route filled the cache for a repository it does not cover")
		}
	})

	t.Run("worker default supplies the fallback", func(t *testing.T) {
		// The job sets no fallback_to_origin; only `worker.fallback_to_origin: true`
		// in the file above can produce a second attempt.
		job := pull(t, remote+"/lib/app:1", "cold")
		if job.GetState() != pb.JobState_JOB_STATE_DONE {
			t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
		}
		tr := job.GetTransfers()
		if len(tr) != 2 {
			t.Fatalf("transfers = %d [%s], want the missed cold source and the origin fallback",
				len(tr), describe(job))
		}
		if tr[0].GetSource() != "cold" || tr[0].GetState() != pb.TransferState_TRANSFER_STATE_FAILED {
			t.Errorf("hop 0 = %s, want the cold source to have missed", describe(job))
		}
		if tr[1].GetSource() != "remote" || tr[1].GetState() != pb.TransferState_TRANSFER_STATE_DONE {
			t.Errorf("hop 1 = %s, want the origin to have served it", describe(job))
		}
	})
}
