package cpx

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
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

// When the authority cannot say what a tag means, reading the cache by tag is the
// useful answer — and the only case a caller can receive content the authority
// never confirmed, so require_authority refuses it.
func TestRequireAuthorityGovernsAnUnreachableAuthority(t *testing.T) {
	w, _, cloud, _, _ := routedCopier(t)
	// Nothing was pushed, so the cloud cannot resolve the tag.
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
