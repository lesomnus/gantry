package rpc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/pb"
)

// The store kinds are declared once, on the field that carries them out over
// REST, and the two conversion maps are maintained by hand. A kind added to the
// declaration and missed here does not fail anything at build time — it makes
// the map return its zero value, and the store goes out over gRPC as
// STORE_KIND_UNSPECIFIED with nothing said. That is how `meta` lost its kind.
func TestEveryStoreKindConverts(t *testing.T) {
	f, ok := reflect.TypeOf(store.Status{}).FieldByName("Kind")
	if !ok {
		t.Fatal("store.Status has no Kind field")
	}
	kinds := strings.Split(f.Tag.Get("enums"), ",")
	if len(kinds) < 4 {
		t.Fatalf("expected the declaration to list every kind; got %v", kinds)
	}
	for _, kind := range kinds {
		k, ok := storeKindToPB[kind]
		if !ok {
			t.Errorf("kind %q is declared but storeKindToPB does not know it", kind)
			continue
		}
		if k == pb.StoreKind_STORE_KIND_UNSPECIFIED {
			t.Errorf("kind %q converts to UNSPECIFIED", kind)
		}
		if back, ok := storeKindFromPB[k]; !ok || back != kind {
			t.Errorf("kind %q does not round-trip: got %q (known=%v)", kind, back, ok)
		}
	}
	if len(storeKindToPB) != len(kinds) {
		t.Errorf("storeKindToPB has %d entries for %d declared kinds", len(storeKindToPB), len(kinds))
	}
	if len(storeKindFromPB) != len(kinds) {
		t.Errorf("storeKindFromPB has %d entries for %d declared kinds", len(storeKindFromPB), len(kinds))
	}
}

// A meta store is a legitimate row of StoreService.List — it is a source a job
// can name — so it must arrive with its kind, not as an unspecified store the
// client then has to guess about.
func TestMetaStoreKeepsItsKind(t *testing.T) {
	got := statusToPB(store.Status{Name: "remote", Kind: "meta", Ready: true})
	if got.GetKind() != pb.StoreKind_STORE_KIND_META {
		t.Errorf("meta store went out as %v", got.GetKind())
	}
}
