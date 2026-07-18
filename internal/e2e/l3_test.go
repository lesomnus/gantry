//go:build e2e

// The L3 black-box tier (docs/e2e-plan.md): build and run the shipped `gantry
// serve` binary against a real registry, driving it over real gRPC. It proves
// the two things no in-process tier can — graceful shutdown on SIGTERM, and the
// audit log surviving a real process restart. Shared helpers are in
// l3_common_test.go.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lesomnus/gantry/pb"
)

func TestL3BlackBox(t *testing.T) {
	cli := dockerClientOrSkip(t)
	daemonHost, needFwd := remoteDaemon()
	remote := startRegistryContainer(t, cli, daemonHost, needFwd)
	cache := startRegistryContainer(t, cli, daemonHost, needFwd)
	seedImage(t, remote, "lib/app", "1")

	bin := buildGantry(t)
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.db")
	addr := "127.0.0.1:" + freePort(t)
	cfgPath := filepath.Join(dir, "gantry-e2e.yaml")
	cfg := fmt.Sprintf(`serve:
  addr: %q
  events:
    path: %q
stores:
  remote: { kind: "oci", host: %q, insecure: true }
  cache: { kind: "oci", host: %q, insecure: true, mode: "copy" }
`, addr, eventsPath, remote, cache)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// First run: copy an image so the audit log records a lifecycle.
	cmd, client, stop := runGantry(t, bin, cfgPath, addr)
	job, err := client.Job().Add(context.Background(), copyReq("remote", "cache"))
	if err != nil {
		stop()
		t.Fatalf("add: %v", err)
	}
	waitTerminal(t, client, job.GetId())

	events := listEvents(t, client)
	if events == 0 {
		stop()
		t.Fatal("no audit events after a copy")
	}

	// Graceful shutdown: SIGTERM must drain and exit cleanly within the window.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitExit(t, cmd, 15*time.Second)
	stop()

	// Second run on the SAME events.db: the audit history survives the restart,
	// while the in-memory job registry does not.
	_, client2, stop2 := runGantry(t, bin, cfgPath, addr)
	defer stop2()
	if got := listEvents(t, client2); got < events {
		t.Errorf("audit events after restart = %d, want >= %d (history must persist)", got, events)
	}
	jobs, err := client2.Job().List(context.Background(), &pb.JobListRequest{})
	if err != nil {
		t.Fatalf("job list: %v", err)
	}
	if n := len(jobs.GetItems()); n != 0 {
		t.Errorf("live job registry after restart = %d, want 0 (it is not durable)", n)
	}
}
