package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

// newJobServer builds a handler whose warmer never actually runs jobs (its
// context is pre-canceled), so submissions land as records without moving bytes.
func newJobServer(t *testing.T) (http.Handler, *warm.Warmer, warm.Store) {
	t.Helper()
	var c config.Config
	c.Serve.AllowUnknownStores = true
	c.Serve.Stores = map[string]config.StoreConfig{
		"cache": {Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Serve.Stores, c.Serve.AllowUnknownStores)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { set.Close() })
	js := warm.NewMemStore()
	wmr := warm.NewWarmer(set, js, c.Serve.Warm)
	// Prepare the warmer without workers: submissions enqueue as pending records
	// the test drives deterministically, with nothing racing their state.
	wmr.SetBaseContext(context.Background())
	return New(wmr, js, set, nil, health.NewChecker(set, health.Options{}), nil, nil), wmr, js
}

func TestJobPlanDryRun(t *testing.T) {
	h, _, js := newJobServer(t)
	rr := do(t, h, http.MethodPost, "/v1/job/plan", `{"ref":"team/app:1","from":"docker.io","to":"cache"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
	}
	var plan warm.PlanResult
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.To != "cache" || plan.DstRef == "" || plan.SrcRef == "" {
		t.Errorf("plan = %+v, want resolved from/to/refs", plan)
	}
	// A dry-run must not create a job.
	if items := js.List(warm.Filter{}); len(items) != 0 {
		t.Errorf("plan created a job: %d", len(items))
	}
	// A bad rewrite surfaces the tried patterns, not an opaque error.
	rr = do(t, h, http.MethodPost, "/v1/job/plan", `{"ref":"team/app:1"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("plan with nothing to do = %d, want 400", rr.Code)
	}
}

func TestCreateJobCoalescedStatus(t *testing.T) {
	h, _, _ := newJobServer(t)
	body := `{"ref":"team/app:1","from":"docker.io","to":"cache","platforms":["linux/amd64"]}`
	rr := do(t, h, http.MethodPost, "/v1/job", body)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first submit = %d, want 202 (%s)", rr.Code, rr.Body)
	}
	var first struct {
		ID        string `json:"id"`
		Coalesced bool   `json:"coalesced"`
	}
	json.Unmarshal(rr.Body.Bytes(), &first)
	if first.Coalesced {
		t.Error("first submit must not be coalesced")
	}
	rr = do(t, h, http.MethodPost, "/v1/job", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("coalesced submit = %d, want 200", rr.Code)
	}
	var second struct {
		ID        string `json:"id"`
		Coalesced bool   `json:"coalesced"`
	}
	json.Unmarshal(rr.Body.Bytes(), &second)
	if !second.Coalesced || second.ID != first.ID {
		t.Errorf("second = %+v, want coalesced onto %s", second, first.ID)
	}
}

func TestCreateJobIdempotencyKey(t *testing.T) {
	h, _, js := newJobServer(t)
	body := `{"ref":"team/app:2","from":"docker.io","to":"cache","platforms":["linux/amd64"]}`
	post := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/job", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "abc-123")
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := post()
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first = %d", rr.Code)
	}
	var first struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &first)

	// Drive the first job to a terminal state so dedup no longer covers it; the
	// idempotency key must still return the same job, not run a second move.
	js.Update(first.ID, func(j *warm.Job) { j.State = warm.JobDone })
	rr = post()
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent replay = %d, want 200", rr.Code)
	}
	var second struct {
		ID        string `json:"id"`
		Coalesced bool   `json:"coalesced"`
	}
	json.Unmarshal(rr.Body.Bytes(), &second)
	if second.ID != first.ID || !second.Coalesced {
		t.Errorf("replay = %+v, want the same job %s", second, first.ID)
	}
	if items := js.List(warm.Filter{}); len(items) != 1 {
		t.Errorf("idempotent replay created a second job: %d", len(items))
	}
}

// A keyed submit that COALESCES onto an active identical move must still record
// the key, so a later replay maps back to the same job instead of re-running.
func TestIdempotencyKeyOnCoalescedSubmit(t *testing.T) {
	h, _, js := newJobServer(t)
	body := `{"ref":"team/app:9","from":"docker.io","to":"cache","platforms":["linux/amd64"]}`
	keyed := func(key string) string {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/job", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		h.ServeHTTP(rr, req)
		var res struct {
			ID string `json:"id"`
		}
		json.Unmarshal(rr.Body.Bytes(), &res)
		return res.ID
	}
	first := keyed("k-first")   // creates the job
	second := keyed("k-second") // coalesces onto it (created=false)
	if second != first {
		t.Fatalf("second submit = %s, want coalesced onto %s", second, first)
	}
	// The coalesced-onto job finishes; a replay of the SECOND key must return the
	// same job, not launch a duplicate move.
	js.Update(first, func(j *warm.Job) { j.State = warm.JobDone })
	replay := keyed("k-second")
	if replay != first {
		t.Errorf("replay of the coalesced key = %s, want %s", replay, first)
	}
	if items := js.List(warm.Filter{}); len(items) != 1 {
		t.Errorf("coalesced-key replay created a duplicate: %d jobs", len(items))
	}
}

func TestListJobsControls(t *testing.T) {
	h, _, _ := newJobServer(t)
	t.Run("unknown state is 400", func(t *testing.T) {
		if rr := do(t, h, http.MethodGet, "/v1/job?state=complete", ""); rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
	t.Run("known state is 200", func(t *testing.T) {
		if rr := do(t, h, http.MethodGet, "/v1/job?state=done", ""); rr.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", rr.Code)
		}
	})
	t.Run("bad limit is 400", func(t *testing.T) {
		if rr := do(t, h, http.MethodGet, "/v1/job?limit=-1", ""); rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
	t.Run("bad since is 400", func(t *testing.T) {
		if rr := do(t, h, http.MethodGet, "/v1/job?since=yesterday", ""); rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
}

func TestCancelKeepsRecord(t *testing.T) {
	h, _, js := newJobServer(t)
	rr := do(t, h, http.MethodPost, "/v1/job", `{"ref":"team/app:3","from":"docker.io","to":"cache","platforms":["linux/amd64"]}`)
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &created)
	// Force a non-terminal state so cancel has something to act on.
	js.Update(created.ID, func(j *warm.Job) { j.State = warm.JobRunning })

	rr = do(t, h, http.MethodPost, "/v1/job/"+created.ID+"/cancel", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d, body = %s", rr.Code, rr.Body)
	}
	// The record survives (unlike DELETE) and is still gettable.
	if rr := do(t, h, http.MethodGet, "/v1/job/"+created.ID, ""); rr.Code != http.StatusOK {
		t.Errorf("job record gone after cancel: %d", rr.Code)
	}
	if rr := do(t, h, http.MethodPost, "/v1/job/nope/cancel", ""); rr.Code != http.StatusNotFound {
		t.Errorf("cancel unknown = %d, want 404", rr.Code)
	}
}

func TestRetryTerminalOnly(t *testing.T) {
	h, _, js := newJobServer(t)
	rr := do(t, h, http.MethodPost, "/v1/job", `{"ref":"team/app:4","from":"docker.io","to":"cache","platforms":["linux/amd64"]}`)
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &created)

	// A non-terminal job cannot be retried.
	js.Update(created.ID, func(j *warm.Job) { j.State = warm.JobRunning })
	if rr := do(t, h, http.MethodPost, "/v1/job/"+created.ID+"/retry", ""); rr.Code != http.StatusConflict {
		t.Errorf("retry of a running job = %d, want 409", rr.Code)
	}
	// Once terminal, retry re-submits the original request as a new job.
	js.Update(created.ID, func(j *warm.Job) { j.State = warm.JobFailed })
	rr = do(t, h, http.MethodPost, "/v1/job/"+created.ID+"/retry", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("retry = %d, body = %s", rr.Code, rr.Body)
	}
	var retried struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &retried)
	if retried.ID == "" || retried.ID == created.ID {
		t.Errorf("retry id = %q, want a fresh job", retried.ID)
	}
	if rr := do(t, h, http.MethodPost, "/v1/job/nope/retry", ""); rr.Code != http.StatusNotFound {
		t.Errorf("retry unknown = %d, want 404", rr.Code)
	}
}
