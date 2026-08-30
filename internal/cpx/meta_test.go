package cpx

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
)

// metaCopier builds the shape a meta store is for: two registries that hold
// DIFFERENT things — a CDN with the released repositories and an internal
// registry with everything else — behind one name that jobs point at.
//
// Separate registries and not one with two repos, because the whole claim under
// test is that the bytes come off the registry the route names.
func metaCopier(t *testing.T) (w *Copier, js Store, cdn, internal, local string) {
	t.Helper()
	cdn, internal, local = startRegistry(t), startRegistry(t), startRegistry(t)
	w, js = newCopier(t, []config.StoreConfig{
		{Name: "remote", Kind: "meta", Routes: []config.Route{
			{ForRepos: []string{"dist/**"}, Store: "cdn"},
			{Store: "internal"},
		}},
		{Name: "cdn", Kind: "oci", Host: cdn, Insecure: true},
		{Name: "internal", Kind: "oci", Host: internal, Insecure: true},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	return w, js, cdn, internal, local
}

// One source name, two repositories, two registries. The job says `remote` both
// times and the bytes come off whichever one the route names — which is the
// feature.
func TestMetaSourceRoutesByRepository(t *testing.T) {
	w, js, cdn, internal, local := metaCopier(t)
	pushImage(t, cdn+"/dist/hday/cove:v1", 2)
	pushImage(t, internal+"/stage/hday/cove:v1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	for _, tc := range []struct{ repo, from string }{
		{"dist/hday/cove", "cdn"},
		{"stage/hday/cove", "internal"},
	} {
		// The ref names the meta store's own host -- which does not exist. Only
		// the route decides where this is read from, so a host that could answer
		// would hide a resolution that never happened.
		snap, _, err := w.Submit(Request{Ref: "cr.invalid/" + tc.repo + ":v1", Source: "remote", Target: "local"})
		if err != nil {
			t.Fatalf("submit %s: %v", tc.repo, err)
		}
		done := waitTerminal(t, js, snap.ID)
		if done.State != JobDone {
			t.Fatalf("%s: state = %q (err=%q)", tc.repo, done.State, done.Err)
		}
		if len(done.Transfers) != 1 {
			t.Fatalf("%s: transfers = %+v, want one hop", tc.repo, done.Transfers)
		}
		if got := done.Transfers[0].Source; got != tc.from {
			t.Errorf("%s read from %q, want %q", tc.repo, got, tc.from)
		}
		if !hasTagAt(t, local, tc.repo, "v1") {
			t.Errorf("%s did not land on the target", tc.repo)
		}
	}
}

// A repository no route covers is a config that does not describe this image,
// and the job has to fail saying which one rather than reach for a default.
func TestMetaSourceRefusesAnUncoveredRepository(t *testing.T) {
	cdn, local := startRegistry(t), startRegistry(t)
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "remote", Kind: "meta", Routes: []config.Route{
			{ForRepos: []string{"dist/**"}, Store: "cdn"},
		}},
		{Name: "cdn", Kind: "oci", Host: cdn, Insecure: true},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	_, _, err := w.Submit(Request{Ref: "cr.invalid/stage/hday/cove:v1", Source: "remote", Target: "local"})
	if err == nil {
		t.Fatal("an uncovered repository was accepted")
	}
	if !strings.Contains(err.Error(), "stage/hday/cove") {
		t.Errorf("error does not name the repository: %v", err)
	}
}

// Pushing into a policy has no meaning; the refusal names the reason so an
// operator who wrote `target: remote` is not left reading connection errors.
func TestMetaIsRefusedAsATarget(t *testing.T) {
	w, _, cdn, _, _ := metaCopier(t)
	pushImage(t, cdn+"/dist/hday/cove:v1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	_, _, err := w.Submit(Request{Ref: "cr.invalid/dist/hday/cove:v1", Source: "cdn", Target: "remote"})
	if err == nil {
		t.Fatal("a meta store was accepted as a target")
	}
	if !strings.Contains(err.Error(), "only be a source") {
		t.Errorf("error does not say why: %v", err)
	}
}
