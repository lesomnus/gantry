package cpx

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
)

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

	copyReferrers bool                  // a job property, applied per registry step
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

	// Attempts last, so every one of them inherits whatever verification pinned: a
	// job that falls back reaches for the very digest it verified, from a different
	// host.
	p.steps = []*execStep{deliver}
	if err := w.bindDelivery(ctx, p, deliver, base.Context().RegistryStr()); err != nil {
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
