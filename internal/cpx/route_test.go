package cpx

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// routedCopier builds the shape routing is about: a cloud registry that holds the
// image and declares a site registry as its cache, plus a rack-local registry the
// caller actually asks to fill. Three separate registries — a single ggcr
// in-memory registry keys blobs by digest across repositories, so fewer could not
// model "the site does not have it".
func routedCopier(t *testing.T, opts ...func(*config.StoreConfig)) (w *Copier, js Store, cloud, site, local string) {
	t.Helper()
	cloud, site, local = startRegistry(t), startRegistry(t), startRegistry(t)
	cloudCfg := config.StoreConfig{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"}
	siteCfg := config.StoreConfig{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"}
	for _, o := range opts {
		o(&siteCfg)
	}
	w, js = newCopier(t, []config.StoreConfig{
		cloudCfg, siteCfg,
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	return w, js, cloud, site, local
}

func hasTagAt(t *testing.T, host, repo, tag string) bool {
	t.Helper()
	r, err := name.ParseReference(host+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Head(r)
	return err == nil
}

// A cold cache is filled first, then read: the cloud registry serves the image
// once instead of once per destination, which is the whole point.
func TestRoutedCopyFillsTheCacheThenReadsIt(t *testing.T) {
	w, js, cloud, site, local := routedCopier(t)
	pushImage(t, cloud+"/team/app:1", 2)
	if hasTagAt(t, site, "team/app", "1") {
		t.Fatal("the site must start empty for this test to mean anything")
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want a fill hop and a delivery hop", done.Transfers)
	}
	fill, deliver := done.Transfers[0], done.Transfers[1]
	if fill.Step != 0 || fill.Store != "site" || fill.Source != "cloud" || fill.State != "done" {
		t.Errorf("fill hop = %+v, want site ◀── cloud", fill)
	}
	if deliver.Step != 1 || deliver.Store != "local" || deliver.Source != "site" || deliver.State != "done" {
		t.Errorf("delivery hop = %+v, want local ◀── site", deliver)
	}
	// The job reports what was asked for; the hops report what happened.
	if done.Source != "cloud" || done.Target != "local" {
		t.Errorf("job stores = %q -> %q, want cloud -> local", done.Source, done.Target)
	}
	// Both stores end up serving the image, the site under the tag so the next
	// job's probe finds it.
	if !hasTagAt(t, site, "team/app", "1") {
		t.Error("the site should hold the image under its tag after the fill")
	}
	if !hasTagAt(t, local, "team/app", "1") {
		t.Error("the caller's target should hold the image")
	}
}

// Once the cache holds the image there is no fill hop and the cloud registry is
// not read at all beyond settling the tag.
func TestRoutedCopySkipsTheFillWhenTheCacheIsWarm(t *testing.T) {
	w, js, cloud, site, _ := routedCopier(t)
	pushImage(t, cloud+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	// First job fills the site.
	first, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if done := waitTerminal(t, js, first.ID); done.State != JobDone {
		t.Fatalf("first job state = %q (err=%q)", done.State, done.Err)
	}
	if !hasTagAt(t, site, "team/app", "1") {
		t.Fatal("the first job should have filled the site")
	}

	// A second destination now finds it warm. Distinct target, so no coalescing.
	second, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "site"})
	if err == nil {
		waitTerminal(t, js, second.ID)
	}
	res, err := w.Plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("plan steps = %+v, want a single hop for a warm cache", res.Steps)
	}
	if got := res.Steps[0].Sources[0]; got.Store != "site" || got.Why != "route" {
		t.Errorf("first source = %+v, want the site as the route", got)
	}
}

// A cache gantry cannot even ask about is not routed through at all: probing is
// how it decides how many hops the job has, so a probe it cannot make means it
// has nothing to decide with.
func TestRoutedCopySkipsAnUnprobeableCache(t *testing.T) {
	w, js, cloud, _, local := routedCopier(t, func(c *config.StoreConfig) {
		c.CACert = filepath.Join(t.TempDir(), "no-such-ca.pem")
	})
	pushImage(t, cloud+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 1 {
		t.Fatalf("transfers = %+v, want the direct copy alone", done.Transfers)
	}
	if d := done.Transfers[0]; d.Source != "cloud" || d.Store != "local" {
		t.Errorf("transfer = %+v, want local ◀── cloud", d)
	}
	if !hasTagAt(t, local, "team/app", "1") {
		t.Error("the caller's target should hold the image")
	}
}

// A route that can be probed but not filled is abandoned, not fatal: the caller
// asked for a copy from the source it named, and that is what it gets. This is the
// shape an operator lands in with read-only access to the cache.
func TestRoutedCopyFallsThroughWhenTheCacheCannotBeFilled(t *testing.T) {
	cloud, local := startRegistry(t), startRegistry(t)
	site := startReadOnlyRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	pushImage(t, cloud+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q), want done: an unusable route is not a failure", done.State, done.Err)
	}
	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want the abandoned fill and the direct copy", done.Transfers)
	}
	if fill := done.Transfers[0]; fill.Step != 0 || fill.Store != "site" || fill.State != "failed" || fill.Err == "" {
		t.Errorf("hop 0 = %+v, want the abandoned fill carrying its error", fill)
	}
	// The delivery hop's route attempt is pruned — the fill did not deliver — so
	// its only row is the direct read of the source the caller named.
	if d := done.Transfers[1]; d.Step != 1 || d.Source != "cloud" || d.State != "done" {
		t.Errorf("hop 1 = %+v, want the direct copy from cloud", d)
	}
	if !hasTagAt(t, local, "team/app", "1") {
		t.Error("the caller's target should hold the image anyway")
	}
}

// Reading a pull-through cache is what fills it, so routing through one is a
// single hop: no fill, no probe, and no write access to it required.
func TestRoutedCopyThroughAProxyCacheIsOneHop(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t, func(c *config.StoreConfig) { c.Mode = "proxy" })
	pushImage(t, cloud+"/team/app:1", 1)

	res, err := w.Plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("plan steps = %+v, want one hop: reading a proxy is what fills it", res.Steps)
	}
	if got := res.Steps[0].Sources[0]; got.Store != "site" || got.Why != "route" {
		t.Errorf("first source = %+v, want the proxy cache as the route", got)
	}
}

// Neither end of a route is routed: naming the cache as the target IS the fill,
// and naming it as the source has nothing to route through.
func TestRouteSkipsItsOwnEnds(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	pushImage(t, cloud+"/team/app:1", 1)

	for _, tc := range []struct{ name, source, target string }{
		{"the target is the cache", "cloud", "site"},
		{"the source is the cache", "site", "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := w.Plan(context.Background(), Request{
				Ref: cloud + "/team/app:1", Source: tc.source, Target: tc.target,
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(res.Steps) != 1 {
				t.Fatalf("plan steps = %+v, want a single unrouted hop", res.Steps)
			}
			if got := res.Steps[0].Sources; len(got) != 1 || got[0].Store != tc.source {
				t.Errorf("sources = %+v, want only the source the caller named", got)
			}
		})
	}
}

// A step gantry generates is never itself routed, whatever its own source
// declares — one level, so no graph is walked and no cycle can form.
func TestGeneratedStepsAreNotThemselvesRouted(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	pushImage(t, cloud+"/team/app:1", 1)

	res, err := w.Plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("plan steps = %+v, want a fill and a delivery", res.Steps)
	}
	// The fill reads the cloud directly. Were it routed too, it would read the
	// site — the store it is filling.
	if got := res.Steps[0].Sources; len(got) != 1 || got[0].Store != "cloud" {
		t.Errorf("fill sources = %+v, want the cloud alone", got)
	}
	if !res.Steps[0].Optional {
		t.Error("a hop gantry added for itself must be optional")
	}
	if res.Steps[1].Optional {
		t.Error("the hop the caller asked for must not be optional")
	}
}

// Every hop is anchored to the digest the authority settled, so a nearer copy
// cannot substitute different content for the same tag.
func TestRoutedCopyIsAnchoredToTheAuthoritysDigest(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	src := pushImage(t, cloud+"/team/app:1", 1)
	got, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	dg := got.Digest.String()

	res, err := w.Plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for i, st := range res.Steps {
		for _, s := range st.Sources {
			if !strings.HasSuffix(s.Ref, "@"+dg) {
				t.Errorf("hop %d reads %q, want it anchored to the authority's digest %s", i, s.Ref, dg)
			}
		}
	}
	// The fill lands under the TAG, so the digest resolves from the cache too and
	// the next job's probe finds it.
	if !strings.HasSuffix(res.Steps[0].Ref, ":1") {
		t.Errorf("the fill lands at %q, want the tag form", res.Steps[0].Ref)
	}
}

// An authority that ANSWERS "I do not have it" is the most definite answer there
// is. Reading a cache of it instead would serve content the authority never had,
// under a tag the caller believes points at the authority's image — so the job is
// not routed at all and reads the source it named.
func TestRouteSkipsAnAuthorityThatSaysNo(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	// Nothing was pushed, so the cloud answers 404 for the tag.
	res, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local",
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("plan steps = %+v, want an unrouted single hop", res.Steps)
	}
	if got := res.Steps[0].Sources; len(got) != 1 || got[0].Store != "cloud" {
		t.Errorf("sources = %+v, want only the source the caller named", got)
	}

	// Strictness has nothing to add: the authority was not unavailable, it answered.
	yes := true
	if _, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", RequireAuthority: &yes,
	}); err != nil {
		t.Errorf("a definite answer is not a require_authority failure: %v", err)
	}
}

// An authority that cannot answer at all is different: reading the cache by tag is
// then the useful answer — the site registry keeps working while the cloud one does
// not — and it is the only case a caller can receive content the authority never
// confirmed, so require_authority refuses it.
func TestRequireAuthorityGovernsAnUnreachableAuthority(t *testing.T) {
	cloud, site, local := startRegistry(t), startRegistry(t), startRegistry(t)
	// The cloud store's transport cannot be built, so it cannot be asked anything.
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site",
			CACert: filepath.Join(t.TempDir(), "no-such-ca.pem")},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	req := Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"}

	res, err := w.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("plan steps = %+v, want a single hop read by tag", res.Steps)
	}
	if got := res.Steps[0].Sources[0]; got.Store != "site" || strings.Contains(got.Ref, "@") {
		t.Errorf("first source = %+v, want the cache read by tag", got)
	}

	strict := req
	yes := true
	strict.RequireAuthority = &yes
	if _, err := w.Plan(context.Background(), strict); err == nil ||
		!strings.Contains(err.Error(), "require_authority") {
		t.Fatalf("err = %v, want a require_authority rejection", err)
	}

	// The server default does the same for a deployment that opts in.
	w.wc.RequireAuthority = true
	if _, err := w.Plan(context.Background(), req); err == nil {
		t.Error("the server default should refuse an unconfirmed reference too")
	}
	// ...and an explicit false still wins over it.
	no := false
	lenient := req
	lenient.RequireAuthority = &no
	if _, err := w.Plan(context.Background(), lenient); err != nil {
		t.Errorf("an explicit require_authority=false must win over the default: %v", err)
	}
}

// require_authority has nothing to say about a job that reads the authority
// itself: there is no nearer copy involved, so an unreachable source is just a
// failure at run time as it always was.
func TestRequireAuthorityIsANoOpWithoutARoute(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	w.wc.RequireAuthority = true
	// "site" declares no cache, so this job is not routed.
	if _, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "site", Target: "local",
	}); err != nil {
		t.Fatalf("an unrouted job must not be refused: %v", err)
	}
}

// A layer failure during a fill hop aborts that hop's siblings and nothing more:
// the delivery hop still has a live context to run the direct copy on.
func TestAbandonedFillLeavesTheJobRunnable(t *testing.T) {
	w, js, cloud, _, local := routedCopier(t, func(c *config.StoreConfig) {
		c.CACert = filepath.Join(t.TempDir(), "no-such-ca.pem")
	})
	pushImage(t, cloud+"/team/app:1", 3)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if !hasTagAt(t, local, "team/app", "1") {
		t.Error("the target should hold the image despite the abandoned fill")
	}
	if errors.Is(errors.New(done.Err), context.Canceled) {
		t.Error("an abandoned hop must not surface as a cancellation")
	}
}

// The fill must preserve the authority's manifest byte for byte, or the cache ends
// up holding a DIFFERENT digest for the same tag — and then the probe misses on
// every subsequent job and nothing is ever read from the cache. A rebuilt index
// is the failure mode, so this uses a multi-platform image, where rebuilding
// actually changes the digest.
func TestRoutedFillPreservesTheAuthoritysDigest(t *testing.T) {
	w, js, cloud, site, _ := routedCopier(t)
	idx := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	got, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := got.Digest.String()

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if done := waitTerminal(t, js, snap.ID); done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	// The site must serve the authority's digest, not one of its own making.
	ref, err := name.ParseReference(site+"/team/app@"+dg, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(ref); err != nil {
		t.Fatalf("the cache does not hold the authority's digest: %v", err)
	}
	// Which is exactly what makes the next job a single hop.
	res, err := w.Plan(context.Background(), Request{Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Errorf("plan steps = %+v, want one hop now that the cache is warm", res.Steps)
	}
}

// A narrowed copy still fills the cache COMPLETELY. Narrowing the fill would leave
// the cache holding a rebuilt, platform-filtered index — a different digest for the
// same tag — and then every later job probes for content that is not there and the
// cache is never read. The cost is real (more bytes into the cache than this caller
// needed) and for a shared cache it is the desirable trade.
func TestRoutedFillIsNotNarrowedByTheRequest(t *testing.T) {
	w, js, cloud, site, _ := routedCopier(t)
	idx := pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")
	got, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := got.Digest.String()

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local",
		Platforms: []string{"linux/amd64"},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want a fill and a delivery", done.Transfers)
	}

	ref, err := name.ParseReference(site+"/team/app@"+dg, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(ref); err != nil {
		t.Fatalf("the cache holds a narrowed index instead of the authority's: %v", err)
	}
	// The caller still gets what it asked for: only its platform, at its target.
	res, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local",
		Platforms: []string{"linux/amd64"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Platforms) != 1 || res.Platforms[0] != "linux/amd64" {
		t.Errorf("job platforms = %v, want the request's narrowing preserved", res.Platforms)
	}
	if len(res.Steps) != 1 {
		t.Errorf("plan steps = %+v, want one hop now that the cache is warm", res.Steps)
	}
}

// The fill hop's own settings, asserted on the plan. Both are requirements against
// a real registry that an in-memory one cannot expose: verbatim, because a rebuilt
// index is a different digest for the same tag unless it happens to round-trip
// byte-identically (a plain index does, one with annotations or attestation
// children does not); and un-narrowed, because a verbatim commit references every
// child and a registry rejects an index whose children are missing. Pinned here so
// neither can be changed by accident on the strength of a passing fixture.
func TestRoutedFillHopSettings(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	pushIndex(t, cloud+"/team/app:multi", "linux/amd64", "linux/arm64")

	p, err := w.plan(context.Background(), Request{
		Ref: cloud + "/team/app:multi", Source: "cloud", Target: "local",
		Platforms: []string{"linux/amd64"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.steps) != 2 {
		t.Fatalf("steps = %d, want a fill and a delivery", len(p.steps))
	}
	fill, deliver := p.steps[0], p.steps[1]
	if !fill.verbatim {
		t.Error("the fill must commit verbatim, or the cache holds a digest of its own making")
	}
	if fill.platforms != nil {
		t.Errorf("fill platforms = %v, want every platform: a verbatim commit references every child", fill.platforms)
	}
	if !fill.optional {
		t.Error("the fill is gantry's own, so its failure must be tolerated")
	}
	if fill.fills == "" || !strings.HasSuffix(fill.fills, ":multi") {
		t.Errorf("fill publishes %q, want the tag form so the next probe finds it", fill.fills)
	}
	// The caller's own hop keeps the caller's narrowing.
	if len(deliver.platforms) != 1 || deliver.platforms[0] != "linux/amd64" {
		t.Errorf("delivery platforms = %v, want the request's narrowing", deliver.platforms)
	}
	// And its cache read is conditional on the fill having delivered.
	if got := deliver.attempts[0]; got.why != whyRoute || len(got.needs) != 1 || got.needs[0] != fill.idx {
		t.Errorf("delivery's first attempt = {why:%s needs:%v}, want the cache gated on the fill", got.why, got.needs)
	}
}

// Referrers must travel on the FILL hop, from the authority that has them. Without
// that, the delivery hop asks the cache for signatures it was never given, finds
// none — an empty list is not an error — and the job completes having silently
// dropped exactly what copy_referrers exists to propagate.
func TestRoutedCopyCarriesReferrersOnEveryHop(t *testing.T) {
	w, js, cloud, site, local := routedCopier(t)
	src := pushImage(t, cloud+"/team/app:1", 2)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	cloudCfg, _ := w.stores.Config("cloud")
	attachReferrer(t, cloudCfg, src.Context(), desc)
	if n := len(listReferrers(t, cloudCfg, src.Context(), desc.Digest)); n != 1 {
		t.Fatalf("the authority holds %d referrers, want 1", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	yes := true
	snap, _, err := w.Submit(Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: &yes,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want a fill and a delivery", done.Transfers)
	}

	siteCfg, _ := w.stores.Config("site")
	siteRepo, err := name.NewRepository(site+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listReferrers(t, siteCfg, siteRepo, desc.Digest)); n != 1 {
		t.Errorf("the cache holds %d referrers, want the fill to have carried it", n)
	}
	localCfg, _ := w.stores.Config("local")
	localRepo, err := name.NewRepository(local+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listReferrers(t, localCfg, localRepo, desc.Digest)); n != 1 {
		t.Errorf("the target holds %d referrers, want the signature to have reached it", n)
	}
}

// A cache filled by an earlier job that did not need referrers holds the image but
// not the signatures over it. Reading it would satisfy the copy and drop them, so a
// job that propagates referrers reads the authority instead — the image is already
// cached, so declining costs nothing but the bytes it was going to move anyway.
func TestRoutedCopyDeclinesAReferrerlessWarmCache(t *testing.T) {
	w, js, cloud, _, _ := routedCopier(t)
	src := pushImage(t, cloud+"/team/app:1", 2)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	// A first job with no referrers to propagate warms the cache.
	no := false
	first, _, err := w.Submit(Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: &no,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := waitTerminal(t, js, first.ID); got.State != JobDone {
		t.Fatalf("first job state = %q (err=%q)", got.State, got.Err)
	}

	// The signature appears at the authority afterwards.
	cloudCfg, _ := w.stores.Config("cloud")
	attachReferrer(t, cloudCfg, src.Context(), desc)

	// A job that wants it must not be served from the referrer-less cache.
	yes := true
	res, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "site", CopyReferrers: &yes,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_ = res
	res2, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: &yes,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res2.Steps) != 1 || res2.Steps[0].Sources[0].Store != "cloud" {
		t.Errorf("plan = %+v, want an unrouted read of the authority", res2.Steps)
	}
}

// A registry that refuses to answer has said nothing about what it holds. Reading
// that as "absent" would copy a whole image into a store that already has it — and
// most likely fail the push for the same reason the probe was refused.
func TestProbeOnlyTreatsADefiniteNoAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		absent  bool
		wantErr bool
	}{
		{"not found", 404, true, false},
		{"forbidden hides existence", 403, true, false},
		{"unauthorized", 401, false, true},
		{"rate limited", 429, false, true},
		{"broken", 500, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := startStatusRegistry(t, tc.status)
			store := config.StoreConfig{Name: "probe", Kind: "oci", Host: host, Insecure: true}
			dg, err := name.NewDigest(host+"/team/app@sha256:"+strings.Repeat("c", 64), name.Insecure)
			if err != nil {
				t.Fatal(err)
			}
			held, err := holdsDigest(context.Background(), store, dg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("held=%v err=nil, want an error: %d says nothing about what the store holds", held, tc.status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if held != !tc.absent {
				t.Errorf("held = %v, want %v", held, !tc.absent)
			}
		})
	}
}

// The same rule settles whether the AUTHORITY answered or failed to: only a
// definite no is ErrNoSuchImage, and only that stops the job being routed.
func TestAuthorityAnswerIsDistinguishedFromSilence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		noSuchImg bool
	}{
		{"not found", 404, true},
		{"forbidden", 403, true},
		{"unauthorized", 401, false},
		{"broken", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := startStatusRegistry(t, tc.status)
			store := config.StoreConfig{Name: "authority", Kind: "oci", Host: host, Insecure: true}
			ref, err := name.ParseReference(host+"/team/app:1", name.Insecure)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolveDigest(context.Background(), store, ref)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, ErrNoSuchImage); got != tc.noSuchImg {
				t.Errorf("ErrNoSuchImage = %v, want %v (status %d)", got, tc.noSuchImg, tc.status)
			}
		})
	}
}

// A route the delivery hop could not read must not be filled: copying a whole image
// into a cache that will then be ignored is the most expensive way to do nothing.
func TestRouteIsNotFilledWhenItCannotBeRead(t *testing.T) {
	t.Run("an engine reaching every source by one host", func(t *testing.T) {
		eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
		cloud, site := startRegistry(t), startRegistry(t)
		w, _ := newCopier(t, []config.StoreConfig{
			{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
			{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		}, false)
		w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker", PullHost: "mirror.local"}, eng)
		pushImage(t, cloud+"/team/app:1", 1)

		res, err := w.Plan(context.Background(), Request{
			Ref: cloud + "/team/app:1", Source: "cloud", Target: "node",
		})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if len(res.Steps) != 1 {
			t.Fatalf("plan steps = %+v, want no fill: the daemon reads one host whatever gantry names", res.Steps)
		}
	})
	t.Run("a pull-through target fetching from its own upstream", func(t *testing.T) {
		cloud, site, local := startRegistry(t), startRegistry(t), startRegistry(t)
		w, _ := newCopier(t, []config.StoreConfig{
			{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
			{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
			{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "proxy"},
		}, false)
		pushImage(t, cloud+"/team/app:1", 1)

		res, err := w.Plan(context.Background(), Request{
			Ref: cloud + "/team/app:1", Source: "cloud", Target: "local",
		})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if len(res.Steps) != 1 {
			t.Fatalf("plan steps = %+v, want no fill: a proxy target ignores the source it is handed", res.Steps)
		}
	})
}

// Giving up on a source gantry chose for itself is the signal that the cache is not
// earning its keep, so it must be reported — including when the attempt was skipped
// rather than tried, because a route that never ran is as unused as one that failed.
func TestAbandonedRouteIsReported(t *testing.T) {
	cloud, local := startRegistry(t), startRegistry(t)
	site := startReadOnlyRegistry(t) // probeable, not fillable
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	rec := &recordingRecorder{}
	w.SetRecorder(rec)
	pushImage(t, cloud+"/team/app:1", 1)

	mctx, reader := otxContext(t)
	ctx, cancel := context.WithCancel(mctx)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if done := waitTerminal(t, js, snap.ID); done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	// The reason names what was given up: a cache gantry chose, not the source the
	// caller named.
	if got, ok := counterValue(t, rm, "gantry.job.fallback", "reason", "route"); !ok || got < 1 {
		t.Errorf("gantry.job.fallback{reason=route} = %d (found=%v), want at least 1", got, ok)
	}
	if got, ok := counterValue(t, rm, "gantry.job.fallback", "from", "site"); !ok || got < 1 {
		t.Errorf("gantry.job.fallback{from=site} = %d (found=%v), want the abandoned cache named", got, ok)
	}
	if calls, _ := rec.snapshot(); len(calls) == 0 {
		t.Error("an abandoned route must leave a durable audit record")
	}
}

// A job that recovers is not failing, so it must go back to attracting callers: an
// identical submit should join work already in progress rather than start a second
// copy of it.
func TestRecoveredAttemptUnsealsTheJob(t *testing.T) {
	w, js, origin, cache := fallbackCopier(t, &fakePullEngine{name: "node", platform: "linux/amd64"})
	pushImage(t, origin+"/team/app:1", 1)
	_ = cache

	job := NewJob("job_r", "a/b:1", nil, time.Now())
	job.dedup = "k"
	job.ctx = context.Background()
	tr := &Transfer{}
	job.Transfers = []*Transfer{tr}
	if err := js.Add(job); err != nil {
		t.Fatal(err)
	}
	js.Update(job.ID, func(j *Job) { j.sealed = true })
	if _, ok := js.Active("k"); ok {
		t.Fatal("a sealed job should not be a coalescing target")
	}

	st := &execStep{dst: stubDest{"x"}, newMover: func(*Copier, *execAttempt) (mover, error) {
		return okMover{}, nil
	}}
	if err := w.runAttempt(context.Background(), job, st, &execAttempt{}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, ok := js.Active("k"); !ok {
		t.Error("a job whose attempt succeeded must attract callers again")
	}
}

type okMover struct{}

func (okMover) run(context.Context, *Job, *Transfer) error { return nil }

// A routed job publishes the very reference its own later hop reads. When that read
// misses, the wait must skip the asking job — waiting for a fill it performed itself
// could only ever burn the whole bound and the only wait slot.
//
// The fill has to SUCCEED for this to be reachable: a failed fill prunes the cache
// read instead, so nothing waits. Hence an engine target, whose pull can be made to
// fail against the cache after the cache was filled.
func TestRoutedJobDoesNotWaitForItsOwnFill(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	cloud, site := startRegistry(t), startRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	w.wc.SourceWait = config.Duration(30 * time.Second) // would hang the test if taken
	eng.failFor = failHost(site, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, cloud+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	start := time.Now()
	yes := true
	snap, _, err := w.Submit(Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "node", FallbackToOrigin: &yes,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("job took %s — it waited for the fill it was performing itself", took)
	}
	// The fill happened, the read of it missed, and the daemon was sent to the cloud.
	calls := eng.pulls()
	if len(calls) != 2 {
		t.Fatalf("pull attempts = %+v, want the cache then the cloud", calls)
	}
	if !strings.HasPrefix(calls[0].ref, site+"/") || !strings.HasPrefix(calls[1].ref, cloud+"/") {
		t.Errorf("pull refs = %q then %q, want the cache then the cloud", calls[0].ref, calls[1].ref)
	}
}

// require_authority changes what a caller may be served, so it splits the dedup
// key: a submit that refused content its source never confirmed must not be handed
// a job that accepted it.
func TestRequireAuthoritySplitsTheDedupKey(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	w.base = context.Background() // enqueue without workers; jobs stay active
	pushImage(t, cloud+"/team/app:1", 1)

	yes, no := true, false
	req := Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", RequireAuthority: &no}
	if _, created, err := w.Submit(req); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}
	same := req
	if _, created, err := w.Submit(same); err != nil || created {
		t.Fatalf("an identical submit should coalesce: created=%v err=%v", created, err)
	}
	strict := req
	strict.RequireAuthority = &yes
	if _, created, err := w.Submit(strict); err != nil || !created {
		t.Fatalf("a stricter submit must not coalesce onto a lenient job: created=%v err=%v", created, err)
	}
}

// --- review round: defects found reviewing this branch ----------------------

// The origin fallback is not a read of the authority's content from somewhere
// nearer — it is the fallback's own trust decision, in which the origin resolves
// the tag itself. Routing settles the tag at the SOURCE, so re-anchoring every
// attempt to that digest would silently convert the fallback, and would break it
// outright whenever the source holds a manifest the origin never had (a
// platform-narrowed copy rebuilds the index, so that is ordinary).
func TestRoutingDoesNotPinTheOriginFallbackToTheSourcesDigest(t *testing.T) {
	origin, mid, rack := startRegistry(t), startRegistry(t), startRegistry(t)
	// Deliberately different content under the same tag: if the origin attempt
	// were re-anchored to what `mid` reports, it would ask the origin for a digest
	// the origin has never held.
	pushImage(t, origin+"/team/app:1", 1)
	pushImage(t, mid+"/team/app:1", 3)

	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "origin", Kind: "oci", Host: origin, Insecure: true},
		{Name: "mid", Kind: "oci", Host: mid, Insecure: true, Mode: "copy", Cache: "rack"},
		{Name: "rack", Kind: "oci", Host: rack, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)

	p, err := w.plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "mid", Target: "node", FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var origAt, routeAt *execAttempt
	for _, at := range p.last().attempts {
		switch at.why {
		case whyOrigin:
			origAt = at
		case whyRoute:
			routeAt = at
		}
	}
	if routeAt == nil {
		t.Fatal("the job should have been routed through the rack cache")
	}
	if _, pinned := routeAt.ref.(name.Digest); !pinned {
		t.Errorf("the route attempt = %q, want it anchored to the authority's digest", routeAt.ref.Name())
	}
	if origAt == nil {
		t.Fatal("the job should have an origin fallback attempt")
	}
	if dg, pinned := origAt.ref.(name.Digest); pinned {
		t.Errorf("the origin attempt = %q, want the tag; routing pinned it to the SOURCE's digest %s",
			origAt.ref.Name(), dg.DigestStr())
	}
	if want := origin + "/team/app:1"; origAt.pullRef != want {
		t.Errorf("origin pull ref = %q, want %q", origAt.pullRef, want)
	}
}

// An authority that cannot answer leaves no digest, so the cache cannot be asked
// whether it holds the referrers either. Reading it unasked is how a signature
// goes missing on a job that reports done, so such a job is simply not routed.
func TestRouteDeclinesAnUnconfirmedCacheWhenReferrersMustTravel(t *testing.T) {
	site, local := startRegistry(t), startRegistry(t)
	// A dead port: not a registry's "no", just silence.
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: "127.0.0.1:1", Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)

	res, err := w.Plan(context.Background(), Request{
		Ref: "127.0.0.1:1/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Sources) != 1 || res.Steps[0].Sources[0].Store != "cloud" {
		t.Fatalf("steps = %+v, want one unrouted hop reading the cloud", res.Steps)
	}
	// Without the referrer requirement the same silence DOES route: that is the
	// documented behaviour this must not have broken.
	res, err = w.Plan(context.Background(), Request{
		Ref: "127.0.0.1:1/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(false),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Sources) != 2 || res.Steps[0].Sources[0].Store != "site" {
		t.Fatalf("steps = %+v, want the site read ahead of the cloud", res.Steps)
	}
}

// A cache is complete when it holds everything the AUTHORITY holds — not when it
// holds anything at all. Asking `> 0` declines every image that legitimately has
// no referrers, which leaves the whole optimization inert while every job still
// reports done.
func TestWarmCacheIsJudgedAgainstTheAuthoritysReferrers(t *testing.T) {
	cloud, site, local := startReferrersRegistry(t), startReferrersRegistry(t), startRegistry(t)
	cloudCfg := config.StoreConfig{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"}
	siteCfg := config.StoreConfig{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"}
	w, js := newCopier(t, []config.StoreConfig{
		cloudCfg, siteCfg,
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	src := pushImage(t, cloud+"/team/app:1", 1)
	// Warm the site directly (target == cache is the degenerate, unrouted shape).
	warm, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "site", CopyReferrers: boolp(false)})
	if err != nil {
		t.Fatalf("warm submit: %v", err)
	}
	if done := waitTerminal(t, js, warm.ID); done.State != JobDone {
		t.Fatalf("warm job state = %q (err=%q)", done.State, done.Err)
	}

	// The image has no referrers anywhere, so nothing can be dropped and the warm
	// cache must be used.
	res, err := w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Sources[0].Store != "site" {
		t.Fatalf("steps = %+v, want the warm site read; an image with no referrers has none to drop", res.Steps)
	}

	// Give the AUTHORITY a signature the site does not have: now the site is
	// genuinely incomplete and must be declined.
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	attachReferrer(t, cloudCfg, src.Context(), desc)
	res, err = w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Sources[0].Store != "cloud" {
		t.Fatalf("steps = %+v, want the cloud read: the site is missing the authority's referrer", res.Steps)
	}

	// A cache holding SOME of them is still incomplete. This is the half a
	// "does it have any" test cannot see.
	siteRepo, err := name.NewRepository(site+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	siteSrc, err := name.ParseReference(site+"/team/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	siteDesc, err := remote.Get(siteSrc)
	if err != nil {
		t.Fatal(err)
	}
	attachReferrer(t, siteCfg, siteRepo, siteDesc)             // the site now has one…
	attachReferrer(t, cloudCfg, src.Context(), desc, "second") // …of the authority's two
	if n := len(listReferrers(t, cloudCfg, src.Context(), desc.Digest)); n != 2 {
		t.Fatalf("the authority holds %d referrers, want 2", n)
	}
	if n := len(listReferrers(t, siteCfg, siteRepo, siteDesc.Digest)); n != 1 {
		t.Fatalf("the site holds %d referrers, want 1", n)
	}
	res, err = w.Plan(context.Background(), Request{
		Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Sources[0].Store != "cloud" {
		t.Fatalf("steps = %+v, want the cloud read: the site has one of the authority's two referrers", res.Steps)
	}
}

// What gantry puts in a shared cache is read by later jobs that asked for
// something else, so a fill carries the referrers whatever the job asked for —
// the same rule as verbatim and platforms. An ENGINE target cannot ask at all,
// and needs them: serve.enforce re-verifies what the node holds against the store
// whose host the daemon recorded, which on a routed job is the cache.
func TestRoutedFillCarriesReferrersEvenForAnEngineTarget(t *testing.T) {
	cloud, site := startReferrersRegistry(t), startReferrersRegistry(t)
	cloudCfg := config.StoreConfig{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"}
	siteCfg := config.StoreConfig{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"}
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js := newCopier(t, []config.StoreConfig{cloudCfg, siteCfg}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)

	src := pushImage(t, cloud+"/team/app:1", 1)
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	attachReferrer(t, cloudCfg, src.Context(), desc)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 2 || done.Transfers[0].Store != "site" {
		t.Fatalf("transfers = %+v, want a fill hop into the site then the pull", done.Transfers)
	}
	siteRepo, err := name.NewRepository(site+"/team/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listReferrers(t, siteCfg, siteRepo, desc.Digest)); n != 1 {
		t.Errorf("the cache holds %d referrers, want the fill to have carried the authority's one", n)
	}
}

// A fill already in flight is invisible to the probe — it has published nothing
// yet — so a second cold job would stream the same image out of the authority
// again, which is the egress the whole feature exists to spend once. Read the
// cache instead and let the attempt wait that fill out.
func TestRouteDoesNotPlanAFillSomeoneElseIsAlreadyRunning(t *testing.T) {
	w, js, cloud, site, _ := routedCopier(t)
	pushImage(t, cloud+"/team/app:1", 1)

	// A job that is, right now, putting exactly this image into the site.
	filling := NewJob("job_filler", cloud+"/team/app:1", nil, time.Now())
	filling.State = JobRunning
	filling.Fills = []string{site + "/team/app:1"}
	if err := js.Add(filling); err != nil {
		t.Fatal(err)
	}

	p, err := w.plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.steps) != 1 {
		t.Fatalf("steps = %d, want one: somebody is already filling the site", len(p.steps))
	}
	at := p.steps[0].attempts[0]
	if at.why != whyRoute || at.src.Name != "site" {
		t.Fatalf("first attempt = %+v, want a read of the site", at)
	}
	if at.waitFill != site+"/team/app:1" {
		t.Errorf("waitFill = %q, want the reference the in-flight fill publishes", at.waitFill)
	}
	if len(at.needs) != 0 {
		t.Errorf("needs = %v, want none: this job plans no fill of its own", at.needs)
	}

	// With nothing filling it, the same request plans the fill itself.
	if !js.Delete("job_filler") {
		t.Fatal("could not remove the filling job")
	}
	p, err = w.plan(context.Background(), Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.steps) != 2 {
		t.Fatalf("steps = %d, want a fill hop and a delivery hop", len(p.steps))
	}
}

// copy_referrers decides what a caller is SERVED — the image is identical either
// way and the signature simply absent — so it splits the dedup key like every
// other flag of that kind.
func TestCopyReferrersSplitsTheDedupKey(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	w.base = context.Background() // enqueue without workers; jobs stay active
	pushImage(t, cloud+"/team/app:1", 1)

	req := Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local", CopyReferrers: boolp(false)}
	if _, created, err := w.Submit(req); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}
	if _, created, err := w.Submit(req); err != nil || created {
		t.Fatalf("an identical submit should coalesce: created=%v err=%v", created, err)
	}
	withRefs := req
	withRefs.CopyReferrers = boolp(true)
	if _, created, err := w.Submit(withRefs); err != nil || !created {
		t.Fatalf("a submit that needs the signatures must not coalesce onto one that drops them: created=%v err=%v", created, err)
	}
}

// A verbatim commit pushes each child manifest at the destination before the
// index. The child reference must inherit the destination's own scheme: re-parsing
// it from the destination's NAME drops that, and pushes the children over https
// into a plain-HTTP cache while the index goes over http.
func TestVerbatimCommitAddressesChildrenWithTheDestinationsScheme(t *testing.T) {
	cloud, local := startRegistry(t), startRegistry(t)
	site := startRegistryOffLoopback(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	// An INDEX, so the fill's verbatim commit has children to push.
	pushIndex(t, cloud+"/team/app:1", "linux/amd64", "linux/arm64")

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	// The fill is optional, so a broken commit would still leave the JOB done —
	// the fill hop's own state and the cache's contents are what tell the truth.
	if len(done.Transfers) != 2 || done.Transfers[0].State != "done" {
		t.Fatalf("fill hop = %+v, want it to have committed", done.Transfers[0])
	}
	if !hasTagAt(t, site, "team/app", "1") {
		t.Error("the site should hold the index after a verbatim fill")
	}
}

// A destination that refuses the write is not a fault of whoever was reading, so
// it must not be answered by re-reading the whole image from another source: the
// second attempt would be refused identically, and gantry would blame its own
// cache in the metric and the audit event for the target's outage.
func TestATargetThatRefusesTheWriteIsNotBlamedOnTheSource(t *testing.T) {
	cloud, site := startRegistry(t), startRegistry(t)
	local := startRefusingRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		{Name: "local", Kind: "oci", Host: local, Insecure: true, Mode: "copy"},
	}, false)
	pushImage(t, cloud+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobFailed {
		t.Fatalf("state = %q, want failed: the caller's target refused the image", done.State)
	}
	var delivery []TransferSnapshot
	for _, tr := range done.Transfers {
		if tr.Store == "local" {
			delivery = append(delivery, tr)
		}
	}
	if len(delivery) != 1 {
		t.Fatalf("delivery attempts = %d (%+v), want one: no source can substitute for a target that refused the write", len(delivery), delivery)
	}
}

// require_authority "is a no-op for a job that is not routed". A job whose engine
// reaches every source by one host can never read the cache, so it is not routed
// whatever the authority says — and must not be refused for the authority's
// silence. Settled before require_authority is asked, exactly as on the confirmed
// path.
func TestUnroutableJobIsNotRefusedForAnUnconfirmedAuthority(t *testing.T) {
	site, edge := startRegistry(t), startRegistry(t)
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: "127.0.0.1:1", Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
		// A pull-through TARGET fetches from its own upstream and ignores whatever
		// source gantry hands it, so naming the cache changes nothing.
		{Name: "edge", Kind: "oci", Host: edge, Insecure: true, Mode: "proxy"},
	}, false)
	// An engine that reaches every source by one host is the other shape that
	// cannot be routed.
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker", PullHost: "mirror.local"}, eng)

	for _, target := range []string{"edge", "node"} {
		t.Run(target, func(t *testing.T) {
			res, err := w.Plan(context.Background(), Request{
				Ref: "127.0.0.1:1/team/app:1", Source: "cloud", Target: target, RequireAuthority: boolp(true),
			})
			if err != nil {
				t.Fatalf("plan: %v — this job cannot read the cache, so it is not routed", err)
			}
			if len(res.Steps) != 1 || len(res.Steps[0].Sources) != 1 || res.Steps[0].Sources[0].Store != "cloud" {
				t.Fatalf("steps = %+v, want one unrouted hop", res.Steps)
			}
		})
	}
}
