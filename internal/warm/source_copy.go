package warm

import (
	"context"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// copySource (default mode) pulls each blob from upstream and pushes it into the
// cache, skipping blobs the cache already has. Progress reflects bytes actually
// moved into the cache.
type copySource struct {
	from config.StoreConfig
	to   config.StoreConfig
}

func (s *copySource) pullOpts(ctx context.Context) []remote.Option {
	return baseOpts(ctx, registryAuth(s.from), s.from.Insecure)
}

func (s *copySource) pushOpts(ctx context.Context) []remote.Option {
	return baseOpts(ctx, registryAuth(s.to), s.to.Insecure)
}

func (s *copySource) Resolve(ctx context.Context, src, _ name.Reference, platforms []string) (*Plan, error) {
	return resolvePlan(ctx, src, platforms, s.pullOpts(ctx))
}

func (s *copySource) Warm(ctx context.Context, src, dst name.Repository, l PlannedLayer, sink ProgressSink) error {
	layer, err := remote.Layer(src.Digest(l.Digest), s.pullOpts(ctx)...)
	if err != nil {
		return z.Err(err, "resolve src layer %s", l.Digest)
	}
	cl := &countingLayer{Layer: layer, sink: sink}
	if err := remote.WriteLayer(dst, cl, s.pushOpts(ctx)...); err != nil {
		return z.Err(err, "push blob %s", l.Digest)
	}
	if cl.moved.Load() == 0 {
		sink.SetState("exists") // blob already in cache; nothing moved
	} else {
		sink.SetState("warm")
	}
	return nil
}

// Commit publishes the cache tag. For a single image it pushes the manifest
// (blobs are skip-checked since Warm uploaded them); for an index it pushes a
// new index referencing only the warmed platforms so unwanted arches are not
// pulled into the cache.
func (s *copySource) Commit(ctx context.Context, src, dst name.Reference, platforms []string) error {
	desc, err := remote.Get(src, s.pullOpts(ctx)...)
	if err != nil {
		return z.Err(err, "get src %q", src.Name())
	}
	if !desc.MediaType.IsIndex() {
		img, err := desc.Image()
		if err != nil {
			return z.Err(err, "image")
		}
		if err := remote.Write(dst, img, s.pushOpts(ctx)...); err != nil {
			return z.Err(err, "push manifest")
		}
		return nil
	}

	want, err := parsePlatforms(platforms)
	if err != nil {
		return err
	}
	srcIdx, err := desc.ImageIndex()
	if err != nil {
		return z.Err(err, "image index")
	}
	im, err := srcIdx.IndexManifest()
	if err != nil {
		return z.Err(err, "index manifest")
	}
	idx := v1.ImageIndex(empty.Index)
	for _, m := range im.Manifests {
		if m.Platform == nil || m.Platform.OS == "unknown" || !selected(m.Platform, want) {
			continue
		}
		img, err := srcIdx.Image(m.Digest)
		if err != nil {
			return z.Err(err, "index image %s", m.Digest)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: m.Platform},
		})
	}
	if err := remote.WriteIndex(dst, idx, s.pushOpts(ctx)...); err != nil {
		return z.Err(err, "push index")
	}
	return nil
}
