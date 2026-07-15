package warm

import (
	"sync"
	"testing"
	"time"
)

func mkJob(id, ref string, platforms []string) *Job {
	j := NewJob(id, ref, platforms, time.Now())
	j.dedup = dedupKey(ref, platforms, "", "", nil)
	return j
}

func TestStoreAddGetSnapshot(t *testing.T) {
	s := NewMemStore()
	j := mkJob("a", "docker.io/library/redis:7", []string{"linux/amd64"})
	j.Transfers = []*Transfer{{Store: "cache", Kind: "oci", BytesTotal: 100}}
	if err := s.Add(j); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.Add(j); err == nil {
		t.Error("duplicate add should error")
	}
	snap, ok := s.Snapshot("a")
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snap.Ref != "docker.io/library/redis:7" || len(snap.Transfers) != 1 || snap.Transfers[0].BytesTotal != 100 {
		t.Errorf("snapshot = %+v", snap)
	}
	if _, ok := s.Snapshot("nope"); ok {
		t.Error("missing job reported present")
	}
}

func TestStoreUpdateAndAtomics(t *testing.T) {
	s := NewMemStore()
	j := mkJob("a", "img:1", nil)
	j.Transfers = []*Transfer{{Store: "cache", Layers: []*LayerProgress{{Digest: "sha256:x", Total: 50}}}}
	_ = s.Add(j)
	live, _ := s.Job("a")
	live.Transfers[0].BytesDone.Add(25)
	live.Transfers[0].Layers[0].Done.Add(25)
	s.Update("a", func(j *Job) {
		j.State = JobRunning
		j.Transfers[0].Layers[0].State = "pulling"
	})
	snap, _ := s.Snapshot("a")
	if snap.State != JobRunning || snap.Transfers[0].BytesDone != 25 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.Transfers[0].Layers[0].Done != 25 || snap.Transfers[0].Layers[0].State != "pulling" {
		t.Errorf("layer = %+v", snap.Transfers[0].Layers[0])
	}
}

func TestStoreActiveDedup(t *testing.T) {
	s := NewMemStore()
	a := mkJob("a", "img:1", []string{"linux/arm64", "linux/amd64"})
	_ = s.Add(a)
	key := dedupKey("img:1", []string{"linux/amd64", "linux/arm64"}, "", "", nil)
	if _, ok := s.Active(key); !ok {
		t.Error("active job not found by dedup key")
	}
	s.Update("a", func(j *Job) { j.State = JobDone })
	if _, ok := s.Active(key); ok {
		t.Error("terminal job should not be active")
	}
}

func TestStoreListFilter(t *testing.T) {
	s := NewMemStore()
	_ = s.Add(mkJob("a", "docker.io/redis:7", nil))
	_ = s.Add(mkJob("b", "ghcr.io/app:1", nil))
	s.Update("a", func(j *Job) { j.State = JobDone })
	if got := s.List(Filter{}); len(got) != 2 {
		t.Errorf("list all = %d, want 2", len(got))
	}
	if got := s.List(Filter{State: JobDone}); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("list by state = %+v", got)
	}
	if got := s.List(Filter{Ref: "ghcr"}); len(got) != 1 || got[0].ID != "b" {
		t.Errorf("list by ref = %+v", got)
	}
}

func TestStoreCoalesceKeepsLabels(t *testing.T) {
	s := NewMemStore()
	a := mkJob("a", "img:1", []string{"linux/amd64"})
	a.Labels = map[string]string{"team": "x"}
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	snap, ok := s.Attach(a.dedup, "h2", map[string]string{"team": "y"}, time.Now())
	if !ok {
		t.Fatal("attach onto active job failed")
	}
	if snap.ID != "h2" || snap.Labels["team"] != "y" {
		t.Errorf("attach snapshot = %+v", snap)
	}
	// Both handles resolve to the one execution, each with its own labels.
	if p, _ := s.Snapshot("a"); p.Labels["team"] != "x" {
		t.Errorf("primary handle labels = %v", p.Labels)
	}
	if got := s.List(Filter{}); len(got) != 2 {
		t.Errorf("list = %d, want 2 handles", len(got))
	}
	if total := countTotal(s.Counts()); total != 1 {
		t.Errorf("executions = %d, want 1", total)
	}
	// A dead execution is not a coalescing target.
	s.Update("a", func(j *Job) { j.State = JobDone })
	if _, ok := s.Attach(a.dedup, "h3", nil, time.Now()); ok {
		t.Error("attached onto a terminal execution")
	}
}

func TestStoreCancelDetachesSharedExecution(t *testing.T) {
	s := NewMemStore()
	canceled := false
	a := mkJob("a", "img:1", nil)
	a.SetCancel(func() { canceled = true })
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Attach(a.dedup, "h2", nil, time.Now()); !ok {
		t.Fatal("attach failed")
	}
	// One caller canceling must not stop work another still wants.
	snap, ok, already := s.Cancel("h2")
	if !ok || already {
		t.Fatalf("cancel h2 = ok:%v already:%v", ok, already)
	}
	if snap.State != JobCanceled {
		t.Errorf("canceled handle state = %q, want canceled", snap.State)
	}
	if canceled {
		t.Error("shared work stopped while another caller still holds it")
	}
	if p, _ := s.Snapshot("a"); p.State == JobCanceled {
		t.Error("the other handle wrongly reads canceled")
	}
	// Re-canceling the same handle is a precondition failure, not a re-signal.
	if _, _, already := s.Cancel("h2"); !already {
		t.Error("second cancel should report already canceled")
	}
	// The last caller canceling stops the execution, keeping the record.
	if _, ok, already := s.Cancel("a"); !ok || already {
		t.Fatalf("cancel primary = ok:%v already:%v", ok, already)
	}
	if !canceled {
		t.Error("last caller cancel did not stop the shared work")
	}
	if _, ok := s.Snapshot("a"); !ok {
		t.Error("canceled record should be kept for inspection")
	}
}

func TestStoreEraseHandleKeepsSharedExecution(t *testing.T) {
	s := NewMemStore()
	canceled := false
	a := mkJob("a", "img:1", nil)
	a.SetCancel(func() { canceled = true })
	_ = s.Add(a)
	if _, ok := s.Attach(a.dedup, "h2", nil, time.Now()); !ok {
		t.Fatal("attach failed")
	}
	// Erasing one handle leaves the shared execution for the other.
	if !s.Delete("h2") {
		t.Fatal("delete h2 returned false")
	}
	if canceled {
		t.Error("execution canceled while a handle remains")
	}
	if _, ok := s.Snapshot("a"); !ok {
		t.Error("shared execution wrongly evicted")
	}
	// Erasing the last handle evicts and cancels the execution.
	if !s.Delete("a") {
		t.Fatal("delete a returned false")
	}
	if !canceled {
		t.Error("last handle erase did not cancel the execution")
	}
	if _, ok := s.Snapshot("a"); ok {
		t.Error("execution not evicted after its last handle was erased")
	}
}

func TestStoreListLabelFilter(t *testing.T) {
	s := NewMemStore()
	a := mkJob("a", "img:1", nil)
	a.Labels = map[string]string{"team": "x", "env": "prod"}
	b := mkJob("b", "img:2", nil)
	b.Labels = map[string]string{"team": "y", "env": "prod"}
	_ = s.Add(a)
	_ = s.Add(b)
	if got := s.List(Filter{Labels: map[string]string{"env": "prod"}}); len(got) != 2 {
		t.Errorf("env=prod = %d, want 2", len(got))
	}
	if got := s.List(Filter{Labels: map[string]string{"team": "x"}}); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("team=x = %+v", got)
	}
	if got := s.List(Filter{Labels: map[string]string{"team": "x", "env": "prod"}}); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("subset both = %+v", got)
	}
	if got := s.List(Filter{Labels: map[string]string{"team": "x", "env": "stage"}}); len(got) != 0 {
		t.Errorf("non-matching value = %+v", got)
	}
}

func TestStoreErasedHandleDoesNotResurrect(t *testing.T) {
	s := NewMemStore()
	a := mkJob("a", "img:1", nil)
	_ = s.Add(a)
	if _, ok := s.Attach(a.dedup, "h2", nil, time.Now()); !ok {
		t.Fatal("attach failed")
	}
	// Erasing the primary handle leaves the shared execution alive for h2, but
	// the erased id must read as gone rather than resolve back to the execution.
	if !s.Delete("a") {
		t.Fatal("delete a returned false")
	}
	if _, ok := s.Snapshot("a"); ok {
		t.Error("erased handle id resurrected the surviving execution")
	}
	if _, ok := s.Snapshot("h2"); !ok {
		t.Error("surviving handle lost")
	}
	// The worker still addresses the execution by its id through Update.
	if !s.Update("a", func(j *Job) { j.State = JobRunning }) {
		t.Error("execution not updatable by its id after the primary handle was erased")
	}
	if snap, _ := s.Snapshot("h2"); snap.State != JobRunning {
		t.Errorf("update via execution id not seen by surviving handle: %q", snap.State)
	}
}

func TestStoreEnqueuingJobNotCoalesceable(t *testing.T) {
	s := NewMemStore()
	a := mkJob("a", "img:1", nil)
	a.enqueuing = true // mid-enqueue: published but not yet on the run queue
	_ = s.Add(a)
	if _, ok := s.Attach(a.dedup, "h2", nil, time.Now()); ok {
		t.Error("coalesced onto a job that is still enqueuing")
	}
	if _, ok := s.Active(a.dedup); ok {
		t.Error("enqueuing job reported active")
	}
	// Once queued it becomes a coalescing target.
	s.Update("a", func(j *Job) { j.enqueuing = false })
	if _, ok := s.Attach(a.dedup, "h2", nil, time.Now()); !ok {
		t.Error("job not coalesceable after enqueue was confirmed")
	}
}

func countTotal(m map[JobState]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

func TestStoreDeleteCancels(t *testing.T) {
	s := NewMemStore()
	j := mkJob("a", "img:1", nil)
	canceled := false
	j.SetCancel(func() { canceled = true })
	_ = s.Add(j)
	if !s.Delete("a") {
		t.Fatal("delete returned false")
	}
	if !canceled {
		t.Error("delete did not cancel the job")
	}
	if s.Delete("a") {
		t.Error("second delete should return false")
	}
}

func TestStoreSweep(t *testing.T) {
	s := NewMemStore()
	now := time.Now()
	old := mkJob("old", "img:1", nil)
	old.State = JobDone
	old.EndedAt = now.Add(-time.Hour)
	_ = s.Add(old)
	fresh := mkJob("fresh", "img:2", nil)
	fresh.State = JobDone
	fresh.EndedAt = now
	_ = s.Add(fresh)
	active := mkJob("active", "img:3", nil)
	active.State = JobRunning
	_ = s.Add(active)
	if n := s.Sweep(now, 30*time.Minute); n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	if _, ok := s.Snapshot("old"); ok {
		t.Error("old job not evicted")
	}
	if _, ok := s.Snapshot("fresh"); !ok {
		t.Error("fresh job wrongly evicted")
	}
	if _, ok := s.Snapshot("active"); !ok {
		t.Error("active job wrongly evicted")
	}
}

func TestStoreConcurrentProgress(t *testing.T) {
	s := NewMemStore()
	j := mkJob("a", "img:1", nil)
	j.Transfers = []*Transfer{{Store: "cache", Layers: []*LayerProgress{{Digest: "sha256:x", Total: 1000}}}}
	_ = s.Add(j)
	live, _ := s.Job("a")
	tr := live.Transfers[0]
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 1000; k++ {
				tr.BytesDone.Add(1)
				tr.Layers[0].Done.Add(1)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 1000; k++ {
				_, _ = s.Snapshot("a")
			}
		}()
	}
	wg.Wait()
	snap, _ := s.Snapshot("a")
	if snap.Transfers[0].BytesDone != 8000 || snap.Transfers[0].Layers[0].Done != 8000 {
		t.Errorf("bytes_done = %d / layer = %d, want 8000", snap.Transfers[0].BytesDone, snap.Transfers[0].Layers[0].Done)
	}
}
