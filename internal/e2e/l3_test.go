//go:build e2e

// The L3 black-box tier (docs/e2e-plan.md): build and run the shipped `gantry
// serve` binary against a real registry, driving it over real gRPC. It proves
// the two things no in-process tier can — graceful shutdown on SIGTERM, and the
// audit log surviving a real process restart.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func buildGantry(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "gantry")
	cmd := exec.Command("go", "build", "-o", out, "github.com/lesomnus/gantry")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gantry: %v\n%s", err, b)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// runGantry starts `gantry serve` and returns the process plus a connected
// client, waiting until the health service reports readiness.
func runGantry(t *testing.T, bin, cfgPath, addr string) (*exec.Cmd, pb.Client, func()) {
	t.Helper()
	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gantry: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Wait for the process to be serving (health responds).
	hc := grpc_health_v1.NewHealthClient(conn)
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("gantry never became reachable at %s: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop := func() {
		conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return cmd, pb.NewClient(conn), stop
}

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

func waitTerminal(t *testing.T, client pb.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		job, err := client.Job().Get(context.Background(), pb.JobGetById(id))
		if err != nil {
			t.Fatalf("job get: %v", err)
		}
		switch job.GetState() {
		case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_CANCELED:
			if job.GetState() != pb.JobState_JOB_STATE_DONE {
				t.Fatalf("job state=%v error=%q", job.GetState(), job.GetError())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not terminate", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func listEvents(t *testing.T, client pb.Client) int {
	t.Helper()
	res, err := client.Event().List(context.Background(), &pb.EventListRequest{})
	if err != nil {
		t.Fatalf("event list: %v", err)
	}
	return len(res.GetItems())
}

func waitExit(t *testing.T, cmd *exec.Cmd, within time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(within):
		_ = cmd.Process.Kill()
		t.Fatalf("gantry did not shut down within %s of SIGTERM", within)
	}
}
