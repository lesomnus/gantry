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
	j.Transfers = []*Transfer{{Store: "cache", Kind: "registry", BytesTotal: 100}}
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
