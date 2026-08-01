//go:build e2e

// The real-daemon tier for the two routing features (docs/examples.md): source
// fallback and the routed copy through a store's declared cache. The hermetic
// tier proves the plan; these prove the plan survives contact with a real
// registry and a real docker daemon — a registry's own 404 travelling out of
// the daemon's pull stream, a registry that refuses writes because it is
// read-only rather than because a fake said so, and an origin that is gone
// because its container is gone.
package e2e

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	orasremote "oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"github.com/lesomnus/z"
	"google.golang.org/protobuf/proto"
)

// --- helpers ---------------------------------------------------------------

// seedInto pushes ONE random image to every given host under repo:tag, so a
// cache and its origin hold the same digest — which is what a warm-cache probe
// asks about. seedImage cannot do this: it randomizes per call.
func seedInto(t *testing.T, repo, tag string, sizeBytes int, hosts ...string) v1.Hash {
	t.Helper()
	img, err := random.Image(int64(sizeBytes), 2)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	for _, h := range hosts {
		if err := remote.Write(insecureTag(t, h, repo, tag), img); err != nil {
			t.Fatalf("push seed image to %s: %v", h, err)
		}
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func orasRepoAt(t *testing.T, host, repo string) *orasremote.Repository {
	t.Helper()
	r, err := orasremote.NewRepository(host + "/" + repo)
	if err != nil {
		t.Fatalf("oras repo %s/%s: %v", host, repo, err)
	}
	r.PlainHTTP = true
	r.Client = &auth.Client{Cache: auth.NewCache()}
	return r
}

// attachReferrer pushes an artifact whose subject is host/repo@subject, the
// shape a notation signature has. oras picks the referrers API or the fallback
// tag scheme depending on what the registry supports, which is the same choice
// gantry's referrer copy makes.
func attachReferrerAt(t *testing.T, host, repo string, subject v1.Hash) {
	t.Helper()
	ctx := context.Background()
	r := orasRepoAt(t, host, repo)
	desc, err := r.Resolve(ctx, subject.String())
	if err != nil {
		t.Fatalf("resolve subject %s: %v", subject, err)
	}
	blob := []byte(`{"sig":"l2"}`)
	bd := ocispec.Descriptor{
		MediaType: "application/vnd.test.signature",
		Digest:    godigest.FromBytes(blob),
		Size:      int64(len(blob)),
	}
	if err := r.Push(ctx, bd, bytes.NewReader(blob)); err != nil {
		t.Fatalf("push referrer blob: %v", err)
	}
	if _, err := oras.PackManifest(ctx, r, oras.PackManifestVersion1_1, "application/vnd.test.signature",
		oras.PackManifestOptions{Subject: &desc, Layers: []ocispec.Descriptor{bd}}); err != nil {
		t.Fatalf("pack referrer: %v", err)
	}
}

func countReferrersAt(t *testing.T, host, repo string, subject v1.Hash) int {
	t.Helper()
	ctx := context.Background()
	r := orasRepoAt(t, host, repo)
	desc, err := r.Resolve(ctx, subject.String())
	if err != nil {
		t.Fatalf("resolve subject %s at %s: %v", subject, host, err)
	}
	n := 0
	if err := r.Referrers(ctx, desc, "", func(ds []ocispec.Descriptor) error {
		n += len(ds)
		return nil
	}); err != nil {
		t.Fatalf("list referrers at %s: %v", host, err)
	}
	return n
}

// daemonHas reports whether the docker daemon holds a reference.
func (h *l2harness) daemonHas(ref string) bool {
	h.t.Helper()
	_, err := h.cli.ImageInspect(context.Background(), ref)
	return err == nil
}

// pullJob is an engine pull of ref out of source.
func pullJob(ref, source string, fallback bool) *pb.JobAddRequest {
	b := pb.JobAddRequest_builder{
		Ref:    ref,
		Source: pb.StoreByName(source),
		Target: pb.StoreByName("edge"),
	}
	if fallback {
		b.FallbackToOrigin = proto.Bool(true)
	}
	return b.Build()
}

// describe renders a job's transfers as "store◀──source:state" for failure
// messages, so a broken expectation shows the route that actually ran.
func describe(job *pb.Job) string {
	var b strings.Builder
	for i, tr := range job.GetTransfers() {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(tr.GetStore())
		b.WriteString("◀──")
		b.WriteString(tr.GetSource())
		b.WriteString(":")
		b.WriteString(tr.GetState().String())
		if e := tr.GetError(); e != "" {
			b.WriteString("(" + e + ")")
		}
	}
	return b.String()
}

func lastTransfer(job *pb.Job) *pb.Transfer {
	tr := job.GetTransfers()
	if len(tr) == 0 {
		return nil
	}
	return tr[len(tr)-1]
}

// --- feature 1: source fallback --------------------------------------------

// A cache that really answers 404 does not fail the node's pull: the daemon's
// own error surfaces out of the pull stream, gantry reads it as the source's
// fault rather than the daemon's, and the origin named in the job's ref serves
// the image instead.
func TestL2EnginePullFallsBackToOrigin(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.remote, "lib/app", "1")
	if hasTag(t, h.cache, "lib/app", "1") {
		t.Fatal("the cache must be empty for this test to mean anything")
	}
	originRef := h.remote + "/lib/app:1"
	h.removeImage(originRef)

	job := h.waitDone(h.add(pullJob(originRef, "cache", true)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q transfers=[%s], want a done job — a real cache miss is not a job failure",
			job.GetState(), job.GetError(), describe(job))
	}
	tr := job.GetTransfers()
	if len(tr) != 2 {
		t.Fatalf("transfers = %d [%s], want one per attempt", len(tr), describe(job))
	}
	if tr[0].GetSource() != "cache" || tr[0].GetState() != pb.TransferState_TRANSFER_STATE_FAILED {
		t.Errorf("transfer[0] = %s, want the failed cache attempt", describe(job))
	}
	// The daemon's message, not a synthetic one: this is the whole point of the
	// tier — the hermetic engine cannot produce a registry's own wording.
	if e := tr[0].GetError(); !strings.Contains(strings.ToLower(e), "manifest") &&
		!strings.Contains(strings.ToLower(e), "not found") {
		t.Errorf("transfer[0] error = %q, want the registry's own miss reported through the daemon", e)
	}
	if tr[1].GetSource() != "remote" || tr[1].GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("transfer[1] = %s, want the origin attempt to have served it", describe(job))
	}
	if !h.daemonHas(originRef) {
		t.Errorf("the daemon does not hold %s", originRef)
	}
}

// Without the flag the same setup fails, and the daemon is never sent to the
// origin.
func TestL2EnginePullWithoutFallbackDoesNotTouchTheOrigin(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.remote, "lib/app", "1")
	originRef := h.remote + "/lib/app:1"
	h.removeImage(originRef)

	job := h.waitDone(h.add(pullJob(originRef, "cache", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("state=%v [%s], want failed", job.GetState(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 1 {
		t.Errorf("transfers = %d [%s], want 1 — no second attempt was permitted", n, describe(job))
	}
	if h.daemonHas(originRef) {
		t.Errorf("the daemon holds %s; it was sent to the origin without the flag", originRef)
	}
}

// When nothing can serve it, both real registries' refusals end up on the
// record and the job reports the source it was admitted for.
func TestL2FallbackReportsBothAttemptsWhenNothingServes(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.remote, "lib/app", "1") // a different tag than the one asked for

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:nope", "cache", true)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("state=%v [%s], want failed when no source has it", job.GetState(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 2 {
		t.Errorf("transfers = %d [%s], want both attempts on the record", n, describe(job))
	}
	if got := job.GetSource().GetName(); got != "cache" {
		t.Errorf("job source = %q, want the requested source", got)
	}
	if e := job.GetError(); !strings.Contains(e, "cache") || !strings.Contains(e, "remote") {
		t.Errorf("job error = %q, want both attempts' failures reported", e)
	}
}

// worker.source_wait against a real fill: a second job whose source is cold
// finds the fill in flight, waits it out, and is served by the same cache
// rather than reading the origin a second time. The origin is throttled so the
// fill is slow enough for the window to be real rather than lucky.
func TestL2SourceWaitJoinsAnInFlightFill(t *testing.T) {
	const rate = 2 << 20 // bytes/sec through the origin
	h := newL2Harness(t,
		l2WithThrottledOrigin(rate),
		l2WithWorker(config.WorkerConfig{
			MaxConcurrentJobs: 4,
			SourceWait:        z.Ptr(config.Duration(90 * time.Second)),
		}),
	)
	// ~8 MiB over a 2 MiB/s link: a few seconds of fill, plenty of window.
	seedInto(t, "lib/app", "1", 4<<20, h.originHost)
	cacheRef := h.cache + "/lib/app:1"
	h.removeImage(cacheRef)

	// The fill: origin → cache, read through the throttle.
	fill := h.add(copyReq("remote", "cache"))
	waitRunning(t, h, fill.GetId())

	// The reader: cache → daemon, no fallback. Its first attempt must miss.
	start := time.Now()
	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "cache", false)).GetId())
	waited := time.Since(start)

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s], want the reader to be served once the fill landed",
			job.GetState(), job.GetError(), describe(job))
	}
	tr := job.GetTransfers()
	if len(tr) < 2 {
		t.Fatalf("transfers = %d [%s], want the missed attempt and the retry after the wait",
			len(tr), describe(job))
	}
	for i, x := range tr {
		if x.GetSource() != "cache" {
			t.Fatalf("transfer[%d] source = %q [%s], want every attempt against the cache — "+
				"the reader had no fallback and must not have read the origin", i, x.GetSource(), describe(job))
		}
	}
	if tr[0].GetState() != pb.TransferState_TRANSFER_STATE_FAILED {
		t.Errorf("transfer[0] = %s, want the cold miss that started the wait", describe(job))
	}
	if lastTransfer(job).GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("last transfer = %s, want the retry to have been served", describe(job))
	}
	if !h.daemonHas(cacheRef) {
		t.Errorf("the daemon does not hold %s", cacheRef)
	}
	// It really waited rather than racing through: the fill alone takes seconds.
	if waited < time.Second {
		t.Errorf("the reader finished in %s; the throttled fill cannot have been in flight", waited)
	}
	if j := h.waitDone(fill.GetId()); j.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Errorf("the fill itself failed: %v %q", j.GetState(), j.GetError())
	}
}

// waitRunning blocks until a job has left the queue, so a second job submitted
// after it really does find it in flight.
func waitRunning(t *testing.T, h *l2harness, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		job, err := h.client.Job().Get(context.Background(), pb.JobGetById(id))
		if err != nil {
			t.Fatalf("job get: %v", err)
		}
		if job.GetState() == pb.JobState_JOB_STATE_RUNNING {
			return
		}
		if job.GetState() != pb.JobState_JOB_STATE_PENDING {
			t.Fatalf("job %s reached %v before it could be observed running", id, job.GetState())
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never started", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- feature 2: the routed copy --------------------------------------------

// The declared cache is filled from the origin once and the daemon is then sent
// to the cache — the address the image is pulled from is the proof, and it is
// only observable against a real daemon.
func TestL2RoutedCopyFillsTheCacheThenFeedsTheDaemon(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	seedImage(t, h.remote, "lib/app", "1")
	originRef, cacheRef := h.remote+"/lib/app:1", h.cache+"/lib/app:1"
	h.removeImage(originRef)
	h.removeImage(cacheRef)

	job := h.waitDone(h.add(pullJob(originRef, "remote", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}
	tr := job.GetTransfers()
	if len(tr) != 2 {
		t.Fatalf("transfers = %d [%s], want a fill hop and a delivery hop", len(tr), describe(job))
	}
	if tr[0].GetStep() != 0 || tr[0].GetStore() != "cache" || tr[0].GetSource() != "remote" ||
		tr[0].GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("hop 0 = %s, want cache ◀── remote", describe(job))
	}
	if tr[1].GetStep() != 1 || tr[1].GetStore() != "edge" || tr[1].GetSource() != "cache" ||
		tr[1].GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("hop 1 = %s, want edge ◀── cache", describe(job))
	}
	if !hasTag(t, h.cache, "lib/app", "1") {
		t.Error("the cache does not hold the image after the fill hop")
	}
	if !h.daemonHas(cacheRef) {
		t.Errorf("the daemon does not hold %s — it was not sent to the cache", cacheRef)
	}
	if h.daemonHas(originRef) {
		t.Errorf("the daemon holds %s — the routed pull read the origin directly", originRef)
	}
	// The job still reports what the caller asked for.
	if job.GetSource().GetName() != "remote" || job.GetTarget().GetName() != "edge" {
		t.Errorf("job stores = %q -> %q, want remote -> edge",
			job.GetSource().GetName(), job.GetTarget().GetName())
	}
}

// A cache that already holds the digest is read directly: no second fill, and
// the origin's bytes are not spent again.
func TestL2RoutedCopySkipsTheFillWhenTheCacheIsWarm(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	dg := seedInto(t, "lib/app", "1", 1<<20, h.remote, h.cache)
	cacheRef := h.cache + "/lib/app:1"
	h.removeImage(cacheRef)

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 1 {
		t.Fatalf("transfers = %d [%s], want the delivery alone — the cache was already warm", n, describe(job))
	}
	tr := job.GetTransfers()[0]
	if tr.GetStore() != "edge" || tr.GetSource() != "cache" {
		t.Errorf("hop = %s, want edge ◀── cache", describe(job))
	}
	if !h.daemonHas(cacheRef) {
		t.Errorf("the daemon does not hold %s", cacheRef)
	}
	if got, err := digestByRef(t, h.cache, "lib/app", dg.String()); err != nil || got != dg {
		t.Errorf("cache digest = %v (%v), want the untouched %v", got, err, dg)
	}
}

// The fill carries the origin's referrers even though the caller asked for
// nothing of the sort: what gantry puts in a shared cache is read by later jobs
// that do need them.
func TestL2RoutedFillCarriesReferrers(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	dg := seedImage(t, h.remote, "lib/app", "1")
	attachReferrerAt(t, h.remote, "lib/app", dg)
	if n := countReferrersAt(t, h.remote, "lib/app", dg); n != 1 {
		t.Fatalf("origin holds %d referrers, want the one just attached", n)
	}
	h.removeImage(h.cache + "/lib/app:1")

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}
	if n := countReferrersAt(t, h.cache, "lib/app", dg); n != 1 {
		t.Errorf("the cache holds %d referrers, want the origin's one — a cache filled without them "+
			"makes every later job that needs them decline the route", n)
	}
}

// A cache that refuses writes fails only the hop gantry added for itself: the
// delivery falls through to the source the caller named and the job completes.
func TestL2RoutedCopyStillDeliversWhenTheCacheRefusesWrites(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"), l2WithReadOnlyCache())
	seedImage(t, h.remote, "lib/app", "1")
	originRef := h.remote + "/lib/app:1"
	h.removeImage(originRef)

	job := h.waitDone(h.add(pullJob(originRef, "remote", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s], want the optional fill's failure not to fail the job",
			job.GetState(), job.GetError(), describe(job))
	}
	tr := job.GetTransfers()
	if len(tr) < 2 {
		t.Fatalf("transfers = %d [%s], want the failed fill and the direct delivery", len(tr), describe(job))
	}
	if tr[0].GetStore() != "cache" || tr[0].GetState() != pb.TransferState_TRANSFER_STATE_FAILED {
		t.Errorf("hop 0 = %s, want the fill to have failed against the read-only cache", describe(job))
	}
	last := lastTransfer(job)
	if last.GetStore() != "edge" || last.GetSource() != "remote" ||
		last.GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("last hop = %s, want edge ◀── remote", describe(job))
	}
	if !h.daemonHas(originRef) {
		t.Errorf("the daemon does not hold %s", originRef)
	}
}

// The fill is verbatim whatever the delivery asked for: a job that wants one
// platform still leaves the whole index in the cache, under the authority's own
// digest. A rebuilt index would have a different digest and satisfy neither the
// next job's probe nor a signature over it.
func TestL2RoutedFillIsVerbatimAcrossPlatforms(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	want := seedPlatformIndex(t, h.remote, "lib/multi", "1", "linux/amd64", "linux/arm64")
	h.removeImage(h.cache + "/lib/multi:1")

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:       h.remote + "/lib/multi:1",
		Source:    pb.StoreByName("remote"),
		Target:    pb.StoreByName("edge"),
		Platforms: []string{"linux/amd64"},
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 2 {
		t.Fatalf("transfers = %d [%s], want a fill and a delivery", n, describe(job))
	}
	got, err := digestOf(t, h.cache, "lib/multi", "1")
	if err != nil {
		t.Fatalf("the cache does not hold the index: %v", err)
	}
	if got != want {
		t.Errorf("cache index digest = %v, want the authority's %v — the fill rebuilt it", got, want)
	}
	idx, err := remote.Index(insecureTag(t, h.cache, "lib/multi", "1"))
	if err != nil {
		t.Fatalf("read cache index: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(im.Manifests); n != 2 {
		t.Errorf("cache index lists %d manifests, want both — the fill honored the job's platform filter", n)
	}
}

// Two jobs, one cold cache: the second finds the fill already in flight, adds no
// fill of its own, and is served by the cache the first one filled. Without this
// the image would be streamed out of the origin twice — the egress the whole
// feature exists to spend once.
func TestL2SecondJobJoinsAnInFlightRoutedFill(t *testing.T) {
	h := newL2Harness(t,
		l2WithRemoteCache("cache"),
		l2WithFarStore(),
		l2WithThrottledOrigin(2<<20),
		l2WithWorker(config.WorkerConfig{
			MaxConcurrentJobs: 4,
			SourceWait:        z.Ptr(config.Duration(90 * time.Second)),
		}),
	)
	seedInto(t, "lib/app", "1", 4<<20, h.originHost)

	// The first job routes and fills. Its target is the daemon.
	first := h.add(pullJob(h.remote+"/lib/app:1", "remote", false))
	waitRunning(t, h, first.GetId())

	// A different destination, so this is a second job rather than the same one
	// coalesced — and a registry, so it is not waiting on the daemon either.
	second := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    h.remote + "/lib/app:1",
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("far"),
	}.Build()).GetId())

	if second.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", second.GetState(), second.GetError(), describe(second))
	}
	for _, tr := range second.GetTransfers() {
		if tr.GetStore() == "cache" {
			t.Errorf("the second job filled the cache too [%s] — the fill in flight was not seen",
				describe(second))
		}
	}
	last := lastTransfer(second)
	if last.GetStore() != "far" || last.GetSource() != "cache" ||
		last.GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("last hop = %s, want far ◀── cache — it fell through to the origin instead",
			describe(second))
	}
	if j := h.waitDone(first.GetId()); j.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Errorf("the filling job itself failed: %v %q", j.GetState(), j.GetError())
	}
}

// A cache gantry cannot probe is not routed through at all: the probe is what
// tells a warm cache from a cold one, and without an answer the fill might be a
// whole image copied for nothing. The job reads the source the caller named and
// completes — a cache being down costs nothing but the cache.
func TestL2RoutedCopyDeclinesWhenTheCacheIsUnreachable(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	seedImage(t, h.remote, "lib/app", "1")
	originRef := h.remote + "/lib/app:1"
	h.removeImage(originRef)
	h.kill(h.cacheID, h.cache)

	job := h.waitDone(h.add(pullJob(originRef, "remote", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s], want the job to be served by the origin",
			job.GetState(), job.GetError(), describe(job))
	}
	tr := job.GetTransfers()
	if len(tr) != 1 {
		t.Fatalf("transfers = %d [%s], want the delivery alone — an unprobeable cache is not routed through",
			len(tr), describe(job))
	}
	if tr[0].GetStore() != "edge" || tr[0].GetSource() != "remote" {
		t.Errorf("hop = %s, want edge ◀── remote", describe(job))
	}
	if !h.daemonHas(originRef) {
		t.Errorf("the daemon does not hold %s", originRef)
	}
}

// An engine target needs the origin's referrers, and an origin that cannot
// answer cannot be asked what they are — so the route is declined and the job
// reads the origin it was told to, failing there. This is the asymmetry
// docs/examples.md B6 records: a routed engine job does NOT ride out an origin
// outage the way the two-job model does, even with the cache warm.
func TestL2RoutedEnginePullDeclinesWhenTheOriginIsDown(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"))
	seedInto(t, "lib/app", "1", 1<<20, h.remote, h.cache)
	h.kill(h.remoteID, h.remote)

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("state=%v [%s], want the job to fail at the origin it was told to read",
			job.GetState(), describe(job))
	}
	if n := len(job.GetTransfers()); n != 1 {
		t.Errorf("transfers = %d [%s], want the unrouted delivery alone", n, describe(job))
	}
	if got := job.GetTransfers()[0].GetSource(); got != "remote" {
		t.Errorf("hop source = %q [%s], want the origin", got, describe(job))
	}
}

// The same outage with a registry target, which does not need the referrers:
// the route is taken by tag against the warm cache and the copy completes. This
// is the "the site registry keeps working while the cloud one does not" case.
func TestL2WarmCacheServesARegistryTargetWhileTheOriginIsDown(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"), l2WithFarStore())
	seedInto(t, "lib/app", "1", 1<<20, h.remote, h.cache)
	h.kill(h.remoteID, h.remote)

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    h.remote + "/lib/app:1",
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("far"),
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s], want the cache to have carried the copy",
			job.GetState(), job.GetError(), describe(job))
	}
	last := lastTransfer(job)
	if last.GetStore() != "far" || last.GetSource() != "cache" {
		t.Errorf("last hop = %s, want far ◀── cache", describe(job))
	}
	if !hasTag(t, h.far, "lib/app", "1") {
		t.Error("the target does not hold the image")
	}
}

// The same outage again, with require_authority: serving content the authority
// never confirmed is exactly what the flag refuses, so the job is rejected at
// admission rather than quietly served from the cache. The pair with the test
// above is the whole flag — same setup, opposite answer.
func TestL2RequireAuthorityRejectsAnUnconfirmedRoute(t *testing.T) {
	h := newL2Harness(t, l2WithRemoteCache("cache"), l2WithFarStore())
	seedInto(t, "lib/app", "1", 1<<20, h.remote, h.cache)
	h.kill(h.remoteID, h.remote)

	_, err := h.client.Job().Add(context.Background(), pb.JobAddRequest_builder{
		Ref:              h.remote + "/lib/app:1",
		Source:           pb.StoreByName("remote"),
		Target:           pb.StoreByName("far"),
		RequireAuthority: proto.Bool(true),
	}.Build())
	if err == nil {
		t.Fatal("the job was admitted; require_authority must refuse a reference its authority could not confirm")
	}
	if !strings.Contains(err.Error(), "require_authority") {
		t.Errorf("error = %v, want it to name the flag that refused it", err)
	}
}

// Retention follows the cache edge: a routed pull deliberately lands the image
// on the node under the CACHE's host, so a rule an operator wrote for the origin
// would not match what the node actually holds — and the image would sit there
// `unmanaged` and never be collected. The rule is expanded across the route
// instead.
func TestL2RetentionCoversWhatARoutedPullLeftBehind(t *testing.T) {
	h := newL2Harness(t,
		l2WithRemoteCache("cache"),
		// Written for the ORIGIN. Only the route expansion can make it cover the
		// cache-named image the daemon ends up holding.
		l2WithRetention(config.RetentionRule{Repo: "{remote}/lib/**"}),
	)
	seedImage(t, h.remote, "lib/app", "1")
	cacheRef := h.cache + "/lib/app:1"
	h.removeImage(cacheRef)

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}

	res, err := h.client.Store().GcPlan(context.Background(), pb.StoreGcRequest_builder{
		Store: pb.StoreByName("edge"),
	}.Build())
	if err != nil {
		t.Fatalf("gc plan: %v", err)
	}
	var seen []string
	for _, c := range res.GetDelete() {
		seen = append(seen, c.GetRef()+":delete")
		if c.GetRef() == cacheRef {
			return // managed, and on the delete path — covered either way
		}
	}
	for _, k := range res.GetKeep() {
		seen = append(seen, k.GetRef()+":"+k.GetReason().String())
		if k.GetRef() != cacheRef {
			continue
		}
		if k.GetReason() == pb.GcKeepReason_GC_KEEP_REASON_UNMANAGED {
			t.Fatalf("%s is unmanaged: the origin's rule was not expanded across the route it was fetched through", cacheRef)
		}
		return
	}
	t.Fatalf("%s is not in the GC plan at all; it holds: %v", cacheRef, seen)
}

// A scoped route applies to the repositories it names and to no others.
func TestL2ScopedRouteOnlyAppliesToMatchingRepos(t *testing.T) {
	h := newL2Harness(t, l2WithRoutes(config.CacheRoute{
		Store:    "cache",
		ForRepos: []string{"team/**"},
	}))
	seedImage(t, h.remote, "team/app", "1")
	seedImage(t, h.remote, "lib/app", "1")

	routed := h.waitDone(h.add(pullJob(h.remote+"/team/app:1", "remote", false)).GetId())
	if routed.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("team/app state=%v error=%q [%s]", routed.GetState(), routed.GetError(), describe(routed))
	}
	if n := len(routed.GetTransfers()); n != 2 {
		t.Errorf("team/app transfers = %d [%s], want the route to apply", n, describe(routed))
	} else if routed.GetTransfers()[0].GetStore() != "cache" {
		t.Errorf("team/app hop 0 = %s, want the fill of the cache", describe(routed))
	}

	direct := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())
	if direct.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("lib/app state=%v error=%q [%s]", direct.GetState(), direct.GetError(), describe(direct))
	}
	if n := len(direct.GetTransfers()); n != 1 {
		t.Errorf("lib/app transfers = %d [%s], want no route — the scope does not name it", n, describe(direct))
	}
	if hasTag(t, h.cache, "lib/app", "1") {
		t.Error("the cache holds lib/app; a scoped route filled it for a repository it does not cover")
	}
	if !h.daemonHas(h.remote + "/lib/app:1") {
		t.Error("the daemon does not hold the unrouted image under the origin ref")
	}
}
