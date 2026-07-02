package warm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/verify"
)

// fakeVerifier records the refs it sees and returns a canned result.
type fakeVerifier struct {
	dg    v1.Hash
	err   error
	calls []string
}

func (f *fakeVerifier) Verify(_ context.Context, _ config.StoreConfig, src name.Reference) (verify.Result, error) {
	f.calls = append(f.calls, src.Name())
	return verify.Result{Mode: config.VerifyRequire, Digest: f.dg}, f.err
}

// TestVerifyRejectsAdmission: a verification failure aborts Submit before any
// job is created (fail-closed), preserving the sentinel error for the handler.
func TestVerifyRejectsAdmission(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	fv := &fakeVerifier{err: fmt.Errorf("%w: up.example/app/x:1", verify.ErrUnsigned)}
	w.SetVerifier(fv)

	_, err := w.Submit(Request{Ref: "app/x:1", From: "up", To: "cache", Platforms: []string{"linux/amd64"}})
	if !errors.Is(err, verify.ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
	if items := js.List(Filter{}); len(items) != 0 {
		t.Errorf("job was created despite rejection: %d job(s)", len(items))
	}
	if len(fv.calls) != 1 {
		t.Errorf("verifier called %d times, want 1", len(fv.calls))
	}
}

// TestVerifyPinKeepsCacheTagged: a verified digest pins the source but the cache
// destination stays tag-named (so the cache remains pullable by tag).
func TestVerifyPinKeepsCacheTagged(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("a", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background() // enable Submit without starting workers

	snap, err := w.Submit(Request{Ref: "app/x:1", From: "up", To: "cache", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { js.Delete(snap.ID) })
	if len(snap.Transfers) == 0 {
		t.Fatal("no transfers")
	}
	cacheRef := snap.Transfers[0].Ref
	if strings.Contains(cacheRef, "@sha256:") || !strings.Contains(cacheRef, ":1") {
		t.Errorf("cache ref = %q, want tag-named (not pinned to the source digest)", cacheRef)
	}
	if snap.Verification == nil || !snap.Verification.Verified ||
		snap.Verification.Digest != h.String() || snap.Verification.Mode != string(config.VerifyRequire) {
		t.Errorf("verification = %+v, want the verified digest surfaced on the snapshot", snap.Verification)
	}
}

// TestVerifyProxyModeRejected: a verified digest cannot be honored by a proxy
// destination (reads through by tag), so the job is refused fail-closed.
func TestVerifyProxyModeRejected(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "proxy"},
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("c", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background()

	_, err := w.Submit(Request{Ref: "app/x:1", From: "up", To: "cache", Platforms: []string{"linux/amd64"}})
	if err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("err = %v, want a proxy-mode rejection", err)
	}
	if items := js.List(Filter{}); len(items) != 0 {
		t.Errorf("job created despite proxy rejection: %d", len(items))
	}
}

// TestVerifyPinsDistributeRef: with no cache, a verified digest pins the ref the
// distribute engine is told to pull (so it fetches exactly what was verified).
func TestVerifyPinsDistributeRef(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "eng", Kind: "docker", Address: "tcp://127.0.0.1:1"}, // lazy; not dialed by plan
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("b", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background() // enable Submit without starting workers

	snap, err := w.Submit(Request{Ref: "app/x:1", From: "up", Distribute: []string{"eng"}, Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { js.Delete(snap.ID) })
	if len(snap.Transfers) == 0 {
		t.Fatal("no transfers")
	}
	ref := snap.Transfers[0].Ref
	if !strings.Contains(ref, "@sha256:"+strings.Repeat("b", 64)) {
		t.Errorf("distribute ref = %q, want pinned to the verified digest", ref)
	}
}
