package cpx

import (
	"context"
	"io"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// proxySource reads each blob through the cache so a pull-through cache fetches
// and persists it from upstream. Every blob is read to EOF (a HEAD or partial
// read would leave the cache cold).
type proxySource struct {
	target   config.StoreConfig
	targetRT http.RoundTripper // resolved outbound transport (nil = library default)
}

func (s *proxySource) opts(ctx context.Context) []remote.Option {
	return baseOpts(ctx, registryAuth(s.target), s.targetRT)
}

func (s *proxySource) Resolve(ctx context.Context, _, dst name.Reference, platforms []string) (*Plan, error) {
	// Resolve against the cache so the manifest itself is pulled through.
	return resolvePlan(ctx, dst, platforms, s.opts(ctx))
}

func (s *proxySource) Fill(ctx context.Context, _, dst name.Repository, l PlannedLayer, sink ProgressSink) error {
	layer, err := remote.Layer(dst.Digest(l.Digest), s.opts(ctx)...)
	if err != nil {
		return z.Err(err, "resolve cache layer %s", l.Digest)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return z.Err(err, "open blob %s", l.Digest)
	}
	defer rc.Close()
	cr := &countingReader{rc: rc, sink: sink}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return z.Err(err, "copy blob %s", l.Digest)
	}
	sink.SetState("copied")
	return nil
}

// Commit is a no-op for proxy mode: Resolve pulled the manifest through the
// cache and Fill pulled every blob, so the cache is already populated. The
// committed digest is unknown (the cache resolves the tag itself).
func (s *proxySource) Commit(ctx context.Context, src, dst name.Reference, platforms []string, verbatim bool) (v1.Hash, error) {
	return v1.Hash{}, nil
}
