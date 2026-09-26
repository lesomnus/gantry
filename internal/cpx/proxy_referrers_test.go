package cpx

import (
	"context"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

// proxyCopier is a cloud registry whose cache is a PULL-THROUGH, delivering to
// an engine — the shape that could not be routed at all until this asked the
// cache what it answers with.
func proxyCopier(t *testing.T) (w *Copier, cloud, site string) {
	t.Helper()
	cloud, site = startReferrersRegistry(t), startReferrersRegistry(t)
	w, _ = newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "proxy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, &fakePullEngine{name: "node", platform: "linux/amd64"})
	return w, cloud, site
}

// routedThrough is the store the delivery reads first, and why.
func routedThrough(t *testing.T, w *Copier, ref string) (store, why string, steps int) {
	t.Helper()
	res, err := w.Plan(context.Background(), Request{Ref: ref, Source: "cloud", Target: "node"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	first := res.Steps[len(res.Steps)-1].Sources[0]
	return first.Store, first.Why, len(res.Steps)
}

// An engine delivery through a pull-through cache, which is the case that was
// refused outright.
//
// The refusal was about referrers: serve.enforce re-derives a node's verdict
// from the digest it recorded, against the store that digest's host names, so a
// route that leaves the signatures behind leaves the node holding an image no
// verifier can check. gantry could not tell whether a given pull-through
// answers for them, so it assumed not — and the assumption covered every engine
// job, which is every delivery to a node.
//
// Now it asks, and the answer decides. A cache that passes the referrers API
// upstream reports what the authority reports and is routed through.
func TestProxyCacheThatServesReferrersIsRoutedThrough(t *testing.T) {
	w, cloud, site := proxyCopier(t)
	src := pushImage(t, cloud+"/team/app:1", 1)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	attachSignature(t, src.Context(), desc.Digest, string(desc.MediaType), desc.Size)

	// The cache as a proxy that has served this image and answers for its
	// referrers: the same image under the same digest, and the same signature
	// over it. Both halves, because a referrers listing is about a subject the
	// registry has.
	siteRepo, err := name.NewRepository(site+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := desc.Image()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(siteRepo.Digest(desc.Digest.String()), img); err != nil {
		t.Fatal(err)
	}
	attachSignature(t, siteRepo, desc.Digest, string(desc.MediaType), desc.Size)

	store, why, steps := routedThrough(t, w, cloud+"/team/app:1")
	if steps != 1 {
		t.Errorf("plan has %d steps, want one: reading a proxy is what fills it", steps)
	}
	if store != "site" || why != "route" {
		t.Errorf("the delivery reads %q (%s), want the cache", store, why)
	}
}

// And the cache that does not: it answers from what it holds, which is nothing
// the authority signed, so the job reads the authority instead. Same outcome as
// the old refusal, now for a reason that was measured rather than assumed — and
// the node still gets its image, from one hop further away.
func TestProxyCacheWithoutReferrersIsNotRoutedThrough(t *testing.T) {
	w, cloud, _ := proxyCopier(t)
	src := pushImage(t, cloud+"/team/app:1", 1)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	attachSignature(t, src.Context(), desc.Digest, string(desc.MediaType), desc.Size)

	store, why, _ := routedThrough(t, w, cloud+"/team/app:1")
	if store != "cloud" || why != "planned" {
		t.Errorf("the delivery reads %q (%s), want the authority the caller named", store, why)
	}
}

// An image with no referrers at all is asked to prove nothing. Signing is not
// on for this fleet yet, and a cache that would serve every signature it was
// given must not be refused for an image that has none.
func TestProxyCacheIsRoutedThroughForAnImageWithNoReferrers(t *testing.T) {
	w, cloud, _ := proxyCopier(t)
	pushImage(t, cloud+"/team/app:1", 1)

	store, why, _ := routedThrough(t, w, cloud+"/team/app:1")
	if store != "site" || why != "route" {
		t.Errorf("the delivery reads %q (%s), want the cache", store, why)
	}
}
