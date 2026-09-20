package cpx

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/lesomnus/gantry/cmd/config"
)

// narrowedCopier is the shape narrowing is about: a cloud registry declaring a
// site cache, delivering to an engine that pulls one platform.
func narrowedCopier(t *testing.T, enginePlatform string, route func(*config.CacheRoute)) (
	w *Copier, js Store, eng *fakePullEngine, cloud, site string,
) {
	t.Helper()
	cloud, site = startReferrersRegistry(t), startReferrersRegistry(t)
	r := config.CacheRoute{Store: "site"}
	if route != nil {
		route(&r)
	}
	cloudCfg := config.StoreConfig{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Caches: []config.CacheRoute{r}}
	siteCfg := config.StoreConfig{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"}
	eng = &fakePullEngine{name: "node", platform: enginePlatform}
	w, js = newCopier(t, []config.StoreConfig{cloudCfg, siteCfg}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	return w, js, eng, cloud, site
}

// holds reports whether a store serves this exact manifest.
func holds(t *testing.T, host, repo string, dg v1.Hash) bool {
	t.Helper()
	r, err := name.NewDigest(host+"/"+repo+"@"+dg.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Head(r)
	return err == nil
}

// attachSignature attaches an artifact typed as a Notary Project signature,
// which is what the narrow decision looks for by name.
//
// The type is carried on the CONFIG media type, the pre-1.1 shape. The
// in-memory registry derives a referrer descriptor's artifactType from the
// config alone, and a registry implementing the referrers API reads the
// manifest's own artifactType first and falls back to the config exactly like
// this — so both report the type these tests turn on, which is what makes the
// fixture worth anything.
func attachSignature(t *testing.T, repo name.Repository, subject v1.Hash, mediaType string, size int64) {
	t.Helper()
	sig, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	sig = mutate.MediaType(sig, types.OCIManifestSchema1)
	sig = mutate.ConfigMediaType(sig, notarySignature)
	sig = mutate.Subject(sig, v1.Descriptor{
		MediaType: types.MediaType(mediaType), Digest: subject, Size: size,
	}).(v1.Image)
	dg, err := sig.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Digest(dg.String()), sig); err != nil {
		t.Fatal(err)
	}
}

// run submits a job and waits for it to finish.
func run(t *testing.T, w *Copier, js Store, req Request) JobSnapshot {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	snap, _, err := w.Submit(req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	return done
}

// An engine pulls one platform, so a fill that carries the rest spends the
// authority's egress on architectures nobody asked for. The cache ends up
// holding the source's own manifest FOR THAT PLATFORM — not the index, and not
// an index gantry rebuilt — which is what lets the node's recorded digest still
// mean something.
func TestRoutedEngineFillCarriesOnlyTheDeliveredPlatform(t *testing.T) {
	w, js, eng, cloud, site := narrowedCopier(t, "linux/amd64", nil)
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64", "linux/s390x")
	index, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	amd64 := childOf(t, src, "linux/amd64")
	arm64 := childOf(t, src, "linux/arm64")

	done := run(t, w, js, Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "node"})
	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want a fill hop and the pull", done.Transfers)
	}

	if !holds(t, site, "team/app", amd64.Digest) {
		t.Error("the cache should hold the delivered platform's own manifest")
	}
	if holds(t, site, "team/app", index.Digest) {
		t.Error("the cache holds the whole index; the fill was not narrowed")
	}
	if holds(t, site, "team/app", arm64.Digest) {
		t.Error("the cache holds a platform nobody delivered")
	}
	// What the daemon was told: anchored to the child, so the digest it records
	// is the one the cache actually serves.
	calls := eng.pulls()
	if len(calls) != 1 {
		t.Fatalf("engine pulls = %+v, want one", calls)
	}
	if calls[0].digest != amd64.Digest.String() {
		t.Errorf("pull anchored to %s, want the child manifest %s", calls[0].digest, amd64.Digest)
	}
	if calls[0].platform != "linux/amd64" {
		t.Errorf("pull platform = %q, want the daemon's own", calls[0].platform)
	}
	// And it is told to pull the platform-suffixed tag, which is what the cache
	// published: the plain tag there would name an image the cache does not have.
	if !strings.HasSuffix(calls[0].ref, ":multi-linux-amd64") {
		t.Errorf("pull ref = %q, want the platform tag the fill published", calls[0].ref)
	}
}

// Two engines of different architectures reading the same origin through the
// same cache. Sharing one tag would make each fill invalidate the other's; the
// platform suffix keeps both, and the second job's probe is answered by its own
// platform's manifest rather than by whatever arrived last.
func TestNarrowedFillsOfDifferentPlatformsDoNotCollide(t *testing.T) {
	w, js, _, cloud, site := narrowedCopier(t, "linux/amd64", nil)
	arm := &fakePullEngine{name: "arm-node", platform: "linux/arm64"}
	w.stores.PutEngine(config.StoreConfig{Name: "arm-node", Kind: "docker"}, arm)
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	amd64, arm64 := childOf(t, src, "linux/amd64"), childOf(t, src, "linux/arm64")

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	for _, target := range []string{"node", "arm-node"} {
		snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: target})
		if err != nil {
			t.Fatalf("submit %s: %v", target, err)
		}
		if done := waitTerminal(t, js, snap.ID); done.State != JobDone {
			t.Fatalf("%s: state = %q (err=%q)", target, done.State, done.Err)
		}
	}
	if !holds(t, site, "team/app", amd64.Digest) || !holds(t, site, "team/app", arm64.Digest) {
		t.Error("the cache should hold both platforms, one per job")
	}
	for _, tag := range []string{"multi-linux-amd64", "multi-linux-arm64"} {
		if !hasTagAt(t, site, "team/app", tag) {
			t.Errorf("the cache should publish %q", tag)
		}
	}
}

// A second job for the same platform finds its own manifest already there.
func TestNarrowedRouteFindsItsPlatformWarm(t *testing.T) {
	w, js, _, cloud, _ := narrowedCopier(t, "linux/amd64", nil)
	pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")

	req := Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "node"}
	if done := run(t, w, js, req); len(done.Transfers) != 2 {
		t.Fatalf("first job transfers = %+v, want a fill and the pull", done.Transfers)
	}
	res, err := w.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Errorf("plan steps = %+v, want one hop now that this platform is cached", res.Steps)
	}
}

// all_platforms is the way to say the cache must hold the whole image — another
// architecture will be delivered from it later, or the origin's own index digest
// has to resolve there.
func TestAllPlatformsKeepsTheWholeImageInTheCache(t *testing.T) {
	w, js, _, cloud, site := narrowedCopier(t, "linux/amd64", func(r *config.CacheRoute) { r.AllPlatforms = true })
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	index, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	arm64 := childOf(t, src, "linux/arm64")

	run(t, w, js, Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "node"})

	if !holds(t, site, "team/app", index.Digest) {
		t.Error("the cache should hold the authority's own index")
	}
	if !holds(t, site, "team/app", arm64.Digest) {
		t.Error("the cache should hold every platform")
	}
	if !hasTagAt(t, site, "team/app", "multi") {
		t.Error("a wide fill publishes the plain tag")
	}
}

// Enforcement re-derives its verdict later from the digest the node recorded. A
// narrowed route makes that the child manifest, so a source that signs only the
// index would have the node quarantined for running exactly what it was told to.
// Carrying the whole image instead keeps the route AND the signature; only the
// narrowing is given up.
func TestNarrowingGivesWayToAnEnforcedTargetWithAnUnsignedPlatform(t *testing.T) {
	w, js, _, cloud, site := narrowedCopier(t, "linux/amd64", nil)
	w.SetEnforcedStores([]string{"node"})
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	index, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	// Signed on the index only, the way `notation sign <index-tag>` leaves it.
	attachSignature(t, src.Context(), index.Digest, string(index.MediaType), index.Size)

	run(t, w, js, Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "node"})

	if !holds(t, site, "team/app", index.Digest) {
		t.Error("an enforced target with no platform signature must get the whole image")
	}
}

// With the platform manifest signed too, the narrowing is safe: the cache holds
// the child, its signature travelled with it, and the digest the node records is
// one a later verification can still resolve a signature for.
func TestNarrowingSurvivesAnEnforcedTargetWithASignedPlatform(t *testing.T) {
	w, js, eng, cloud, site := narrowedCopier(t, "linux/amd64", nil)
	w.SetEnforcedStores([]string{"node"})
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	index, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	amd64 := childOf(t, src, "linux/amd64")
	attachSignature(t, src.Context(), amd64.Digest, string(amd64.MediaType), amd64.Size)

	run(t, w, js, Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "node"})

	if holds(t, site, "team/app", index.Digest) {
		t.Error("the fill was not narrowed even though the platform is signed")
	}
	if !holds(t, site, "team/app", amd64.Digest) {
		t.Fatal("the cache should hold the signed platform manifest")
	}
	siteCfg := config.StoreConfig{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"}
	siteRepo, err := name.NewRepository(site+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listReferrers(t, siteCfg, siteRepo, amd64.Digest)); n != 1 {
		t.Errorf("the cache serves %d referrers over the platform manifest, want the signature to have travelled", n)
	}
	if got := eng.pulls()[0].digest; got != amd64.Digest.String() {
		t.Errorf("pull anchored to %s, want the signed child %s", got, amd64.Digest)
	}
}

// A single-architecture image has nothing to narrow to, and its manifest is
// already the digest everything downstream keys on.
func TestNarrowingIsANoOpForASinglePlatformImage(t *testing.T) {
	w, js, _, cloud, site := narrowedCopier(t, "linux/amd64", nil)
	src := pushImage(t, cloud+"/team/app:1", 2)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}

	run(t, w, js, Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "node"})

	if !holds(t, site, "team/app", desc.Digest) {
		t.Error("the cache should hold the image's own manifest")
	}
	if !hasTagAt(t, site, "team/app", "1") {
		t.Error("nothing was narrowed, so the plain tag is what the fill publishes")
	}
}

// A registry delivery hands on what it received, and its caller may have asked
// for every platform — so the request decides, not the route.
func TestNarrowingDoesNotApplyToARegistryTarget(t *testing.T) {
	w, js, cloud, site, _ := routedCopier(t)
	src := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	index, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}

	run(t, w, js, Request{
		Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local",
		Platforms: []string{"linux/amd64"},
	})

	if !holds(t, site, "team/app", index.Digest) {
		t.Error("a registry-target route must still fill the cache verbatim")
	}
}

func TestNarrowTag(t *testing.T) {
	long := strings.Repeat("v1.2.3-", 30) // 210 chars, well past the limit
	for _, tc := range []struct {
		name, id, platform, want string
	}{
		{"tag", ":1.0", "linux/amd64", "1.0-linux-amd64"},
		{"variant", ":1.0", "linux/arm/v7", "1.0-linux-arm-v7"},
		{"os version", ":1.0", "windows/amd64:10.0.17763.1", "1.0-windows-amd64-10.0.17763.1"},
		{"digest", "@sha256:abc123", "linux/amd64", "sha256-abc123-linux-amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := narrowTag(tc.id, tc.platform); got != tc.want {
				t.Errorf("narrowTag(%q, %q) = %q, want %q", tc.id, tc.platform, got, tc.want)
			}
		})
	}
	t.Run("too long is truncated to a digest, not just cut", func(t *testing.T) {
		a := narrowTag(":"+long+"a", "linux/amd64")
		b := narrowTag(":"+long+"b", "linux/amd64")
		if len(a) > maxTagLen {
			t.Errorf("tag is %d chars, over the %d limit", len(a), maxTagLen)
		}
		if a == b {
			t.Error("two tags sharing a long prefix collapsed onto one reference")
		}
	})
	t.Run("every result is a legal tag", func(t *testing.T) {
		for _, id := range []string{":1.0", "@sha256:abcdef", ":" + long} {
			tag := narrowTag(id, "linux/arm/v7")
			if _, err := name.NewTag("example.com/x/y:"+tag, name.Insecure); err != nil {
				t.Errorf("narrowTag(%q) = %q, not a usable tag: %v", id, tag, err)
			}
		}
	})
}
