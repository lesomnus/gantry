package cpx

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/z"
)

// ProgressSink receives byte and state updates for one blob being copied.
type ProgressSink interface {
	Add(n int64)
	SetState(state string)
}

// Source fills blobs into the cache. copy mode pulls from upstream and pushes
// to the cache; proxy mode reads through the cache to trigger a self-fill.
//
// The Copier parses both references once and hands them to the Source: Resolve
// walks whichever side the mode reads (upstream for copy, cache for proxy), Fill
// moves each blob, and Commit finalizes the image so the cache tag resolves.
type Source interface {
	Resolve(ctx context.Context, src, dst name.Reference, platforms []string) (*Plan, error)
	Fill(ctx context.Context, src, dst name.Repository, l PlannedLayer, sink ProgressSink) error
	// Commit publishes the manifest(s) under the cache tag and returns the
	// committed digest (zero when unknown, e.g. proxy mode). copy mode pushes a
	// platform-filtered index referencing the blobs Fill uploaded — or, with
	// verbatim, the source manifest/index byte-for-byte so its digest (and any
	// signature over it) is preserved; proxy mode is a no-op (resolving +
	// reading already populated the cache).
	Commit(ctx context.Context, src, dst name.Reference, platforms []string, verbatim bool) (v1.Hash, error)
}

// NewSource builds a registry copy/proxy between two registry stores, selected
// by the target store's mode. The per-store outbound transport (TPM mTLS or
// a TLS-skip transport for self-signed registries) is resolved once here and
// reused for every request the source makes.
func NewSource(source, target config.StoreConfig) (Source, error) {
	switch target.Mode {
	case "", "copy":
		sourceRT, err := xport.Transport(source)
		if err != nil {
			return nil, z.Err(err, "store %q outbound transport", source.Name)
		}
		targetRT, err := xport.Transport(target)
		if err != nil {
			return nil, z.Err(err, "store %q outbound transport", target.Name)
		}
		return &copySource{source: source, target: target, sourceRT: sourceRT, targetRT: targetRT}, nil
	case "proxy":
		targetRT, err := xport.Transport(target)
		if err != nil {
			return nil, z.Err(err, "store %q outbound transport", target.Name)
		}
		return &proxySource{target: target, targetRT: targetRT}, nil
	default:
		return nil, fmt.Errorf("store %q: unknown mode %q", target.Name, target.Mode)
	}
}

// registryAuth returns explicit basic auth when credentials are set, or nil to
// fall back to the docker keychain.
func registryAuth(c config.StoreConfig) authn.Authenticator {
	if c.Username != "" {
		return &authn.Basic{Username: c.Username, Password: c.Password}
	}
	return nil
}

// baseOpts builds remote options: context, credentials, and an optional custom
// transport. A nil auth falls back to the docker keychain (used for upstream
// pulls). rt, when non-nil, is the store's outbound transport (TPM mTLS or a
// TLS-skip transport for self-signed registries); ggcr dials both the registry
// and its token endpoint through it. Plain HTTP is driven by name.Insecure on
// the reference.
func baseOpts(ctx context.Context, auth authn.Authenticator, rt http.RoundTripper) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx)}
	if auth != nil {
		opts = append(opts, remote.WithAuth(auth))
	} else {
		opts = append(opts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	if rt != nil {
		opts = append(opts, remote.WithTransport(rt))
	}
	return opts
}

// upstreamPlan resolves the layer plan for ref directly against its source
// store — used to estimate an engine pull's size without a copy Source.
func upstreamPlan(ctx context.Context, source config.StoreConfig, ref name.Reference, platforms []string) (*Plan, error) {
	rt, err := xport.Transport(source)
	if err != nil {
		return nil, err
	}
	return resolvePlan(ctx, ref, platforms, baseOpts(ctx, registryAuth(source), rt))
}

// fetchAnchor fetches the raw manifest/index bytes the digest reference names
// from the store the attempt is pulling from — normally the job's source (the
// cache), keeping the two-hop promise that the origin registry is never
// contacted; a fallback attempt passes the origin here, which is the point of
// falling back. The bytes are hashed here against the reference's digest rather
// than trusting the transport (ggcr skips content verification for some legacy
// media types), because they are about to be registered on a node under that
// digest's name — so an anchor is only ever as trustworthy as its digest, never
// as the host that served it.
func fetchAnchor(ctx context.Context, source config.StoreConfig, ref name.Digest) (*down.AnchorBlob, error) {
	rt, err := xport.Transport(source)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(ref, baseOpts(ctx, registryAuth(source), rt)...)
	if err != nil {
		return nil, z.Err(err, "get anchor manifest %q", ref.Name())
	}
	if !strings.HasPrefix(ref.DigestStr(), "sha256:") {
		return nil, fmt.Errorf("digest `as` supports sha256 references; got %q", ref.DigestStr())
	}
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256(desc.Manifest)); got != ref.DigestStr() {
		return nil, fmt.Errorf("anchor manifest for %q hashes to %s", ref.Name(), got)
	}
	return &down.AnchorBlob{
		MediaType: string(desc.MediaType),
		Digest:    ref.DigestStr(),
		Bytes:     desc.Manifest,
	}, nil
}

// resolvePlan walks ref (image or index) restricted to the selected platforms
// and lists every layer + config blob with its compressed size. No blob bytes
// are downloaded; sizes come from the manifest descriptors.
func resolvePlan(ctx context.Context, ref name.Reference, platforms []string, opts []remote.Option) (*Plan, error) {
	want, err := parsePlatforms(platforms)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, z.Err(err, "get manifest %q", ref.Name())
	}
	plan := &Plan{Ref: ref.Name()}
	repo := ref.Context().Name()
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, z.Err(err, "image index")
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, z.Err(err, "index manifest")
		}
		for _, m := range im.Manifests {
			if m.Platform == nil || m.Platform.OS == "unknown" {
				continue // attestation/SBOM entry, not an image
			}
			if !selected(m.Platform, want) {
				continue
			}
			img, err := idx.Image(m.Digest)
			if err != nil {
				return nil, z.Err(err, "index image %s", m.Digest)
			}
			if err := addImage(plan, img, repo, m.Platform.String()); err != nil {
				return nil, err
			}
		}
		if len(plan.Layers) == 0 {
			return nil, fmt.Errorf("no manifest matched the requested platforms for %q", ref.Name())
		}
	} else {
		img, err := desc.Image()
		if err != nil {
			return nil, z.Err(err, "image")
		}
		if err := addImage(plan, img, repo, ""); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func addImage(plan *Plan, img v1.Image, repo, platform string) error {
	mf, err := img.Manifest()
	if err != nil {
		return z.Err(err, "manifest")
	}
	for _, d := range mf.Layers {
		plan.Layers = append(plan.Layers, PlannedLayer{Digest: d.Digest.String(), Repo: repo, Platform: platform, Size: d.Size})
		plan.Total += d.Size
	}
	plan.Layers = append(plan.Layers, PlannedLayer{Digest: mf.Config.Digest.String(), Repo: repo, Platform: platform, Size: mf.Config.Size})
	plan.Total += mf.Config.Size
	return nil
}

func parsePlatforms(ss []string) ([]v1.Platform, error) {
	out := make([]v1.Platform, 0, len(ss))
	for _, s := range ss {
		p, err := v1.ParsePlatform(s)
		if err != nil {
			return nil, z.Err(err, "parse platform %q", s)
		}
		out = append(out, *p)
	}
	return out, nil
}

// selected reports whether have matches any wanted platform; empty want matches all.
func selected(have *v1.Platform, want []v1.Platform) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if have.OS == w.OS && have.Architecture == w.Architecture &&
			(w.Variant == "" || have.Variant == w.Variant) {
			return true
		}
	}
	return false
}

// countingLayer wraps a layer so the bytes streamed during a push are counted.
// If WriteLayer skips an already-present blob, Compressed is never called and
// moved stays zero, which the caller reads as "exists".
type countingLayer struct {
	v1.Layer
	sink  ProgressSink
	moved atomic.Int64
}

func (c *countingLayer) Compressed() (io.ReadCloser, error) {
	rc, err := c.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	return &countingReader{rc: rc, sink: c.sink, moved: &c.moved}, nil
}

type countingReader struct {
	rc    io.ReadCloser
	sink  ProgressSink
	moved *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		if c.moved != nil {
			c.moved.Add(int64(n))
		}
		c.sink.Add(int64(n))
	}
	return n, err
}

func (c *countingReader) Close() error { return c.rc.Close() }
