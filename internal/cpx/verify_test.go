package cpx

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
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	fv := &fakeVerifier{err: fmt.Errorf("%w: up.example/app/x:1", verify.ErrUnsigned)}
	w.SetVerifier(fv)

	_, _, err := w.Submit(Request{Ref: "app/x:1", Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
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
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("a", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background() // enable Submit without starting workers

	snap, _, err := w.Submit(Request{Ref: "app/x:1", Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
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
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "proxy"},
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("c", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background()

	_, _, err := w.Submit(Request{Ref: "app/x:1", Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
	if err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("err = %v, want a proxy-mode rejection", err)
	}
	if items := js.List(Filter{}); len(items) != 0 {
		t.Errorf("job created despite proxy rejection: %d", len(items))
	}
}

// TestVerifyPinsSourceRef: a verified digest pins the source ref the copy pulls,
// so the cache is filled from exactly what was verified (surfaced as Plan.SrcRef).
func TestVerifyPinsSourceRef(t *testing.T) {
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, false)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("b", 64))
	w.SetVerifier(&fakeVerifier{dg: h})
	w.base = context.Background()

	res, err := w.Plan(context.Background(), Request{Ref: "app/x:1", Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SourceRef, "@sha256:"+strings.Repeat("b", 64)) {
		t.Errorf("src ref = %q, want pinned to the verified digest", res.SourceRef)
	}
}
