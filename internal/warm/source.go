package warm

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// ProgressSink receives byte and state updates for one blob being warmed.
type ProgressSink interface {
	Add(n int64)
	SetState(state string)
}

// Source warms blobs into the cache. copy mode pulls from upstream and pushes
// to the cache; proxy mode reads through the cache to trigger a self-fill.
//
// The Warmer parses both references once and hands them to the Source: Resolve
// walks whichever side the mode reads (upstream for copy, cache for proxy), Warm
// moves each blob, and Commit finalizes the image so the cache tag resolves.
type Source interface {
	Resolve(ctx context.Context, src, dst name.Reference, platforms []string) (*Plan, error)
	Warm(ctx context.Context, src, dst name.Repository, l PlannedLayer, sink ProgressSink) error
	// Commit publishes the manifest(s) under the cache tag. copy mode pushes a
	// platform-filtered index referencing the blobs Warm uploaded; proxy mode is
	// a no-op (resolving + reading already populated the cache).
	Commit(ctx context.Context, src, dst name.Reference, platforms []string) error
}

// NewSource builds the Source selected by registry.mode.
func NewSource(rc config.RegistryConfig) (Source, error) {
	switch rc.Mode {
	case "", "copy":
		return &copySource{pushAuth: cacheAuth(rc), insecure: rc.Insecure}, nil
	case "proxy":
		return &proxySource{auth: cacheAuth(rc), insecure: rc.Insecure}, nil
	default:
		return nil, fmt.Errorf("unknown registry mode %q", rc.Mode)
	}
}

func cacheAuth(rc config.RegistryConfig) authn.Authenticator {
	if rc.Username != "" {
		return &authn.Basic{Username: rc.Username, Password: rc.Password}
	}
	return authn.Anonymous
}

// baseOpts builds remote options. A nil auth falls back to the docker keychain
// (used for upstream pulls); insecure adds a TLS-skip transport for self-signed
// cache registries (plain HTTP is driven by name.Insecure on the reference).
func baseOpts(ctx context.Context, auth authn.Authenticator, insecure bool) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx)}
	if auth != nil {
		opts = append(opts, remote.WithAuth(auth))
	} else {
		opts = append(opts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	if insecure {
		opts = append(opts, remote.WithTransport(insecureTransport()))
	}
	return opts
}

func insecureTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return t
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
