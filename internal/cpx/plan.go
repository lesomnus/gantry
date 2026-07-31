package cpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
)

// ErrUnconfirmed is returned when require_authority refuses a job because the
// store named as its source could not confirm what the reference means. Like a
// verification rejection it is a precondition on the environment rather than a
// fault in the request, so the API reports it as FAILED_PRECONDITION.
var ErrUnconfirmed = errors.New("the authority could not confirm the reference")

// maxAttempts bounds how many attempts one job may make in total. Every attempt
// can move a whole image, so this is a cost bound rather than a loop guard: a
// plan that would exceed it is rejected at admission, where it is still cheap.
const maxAttempts = 6

// attemptWhy records why an attempt exists. It is not decoration: leaving the
// source the caller named for the origin is a different fact from gantry
// abandoning a route it chose for its own benefit, and observability has to be
// able to say which happened.
type attemptWhy string

const (
	whyPlanned attemptWhy = "planned" // the source the caller named
	whyRoute   attemptWhy = "route"   // a nearer copy gantry chose to read through
	whyOrigin  attemptWhy = "origin"  // the registry named in the job's own ref
)

// execAttempt is one place a step's bytes can come from.
type execAttempt struct {
	src config.StoreConfig
	// ref is the source-side reference, parsed under src's OWN name options —
	// http-vs-https is baked into a parsed reference by name.Insecure, so an
	// insecure cache must never lend its scheme to a TLS origin nor the reverse —
	// and digest-pinned when the authority confirmed a digest.
	ref name.Reference
	// pullRef is what the daemon is told to pull (engine steps only). It keeps the
	// tag; the digest travels separately and anchors the pull.
	pullRef string
	why     attemptWhy
	// needs are step indices that must have DELIVERED for this attempt to be worth
	// running. It is how a route prunes itself: a delivery attempt that reads a
	// cache needs the step that filled it, so a fill that could not run is followed
	// by the direct copy rather than by a read of an empty cache.
	needs []int
	// waitFill, when set, is the reference a job filling this attempt's source
	// would publish. On a miss the attempt is re-run once, after an in-flight job
	// filling exactly that reference finishes. A runtime discovery, not a planned
	// dependency: a source that can already serve the image never waits.
	waitFill string
}

// execStep is one hop: bytes must end up in dst, and any one of its attempts may
// put them there.
type execStep struct {
	idx int  // position in the plan; stamped on every Transfer row the step produces
	dst dest // pusher (gantry copies the blobs in) or puller (the daemon pulls)

	// ref is the reference this step lands in dst — a registry step's in-store
	// ref. nil for an engine step, whose per-attempt pull ref is the only
	// reference there is.
	ref name.Reference
	// platforms is what THIS hop moves, which is not always what the job asked
	// for: a verbatim commit writes every child manifest of the index, so a hop
	// that commits verbatim copies every platform even when a later hop narrows.
	platforms []string
	verbatim  bool // registry step: commit the source manifest/index byte-for-byte
	referrers bool // registry step: copy the source's referrer artifacts too
	// fills is the reference this step publishes into dst — the one thing another
	// job's read could be waiting on. Empty for an engine step, which publishes
	// nothing another job reads.
	fills string
	// optional marks a step gantry added for its own benefit. Its failure is
	// recorded and tolerated, because the caller asked for the last step and a
	// route that does not work is not a failure. A required step's failure ends
	// the job.
	optional bool
	// newMover builds the runner for one of this step's attempts. Assigned where
	// the step is built, which is the one place that knows whether dst is pushed
	// into or pulls for itself — so nothing downstream switches on store kind, and
	// a registry step cannot reach the engine retention hook at all.
	newMover func(w *Copier, at *execAttempt) (mover, error)

	attempts []*execAttempt
}

// execPlan is what a job will do, resolved once at admission: an ordered sequence
// of steps, each with an ordered list of alternative sources. Steps are a
// sequence — every required one must succeed, in order. Attempts are
// alternatives — the first that succeeds ends the step. Every shape gantry runs
// is a point on those two axes, so nothing downstream carries a per-shape branch.
//
// Steps are coupled only by whether an earlier one DELIVERED (execAttempt.needs).
// No data flows between them: every reference a later step needs is derived here,
// from store config, the repository, the identifier and the pinned digest. That is
// what keeps the plan immutable, and what lets Plan report the whole route before
// a byte moves.
type execPlan struct {
	// source and target are what the caller asked for, resolved. They are what the
	// JOB reports as its own; a transfer says where some bytes came from, and one
	// job can have many of those.
	source config.StoreConfig
	target dest
	repo   string // repository path, shared by every hop
	id     string // ":tag" or "@digest", as requested
	// authorityRef is the reference at source, digest-pinned once the authority has
	// confirmed a digest. Named for what it is because it is NOT necessarily any
	// step's source ref: a routed job's first delivery attempt reads a cache.
	authorityRef name.Reference
	// platforms is the effective set the job records and dedups on. A step carries
	// its own, which may legitimately differ.
	platforms []string

	as       []string // engine step: names the image is recorded under
	asDigest bool     // `as` holds digest refs, so an anchor blob must be fetched

	copyReferrers bool // a job property, applied per registry step
	// needReferrers says a routed read must be able to supply everything the
	// authority holds over the image. It is NOT the same question as
	// copyReferrers: an engine target cannot ask for referrer propagation (there
	// is no referrer transport in a daemon pull) and yet the signatures still
	// matter to it, because serve.enforce re-verifies what the node holds against
	// the store whose HOST the daemon recorded — which, on a routed job, is the
	// cache. So the caller can only ever widen this, never narrow it away.
	needReferrers bool
	verification  *VerificationSnapshot // admission-time verification, stamped on the Job
	// fallback is the EFFECTIVE fallback_to_origin decision: true exactly when an
	// origin attempt was actually bound. Part of the dedup key, so a job that
	// refused the origin never coalesces onto one that allows it — and, being the
	// effective value, two jobs that provably behave the same still do.
	fallback bool
	// fallbackAsked records that the REQUEST asked, so a fallback this job cannot
	// express is an error the caller hears about while an inherited server default
	// simply does not apply to this job.
	fallbackAsked bool
	// strictAuthority is the effective require_authority decision. It is part of
	// the dedup key: a caller that refused content the authority never confirmed
	// must not be handed a job that accepted it.
	strictAuthority bool

	steps []*execStep
}

// last is the step that delivers what the caller asked for.
func (p *execPlan) last() *execStep { return p.steps[len(p.steps)-1] }

// pin re-anchors the plan to a confirmed digest. Called after verification, and
// before any step's attempts are built, so every hop inherits it.
func (p *execPlan) pin(digest string) {
	p.authorityRef = p.authorityRef.Context().Digest(digest)
}

// digest is the digest every hop is anchored to, or "" for a job that runs by tag.
func (p *execPlan) digest() string {
	if dg, ok := p.authorityRef.(name.Digest); ok {
		return dg.DigestStr()
	}
	return ""
}

// fills are the references this job's steps publish into their targets.
func (p *execPlan) fills() []string {
	var out []string
	for _, st := range p.steps {
		if st.fills != "" {
			out = append(out, st.fills)
		}
	}
	return out
}

// rowRef is what a row reports as "the reference placed in the target": the
// in-store ref for a registry step, which every attempt shares, or the ref the
// daemon was told to pull, which is per attempt.
func (st *execStep) rowRef(at *execAttempt) string {
	if st.ref != nil {
		return st.ref.Name()
	}
	return at.pullRef
}

// platform is the single platform an engine step pulls; plan() resolves it to the
// daemon host's own when the request names none.
func (st *execStep) platform() string {
	if len(st.platforms) == 0 {
		return ""
	}
	return st.platforms[0]
}

// seed is the row published for a step before it runs: the route as intended,
// visible to a client watching a job that has not started.
func (st *execStep) seed() *Transfer {
	at := st.attempts[0]
	return &Transfer{
		Step: st.idx, Store: st.dst.Name(), Kind: st.dst.Kind(),
		Source: at.src.Name, Ref: st.rowRef(at), State: "pending",
	}
}

// validate checks the invariants a plan must hold before anything runs. They are
// cheap here and expensive to debug from a client reporting an intermediate store
// as a job's target.
func (p *execPlan) validate() error {
	if len(p.steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}
	n := 0
	for i, st := range p.steps {
		if st.idx != i {
			return fmt.Errorf("step %d is indexed %d", i, st.idx)
		}
		if len(st.attempts) == 0 {
			return fmt.Errorf("step %d has no attempts", i)
		}
		if st.newMover == nil {
			return fmt.Errorf("step %d has no runner", i)
		}
		n += len(st.attempts)
		// Only the last step may deliver to the job's target: an earlier one doing
		// so means the route was assembled wrong, and the job would report a
		// finished move while a later hop overwrote it.
		if isLast := i == len(p.steps)-1; isLast != (st.dst.Name() == p.target.Name()) {
			return fmt.Errorf("step %d targets %q; only the last step may target the job's target %q",
				i, st.dst.Name(), p.target.Name())
		}
	}
	if n > maxAttempts {
		return fmt.Errorf("plan would make %d attempts, more than the %d allowed; each attempt can move a whole image", n, maxAttempts)
	}
	return nil
}

// planAttemptRef builds a source-side reference at src for the plan's image,
// preserving whatever the plan is anchored to. It is the single place a
// source-side reference is built, for every store of every step, and it parses
// under src's OWN name options: http-vs-https is baked into a parsed reference,
// so an insecure cache must not lend its scheme to a TLS origin nor the reverse.
func (w *Copier) planAttemptRef(p *execPlan, src config.StoreConfig) (name.Reference, error) {
	id := p.id
	if dg := p.digest(); dg != "" {
		id = "@" + dg
	}
	ref, err := name.ParseReference(src.Host+"/"+p.repo+id, w.refOpts(src)...)
	if err != nil {
		return nil, z.Err(err, "source ref at %q", src.Name)
	}
	return ref, nil
}

// plan resolves a request into the plan that will run it.
func (w *Copier) plan(ctx context.Context, req Request) (*execPlan, error) {
	base, err := name.ParseReference(req.Ref, w.srcOpts...)
	if err != nil {
		return nil, z.Err(err, "parse ref %q", req.Ref)
	}
	p := &execPlan{repo: base.Context().RepositoryStr(), id: identifier(base)}

	sourceKey := req.Source
	if sourceKey == "" {
		sourceKey = base.Context().RegistryStr()
	}
	if p.source, err = w.stores.Registry(sourceKey); err != nil {
		return nil, z.Err(err, "source")
	}
	if p.authorityRef, err = name.ParseReference(p.source.Host+"/"+p.repo+p.id, w.refOpts(p.source)...); err != nil {
		return nil, z.Err(err, "source ref")
	}

	if req.Target == "" {
		return nil, fmt.Errorf("job has nothing to do: set `target`")
	}
	if p.target, err = resolveDest(w.stores, req.Target); err != nil {
		return nil, z.Err(err, "target")
	}

	// The fallback is an engine-pull property: a registry target's source is
	// normally the origin already, so there is nothing to fall back to. Resolved
	// here because it is part of the dedup key.
	if _, isPuller := p.target.(puller); isPuller {
		p.fallback = w.wc.FallbackToOrigin
		if req.FallbackToOrigin != nil {
			p.fallback = *req.FallbackToOrigin
			p.fallbackAsked = *req.FallbackToOrigin
		}
	} else if req.FallbackToOrigin != nil && *req.FallbackToOrigin {
		return nil, fmt.Errorf("fallback_to_origin applies to an engine target; store %q is a registry", p.target.Name())
	}
	p.strictAuthority = w.wc.RequireAuthority
	if req.RequireAuthority != nil {
		p.strictAuthority = *req.RequireAuthority
	}

	// The delivery step, built from the requested (tag) form: a registry target's
	// in-store ref stays tag-named, and an engine's pull ref is derived from it
	// too, with only the anchoring digest travelling separately.
	deliver := &execStep{idx: 0, dst: p.target}
	p.platforms = req.Platforms
	var asDigests []name.Digest
	switch d := p.target.(type) {
	case pusher:
		if len(req.As) > 0 {
			return nil, fmt.Errorf("`as` names the image on an engine; store %q is a registry", d.Name())
		}
		if deliver.ref, err = d.dstRef(p.authorityRef); err != nil {
			return nil, z.Err(err, "destination ref for %q", d.Name())
		}
		// A digest-named in-store ref keeps the source digest, so a copy-mode
		// commit must preserve the source manifest/index byte-for-byte — a rebuilt
		// (platform-filtered) index has a different digest and the registry would
		// reject the put. Proxy mode commits nothing, so it is exempt.
		isDigestDst := false
		if _, ok := deliver.ref.(name.Digest); ok {
			rd, isRegistry := p.target.(*registryDest)
			isDigestDst = !isRegistry || !rd.isProxy()
		}
		if isDigestDst {
			if len(req.Platforms) > 0 {
				return nil, fmt.Errorf("a digest-pinned copy preserves the source image verbatim (all platforms); omit platforms")
			}
			deliver.verbatim = true
		}
		deliver.fills = deliver.ref.Name()
		deliver.newMover = newCopyMover(deliver)
	case puller:
		// An engine pulls exactly one platform; an empty request means the daemon
		// host's own. The value is handed to the daemon as-is — a platform the
		// image lacks is the daemon's error, surfaced by the pull.
		if len(p.platforms) > 1 {
			return nil, fmt.Errorf("engine destination %q pulls a single platform; got %d", d.Name(), len(p.platforms))
		}
		if len(p.platforms) == 0 {
			host, err := d.hostPlatform(ctx)
			if err != nil {
				return nil, z.Err(err, "resolve host platform of %q", d.Name())
			}
			p.platforms = []string{host}
		}
		// The image is recorded under the requested names — so a cache-fed engine
		// can keep the upstream name — instead of the pull reference. The strings
		// are kept VERBATIM: containerd resolves image names by exact match, so
		// normalizing (docker.io -> index.docker.io) would break kubelet lookups.
		// A digest reference is admitted too, and validated below once verification
		// has had its say.
		for _, n := range req.As {
			parsed, err := name.ParseReference(n, w.srcOpts...)
			if err != nil {
				return nil, z.Err(err, "parse `as` name %q", n)
			}
			if dg, ok := parsed.(name.Digest); ok {
				asDigests = append(asDigests, dg)
				p.asDigest = true
			}
			p.as = append(p.as, n)
		}
		deliver.newMover = newPullMover(p, deliver)
	default:
		return nil, fmt.Errorf("store %q can neither be pushed to nor pull", p.target.Name())
	}
	deliver.platforms = p.platforms

	// Verify the source signature (fail-closed) before admitting the job, and pin
	// the plan to the verified digest so every hop covers exactly what was
	// verified. Runs after the delivery ref is derived from the tag.
	verified := false
	if w.verifier != nil {
		res, err := w.verifier.Verify(ctx, p.source, p.authorityRef)
		if err != nil {
			return nil, err // sentinel (ErrUnsigned/ErrUntrusted) preserved for the handler
		}
		p.verification = &VerificationSnapshot{Mode: string(res.Mode), Verified: res.Verified()}
		if dg := res.Digest; dg.Hex != "" {
			verified = true
			p.verification.Digest = dg.String()
			// A proxy-mode destination reads through by tag and ignores the pinned
			// digest, so it could fill from a different (unverified) image if the tag
			// moves after verification. Refuse rather than move unverified bytes.
			if rd, ok := p.target.(*registryDest); ok && rd.isProxy() {
				return nil, fmt.Errorf("signature verification requires a copy-mode destination; store %q is proxy", rd.Name())
			}
			p.pin(dg.String())
			log.From(w.rootCtx()).Info("source signature verified",
				slog.String("ref", req.Ref), slog.String("source", p.source.Name), slog.String("digest", dg.String()))
		}
	}

	// A digest `as` name is honest only when it names exactly what the engine
	// pulls: it requires an anchored pull and must carry that anchor digest.
	if len(asDigests) > 0 {
		dg := p.digest()
		if dg == "" {
			return nil, fmt.Errorf("digest `as` names require a digest-pinned job (a digest ref, or a verified source)")
		}
		for _, d := range asDigests {
			if d.DigestStr() != dg {
				return nil, fmt.Errorf("`as` name %q does not carry the job's pinned digest %s", d.Name(), dg)
			}
		}
	}

	// Referrer propagation (signatures travel with the image): copy the source's
	// referrer artifacts along with it, with the source digest preserved — which
	// requires copying the image verbatim, i.e. every platform. Registry
	// destinations only: an engine pull has no referrer transport.
	rd, isRegistry := p.target.(*registryDest)
	if req.CopyReferrers != nil && *req.CopyReferrers && !isRegistry {
		return nil, fmt.Errorf("copy_referrers requires a registry destination; store %q is an engine", p.target.Name())
	}
	if isRegistry {
		if req.CopyReferrers != nil {
			p.copyReferrers = *req.CopyReferrers
		} else {
			// Default on only when this job actually verified a signature and the
			// request did not narrow the platform set: the pinned digest still
			// protects a narrowed copy, only signature propagation is skipped.
			p.copyReferrers = verified && !rd.isProxy() && len(req.Platforms) == 0
		}
		if p.copyReferrers {
			if rd.isProxy() {
				return nil, fmt.Errorf("copy_referrers requires a copy-mode destination; store %q is proxy", rd.Name())
			}
			if len(req.Platforms) > 0 {
				return nil, fmt.Errorf("copy_referrers preserves the source image verbatim (all platforms); omit platforms or set copy_referrers to false")
			}
			p.platforms = nil // the verbatim commit needs every child manifest present
			deliver.platforms = nil
			deliver.verbatim = true
		}
		deliver.referrers = p.copyReferrers
	}
	// Whether a routed read has to be able to supply the referrers. A registry
	// target says so with copy_referrers. An engine target has no way to say it —
	// a daemon pull has no referrer transport — and needs it regardless: the node
	// records the host it was told to pull from, and serve.enforce re-verifies what
	// the node holds against the store that host resolves to. Route it through a
	// cache with no signatures and a live verifier finds an unsigned image.
	_, isPuller := p.target.(puller)
	p.needReferrers = p.copyReferrers || isPuller

	// Attempts last, so every one of them inherits whatever verification pinned: a
	// job that falls back reaches for the very digest it verified, from a different
	// host.
	p.steps = []*execStep{deliver}
	if err := w.bindDelivery(ctx, p, deliver, base.Context().RegistryStr()); err != nil {
		return nil, err
	}
	// Routing last: it reads the authority to settle the tag, which re-anchors
	// every attempt built above, and it prepends its own read of the cache ahead
	// of the source the caller named.
	if err := w.route(ctx, p, req); err != nil {
		return nil, err
	}
	if err := p.validate(); err != nil {
		return nil, z.Err(err, "plan")
	}
	return p, nil
}

// bindDelivery gives the delivery step its attempts: the source the caller named,
// then — when the job allows a fallback — the origin registry named in the job's
// own ref. `source` is only ever an override of that origin, so the fallback
// needs no new input from the caller.
func (w *Copier) bindDelivery(ctx context.Context, p *execPlan, st *execStep, originHost string) error {
	planned := &execAttempt{src: p.source, ref: p.authorityRef, why: whyPlanned}
	// The tag form at this source, which two things need: the reference a fill of
	// it would publish — a registry target derives its in-store ref from the tag
	// too, so the tag is the one string both sides of a fill/read pair agree on —
	// and the pull ref, which keeps the tag because the digest travels separately
	// and anchors the pull.
	tagRef, err := name.ParseReference(p.source.Host+"/"+p.repo+p.id, w.refOpts(p.source)...)
	if err != nil {
		return z.Err(err, "source ref")
	}
	planned.waitFill = tagRef.Name()
	if pd, ok := st.dst.(puller); ok {
		if planned.pullRef, err = pd.pullRef(tagRef, p.source); err != nil {
			return err
		}
	}
	st.attempts = []*execAttempt{planned}

	if !p.fallback {
		return nil
	}
	// unavailable reports a fallback this job cannot express. Asked for explicitly
	// it is an error; inherited from the server default it simply does not apply
	// here, since one job's undeclared origin must not take down every job a
	// global default touches. Either way it is never silent: the job runs with a
	// single attempt and Plan reports no fallback ref.
	unavailable := func(err error) error {
		if p.fallbackAsked {
			return err
		}
		p.fallback = false
		log.From(ctx).Info("fallback to origin does not apply to this job",
			slog.String("ref", p.repo+p.id), slog.String("source", p.source.Name), slog.String("reason", err.Error()))
		return nil
	}

	origin, err := w.stores.Registry(originHost)
	if err != nil {
		return unavailable(z.Err(err, "fallback origin"))
	}
	if origin.Name == p.source.Name {
		// Already reading from the origin; there is nowhere to fall back to. Not a
		// misconfiguration — a client that always sets the flag hits this on every
		// direct job — so it is not reported as one.
		p.fallback = false
		return nil
	}
	at := &execAttempt{src: origin, why: whyOrigin}
	if at.ref, err = w.planAttemptRef(p, origin); err != nil {
		return unavailable(err)
	}
	if pd, ok := st.dst.(puller); ok {
		// The pull ref keeps the tag — the pinned digest travels separately and
		// anchors the pull — so it is derived the same way the planned one was.
		tagRef, err := name.ParseReference(origin.Host+"/"+p.repo+p.id, w.refOpts(origin)...)
		if err != nil {
			return unavailable(z.Err(err, "fallback origin ref"))
		}
		if at.pullRef, err = pd.pullRef(tagRef, origin); err != nil {
			return unavailable(z.Err(err, "fallback origin pull ref"))
		}
		if at.pullRef == st.attempts[0].pullRef {
			// pull_host (or a downstream_host shared by both stores) collapses every
			// source onto one host as far as the daemon is concerned, so the second
			// attempt would re-pull from the same place. Say so rather than ship a
			// fallback that silently cannot fall back.
			return unavailable(fmt.Errorf("fallback to origin %q is not addressable from engine %q: both sources resolve to the pull ref %q (pull_host / downstream_host)", origin.Name, st.dst.Name(), at.pullRef))
		}
	}
	st.attempts = append(st.attempts, at)
	return nil
}

// route inserts a nearer copy of the source into the plan, when the source
// declares one. It is gantry's own optimization: the caller neither named the
// cache nor sees a different result, so nothing here may change what the job
// delivers — only how many times the authority is read.
//
// The shape depends on one probe:
//
//	warm cache   step 0: target ◀── [cache, source]
//	cold cache   step 0: cache  ◀── [source]                 (optional)
//	             step 1: target ◀── [cache needs:0, source]
//
// Either way the source the caller asked for stays in the list, so a route that
// does not work is not a failure — it costs one abandoned attempt.
func (w *Copier) route(ctx context.Context, p *execPlan, req Request) error {
	l := log.From(ctx)
	deliver := p.last()
	cacheName := p.source.Cache
	if cacheName == "" {
		return w.requireAuthority(p, req, false)
	}
	// Degenerate shapes. None of them is a misconfiguration: an operator declares
	// the cache once on the origin, and jobs that happen to name either end of the
	// route are ordinary.
	switch {
	case cacheName == p.target.Name():
		// Filling the cache IS the copy the caller asked for.
		return w.requireAuthority(p, req, false)
	case cacheName == p.source.Name:
		return w.requireAuthority(p, req, false)
	}
	cacheDest, err := resolveDest(w.stores, cacheName)
	if err != nil {
		// Validated at config load, so this is a store that stopped resolving.
		l.Warn("not routing: the cache store does not resolve",
			slog.String("cache", cacheName), slog.String("error", err.Error()))
		return w.requireAuthority(p, req, false)
	}
	if _, ok := cacheDest.(pusher); !ok {
		l.Warn("not routing: the cache store cannot hold an image", slog.String("cache", cacheName))
		return w.requireAuthority(p, req, false)
	}
	cacheCfg, declared := w.stores.Config(cacheName)
	if !declared {
		// Validated at config load, so this is a store that stopped being declared.
		// A zero StoreConfig has no host, which would route the job at whatever
		// `/repo:tag` parses to — never that.
		l.Warn("not routing: the cache store is not declared", slog.String("cache", cacheName))
		return w.requireAuthority(p, req, false)
	}

	// Settle the tag at the authority. Everything downstream is anchored to this:
	// it makes the nearer copy provably the same content, and it is what the probe
	// asks about. One manifest request against bytes that would otherwise move in
	// full.
	// The two requests below are the only I/O admission does. Bound them together,
	// so an unresponsive registry costs one submit its timeout rather than holding
	// the caller open for as long as the store cares to stay silent.
	if limit := time.Duration(w.wc.AdmissionTimeout); limit > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, limit)
		defer stop()
	}

	digest, derr := resolveDigest(ctx, p.source, p.authorityRef)
	if errors.Is(derr, ErrNoSuchImage) {
		// The authority ANSWERED: it does not have this reference. That is the most
		// definite answer there is, and reading a cache of it instead would serve
		// content the authority never had — under a tag the caller believes points
		// at the authority's image. Do not route; let the job read the source it
		// named and fail there, as an unrouted job would.
		l.Debug("not routing: the authority does not have this reference",
			slog.String("ref", p.authorityRef.Name()), slog.String("source", p.source.Name))
		return nil
	}
	if derr != nil {
		// The authority could not answer at all. Reading the cache by tag is then
		// the useful answer — the site registry keeps working while the cloud one
		// does not — and it is the one case where the caller can receive content the
		// authority never confirmed, so require_authority governs it.
		//
		// Everything that can decline the route is settled BEFORE require_authority
		// is asked, because the flag "is a no-op for a job that is not routed" and a
		// job gantry was never going to route is exactly that. Both checks are the
		// ones the confirmed path makes; the difference is only that there is no
		// digest here to make them with.
		if why, ok := w.unreadableCache(p, deliver, cacheCfg); !ok {
			l.Debug("not routing: the cache could not be read by this job",
				slog.String("cache", cacheName), slog.String("reason", why))
			return nil
		}
		if p.needReferrers {
			// Without a digest the cache cannot be asked whether it holds the
			// referrers, and reading it unasked is how a signature goes missing on a
			// job that reports done. Decline: the job then reads the source it named
			// and fails there, which is what an unrouted job does when its source is
			// unreachable.
			l.Info("not routing: the authority could not confirm the reference and this job needs its referrers",
				slog.String("ref", p.authorityRef.Name()), slog.String("cache", cacheName))
			return nil
		}
		if aerr := w.requireAuthority(p, req, true); aerr != nil {
			// Both halves matter: that it is fatal, and why the authority went quiet.
			return fmt.Errorf("%w: %w", aerr, derr)
		}
		l.Warn("the authority could not confirm the reference; reading the cache by tag",
			slog.String("ref", p.authorityRef.Name()), slog.String("cache", cacheName),
			slog.String("error", derr.Error()))
		return w.addRouteAttempt(ctx, p, deliver, cacheCfg, nil)
	}
	p.pin(digest)
	if err := w.repin(p, deliver); err != nil {
		return err
	}
	if err := w.requireAuthority(p, req, false); err != nil {
		return err
	}

	// Can the delivery hop even read the cache? Settled before anything is filled:
	// an engine whose pull_host collapses every source onto one host reads the same
	// place whichever store gantry names, and a proxy TARGET ignores its source
	// entirely and fetches from its own upstream. Either way the cache would be
	// filled and then never read — a whole image copied for nothing.
	if why, ok := w.unreadableCache(p, deliver, cacheCfg); !ok {
		l.Debug("not routing: the cache could not be read by this job",
			slog.String("cache", cacheName), slog.String("reason", why))
		return nil
	}

	// A pull-through cache fills itself when it is read, so reading it IS the fill:
	// no step, no probe, and no write access to it required. The inverse is a hard
	// guard — a fill step targeting a proxy would read the whole image into
	// io.Discard and commit nothing.
	if rd, isRegistry := cacheDest.(*registryDest); isRegistry && rd.isProxy() {
		if p.needReferrers {
			// What a pull-through cache fills on a read is the image. Whether it
			// also proxies the referrers API is the upstream product's business, not
			// something gantry can establish, and there is no fill step here to carry
			// them instead — so a job whose signatures must travel reads the
			// authority, which certainly has them.
			l.Info("not routing: a pull-through cache cannot be relied on for referrers",
				slog.String("cache", cacheName))
			return nil
		}
		l.Debug("routing through a pull-through cache: reading it is what fills it",
			slog.String("cache", cacheName))
		return w.addRouteAttempt(ctx, p, deliver, cacheCfg, nil)
	}

	cacheRef, err := w.planAttemptRef(p, cacheCfg)
	if err != nil {
		return err
	}
	dg, ok := cacheRef.(name.Digest)
	if !ok {
		return fmt.Errorf("routing needs a digest-anchored reference at %q", cacheName)
	}
	warm, perr := holdsDigest(ctx, cacheCfg, dg)
	if perr != nil {
		l.Warn("not routing: the cache could not be probed",
			slog.String("cache", cacheName), slog.String("error", perr.Error()))
		return nil
	}
	if warm {
		if p.needReferrers && !w.cacheServesReferrers(ctx, p, cacheCfg, dg) {
			// The cache holds the image but not everything the authority has over it
			// — filled before, by a job that did not carry them. Reading it would
			// satisfy the move and drop a signature, so read the authority instead.
			// The image is already there, so nothing is re-transferred by declining.
			l.Info("not routing: the cache does not hold every referrer the authority has",
				slog.String("cache", cacheName), slog.String("digest", digest))
			return nil
		}
		l.Debug("routing through a cache that already holds the image",
			slog.String("cache", cacheName), slog.String("digest", digest))
		return w.addRouteAttempt(ctx, p, deliver, cacheCfg, nil)
	}

	// Cold: fill it first. The fill lands under the TAG, committed verbatim, so the
	// authority's digest resolves from the cache too — which is what the next job's
	// probe asks about, and what anchors every later hop. A rebuilt
	// (platform-filtered) index would have a different digest and satisfy neither.
	tagRef, err := name.ParseReference(cacheCfg.Host+"/"+p.repo+p.id, w.refOpts(cacheCfg)...)
	if err != nil {
		return z.Err(err, "cache ref at %q", cacheName)
	}
	// Unless somebody is already filling it. A second fill would stream the whole
	// image out of the authority again — the egress this feature exists to spend
	// once — and the probe cannot see it, because a fill in flight has published
	// nothing yet. Read the cache instead and let the attempt wait that fill out
	// (worker.source_wait). If waiting is off, or the fill fails, the delivery falls
	// through to the source the caller named, exactly as an abandoned route does:
	// no worse than not routing, and never a duplicated image.
	if _, filling := w.store.Filling(tagRef.Name(), ""); filling {
		l.Debug("not filling: another job is already filling this cache",
			slog.String("cache", cacheName), slog.String("ref", tagRef.Name()))
		return w.addRouteAttempt(ctx, p, deliver, cacheCfg, nil)
	}
	fill := &execStep{
		dst: cacheDest, ref: tagRef,
		verbatim:  true, // so the authority's digest resolves from the cache
		platforms: nil,  // a verbatim commit writes every child manifest
		fills:     tagRef.Name(),
		optional:  true, // gantry added this step for itself
		// Referrers travel on THIS hop, from the authority that has them, whatever
		// the job asked for — the same rule as verbatim and platforms above, and for
		// the same reason: what gantry puts in a shared cache is read by later jobs
		// that asked for something else. A cache filled without them makes every
		// later job that needs them decline the route (or, for an engine target,
		// leaves a node holding an image no host-keyed verifier can check).
		referrers: true,
	}
	fill.newMover = newCopyMover(fill)
	src, err := w.planAttemptRef(p, p.source)
	if err != nil {
		return err
	}
	fill.attempts = []*execAttempt{{src: p.source, ref: src, why: whyPlanned}}

	p.steps = []*execStep{fill, deliver}
	for i, st := range p.steps {
		st.idx = i
	}
	l.Debug("routing through a cache that must be filled first",
		slog.String("cache", cacheName), slog.String("digest", digest))
	return w.addRouteAttempt(ctx, p, deliver, cacheCfg, []int{fill.idx})
}

// cacheServesReferrers reports whether reading the cache instead of the authority
// would deliver everything the authority has over this digest.
//
// It compares counts rather than asking whether the cache has ANY, because "any"
// answers a different question than the one that matters and gets both directions
// wrong: a cache holding one of three signatures reads as complete, and an image
// that legitimately has none reads as deficient — which would decline the route
// for every unsigned image and leave the whole optimization inert while every job
// still reported done. Asking the authority first is also what makes the common
// case cheap: an image with no referrers costs one listing and routes.
func (w *Copier) cacheServesReferrers(ctx context.Context, p *execPlan, cache config.StoreConfig, dg name.Digest) bool {
	l := log.From(ctx)
	authority, ok := p.authorityRef.(name.Digest)
	if !ok {
		return false // unpinned: there is nothing to compare against
	}
	want, err := countReferrers(ctx, p.source, authority)
	if err != nil {
		l.Debug("could not list the authority's referrers",
			slog.String("source", p.source.Name), slog.String("error", err.Error()))
		return false
	}
	if want == 0 {
		return true // nothing to drop, so nothing to check for
	}
	have, err := countReferrers(ctx, cache, dg)
	if err != nil {
		l.Debug("could not list the cache's referrers",
			slog.String("cache", cache.Name), slog.String("error", err.Error()))
		return false
	}
	return have >= want
}

// unreadableCache reports whether a read of cache would actually reach it from the
// delivery step, and why not when it would not.
func (w *Copier) unreadableCache(p *execPlan, st *execStep, cache config.StoreConfig) (string, bool) {
	if rd, ok := st.dst.(*registryDest); ok && rd.isProxy() {
		// A proxy destination reads THROUGH itself from its own upstream, ignoring
		// whatever source gantry hands it, so naming the cache changes nothing.
		return "the target is a pull-through cache and fetches from its own upstream", false
	}
	pd, ok := st.dst.(puller)
	if !ok {
		return "", true
	}
	tagRef, err := name.ParseReference(cache.Host+"/"+p.repo+p.id, w.refOpts(cache)...)
	if err != nil {
		return err.Error(), false
	}
	via, err := pd.pullRef(tagRef, cache)
	if err != nil {
		return err.Error(), false
	}
	for _, other := range st.attempts {
		if other.pullRef == via {
			return "the engine reaches every source by one host (pull_host / downstream_host)", false
		}
	}
	return "", true
}

// addRouteAttempt puts a read of the cache at the front of the delivery step's
// attempts, ahead of the source the caller named.
func (w *Copier) addRouteAttempt(ctx context.Context, p *execPlan, st *execStep, cache config.StoreConfig, needs []int) error {
	at := &execAttempt{src: cache, why: whyRoute, needs: needs}
	ref, err := w.planAttemptRef(p, cache)
	if err != nil {
		return err
	}
	at.ref = ref
	// The reference a fill of the cache publishes, so a read of it that misses can
	// wait for a job filling it right now.
	if tagRef, err := name.ParseReference(cache.Host+"/"+p.repo+p.id, w.refOpts(cache)...); err == nil {
		at.waitFill = tagRef.Name()
		if pd, ok := st.dst.(puller); ok {
			// Reachability was settled by unreadableCache before anything was filled.
			if at.pullRef, err = pd.pullRef(tagRef, cache); err != nil {
				return err
			}
		}
	}
	st.attempts = append([]*execAttempt{at}, st.attempts...)
	return nil
}

// repin re-derives everything that was built from the authority reference before
// the authority settled its tag. Only the delivery step's own attempts exist at
// this point; its in-store ref is deliberately left on the tag.
//
// The origin attempt is deliberately left alone. It is not a read of the
// authority's content from somewhere nearer — it is the fallback's own,
// different trust decision: an unpinned job's tag is resolved by the origin
// itself, which is the tag's authority (plan-source-fallback.md §3.2). Pinning it
// to a digest the SOURCE reported would silently convert that, and would fail the
// fallback outright whenever the source holds a manifest the origin never had —
// a platform-narrowed copy rebuilds the index, so this is ordinary. A job pinned
// by its own ref or by verification is unaffected: that digest was already on
// the attempt before routing ran.
func (w *Copier) repin(p *execPlan, st *execStep) error {
	for _, at := range st.attempts {
		if at.why == whyOrigin {
			continue
		}
		ref, err := w.planAttemptRef(p, at.src)
		if err != nil {
			return err
		}
		at.ref = ref
	}
	return nil
}

// requireAuthority enforces the job's require_authority decision. unconfirmed
// says the authority could not settle what the reference means, which is the only
// case where a caller can receive content it never confirmed — so it is the only
// case this refuses. It is a no-op for a job that is not routed, since there the
// source the caller named IS the authority.
func (w *Copier) requireAuthority(p *execPlan, req Request, unconfirmed bool) error {
	if !unconfirmed {
		return nil
	}
	if p.strictAuthority {
		return fmt.Errorf("%w: require_authority: %q could not confirm what %q means",
			ErrUnconfirmed, p.source.Name, p.repo+p.id)
	}
	return nil
}
