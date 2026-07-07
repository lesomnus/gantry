package rpc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TestWatchReadiness proves the overall serving status follows the gated
// stores, like the old /readyz.
func TestWatchReadiness(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.WatchReadiness(ctx, e.hs, 10*time.Millisecond)

	hc := healthpb.NewHealthClient(e.conn)
	await := func(want healthpb.HealthCheckResponse_ServingStatus) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			res, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
			if err == nil && res.GetStatus() == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("serving status never became %v", want)
	}

	await(healthpb.HealthCheckResponse_SERVING)
	e.eng.setReady(errors.New("daemon down"))
	await(healthpb.HealthCheckResponse_NOT_SERVING)
	e.eng.setReady(nil)
	await(healthpb.HealthCheckResponse_SERVING)
}
