package event

import (
	"path/filepath"
	"testing"
)

func TestLogGet(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "ev.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.Append(Event{Type: ImagePulled, Ref: "a:1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: ImageRemove, Ref: "a:1"}); err != nil {
		t.Fatal(err)
	}

	e, ok, err := l.Get(2)
	if err != nil || !ok || e.Type != ImageRemove || e.Seq != 2 {
		t.Fatalf("get: %+v %v %v", e, ok, err)
	}
	if _, ok, err := l.Get(99); err != nil || ok {
		t.Fatalf("missing seq must be a miss, got ok=%v err=%v", ok, err)
	}

	// Ring eviction turns old sequences into misses.
	for range 10 {
		if err := l.Append(Event{Type: ImagePulled, Ref: "b:1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := l.Get(1); ok {
		t.Error("seq 1 must be evicted")
	}
	if _, ok, _ := l.Get(2); ok {
		t.Error("seq 2 must be evicted")
	}
	if e, ok, _ := l.Get(12); !ok || e.Ref != "b:1" {
		t.Errorf("newest must survive: %+v %v", e, ok)
	}
}
