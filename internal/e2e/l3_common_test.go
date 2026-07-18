//go:build e2e || e2e_infra

// Shared black-box helpers for the L3 tiers: build and run the shipped `gantry
// serve` binary and drive it over real gRPC.
package e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// buildGantry returns a path to a `gantry` binary to black-box. When
// GANTRY_E2E_BIN points at a prebuilt binary it is reused verbatim — CI sets it
// to the release binary that `bake build` drops in ./dist/<arch>, so the exact
// stripped, trimmed artifact is exercised and nothing is compiled twice. When
// unset (local dev) it falls back to `go build`.
func buildGantry(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("GANTRY_E2E_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("GANTRY_E2E_BIN=%q not usable: %v", bin, err)
		}
		return bin
	}
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
