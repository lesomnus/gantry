package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
)

type rejectVerifier struct{}

func (rejectVerifier) Verify(context.Context, config.StoreConfig, name.Reference) (v1.Hash, error) {
	return v1.Hash{}, fmt.Errorf("%w: up.example/app/x:1", verify.ErrUnsigned)
}

// TestCreateJobVerifyRejected: a source-signature failure returns 422 and no job
// is created.
func TestCreateJobVerifyRejected(t *testing.T) {
	var c config.Config
	c.Serve.AllowUnknownStores = true
	c.Serve.Stores = map[string]config.StoreConfig{
		"up":    {Kind: "oci", Host: "up.example", Insecure: true},
		"cache": {Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Serve.Stores, c.Serve.AllowUnknownStores)
	if err != nil {
		t.Fatal(err)
	}
	js := warm.NewMemStore()
	wmr := warm.NewWarmer(set, js, c.Serve.Warm)
	wmr.SetVerifier(rejectVerifier{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wmr.Start(ctx)
	t.Cleanup(wmr.Stop)
	h := New(wmr, js, set, nil, health.NewChecker(set, health.Options{}))

	rr := httptest.NewRecorder()
	body := `{"ref":"app/x:1","from":"up","to":"cache","platforms":["linux/amd64"]}`
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(body)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422 (%s)", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job", nil))
	var resp struct {
		Items []warm.JobSnapshot `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("a job was created despite verification failure: %+v", resp.Items)
	}
}
