package warm

import (
	"context"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/z"
)

// dest is a job's destination store. It only exposes identity; HOW an image
// reaches it is a capability discovered by type assertion (pusher / puller), so
// plan/execute don't care about store kinds — only about what the destination
// can do. A future kind that pushes AND pulls would just satisfy both.
type dest interface {
	Name() string
	Kind() string
}

// pusher is a destination gantry fills itself by copying blobs into it (an OCI
// registry).
type pusher interface {
	dest
	// newSource builds the copy pipeline from the given source store into this
	// destination (copy or proxy mode).
	newSource(from config.StoreConfig) (Source, error)
	// dstRef derives the in-store reference for src via the store's rewrite rules.
	dstRef(src name.Reference) (name.Reference, error)
}

// puller is a destination that fetches the image itself when told to (a
// docker/containerd daemon).
type puller interface {
	dest
	// pullRef is the reference the destination is told to pull for src: the
	// source ref with the engine's pull_host — or the source store's
	// downstream_host — applied.
	pullRef(src name.Reference, from config.StoreConfig) (string, error)
	// pull triggers the fetch. digest anchors it; platform is passed through
	// as-is (the daemon errors if the image has no such platform).
	pull(ctx context.Context, ref, digest, platform string, sink down.Sink) error
	// hostPlatform is the daemon host's platform in OCI form ("linux/amd64"),
	// the default platform when a job does not name one.
	hostPlatform(ctx context.Context) (string, error)
}

// resolveDest resolves a job's `to` into a destination: a declared engine store
// becomes a puller; anything else resolves through the registry path (declared
// oci store, or a bare host when allow_unknown_stores is set) and becomes a
// pusher.
func resolveDest(stores *store.Set, to string) (dest, error) {
	if c, ok := stores.Config(to); ok && c.IsEngine() {
		eng, err := stores.Engine(to)
		if err != nil {
			return nil, err
		}
		return &engineDest{cfg: c, eng: eng}, nil
	}
	c, err := stores.Registry(to)
	if err != nil {
		return nil, err
	}
	return &registryDest{cfg: c}, nil
}

// registryDest is an OCI registry destination: gantry copies blobs into it.
type registryDest struct{ cfg config.StoreConfig }

func (d *registryDest) Name() string { return d.cfg.Name }
func (d *registryDest) Kind() string { return "oci" }

// isProxy reports whether the store self-fills (mode "proxy") — relevant to
// verification and referrer copying, which need a copy-mode destination.
func (d *registryDest) isProxy() bool { return d.cfg.Mode == "proxy" }

func (d *registryDest) newSource(from config.StoreConfig) (Source, error) {
	return NewSource(from, d.cfg)
}

func (d *registryDest) dstRef(src name.Reference) (name.Reference, error) {
	return Rewrite(d.cfg.Rewrite, d.cfg.Host, src, d.cfg.Insecure)
}

// engineDest is a docker/containerd daemon destination: it pulls the image
// itself when told to.
type engineDest struct {
	cfg config.StoreConfig
	eng down.Engine
}

func (d *engineDest) Name() string { return d.cfg.Name }
func (d *engineDest) Kind() string { return d.eng.Kind() }

func (d *engineDest) pullRef(src name.Reference, from config.StoreConfig) (string, error) {
	host := d.cfg.PullHost
	if host == "" {
		host = from.DownstreamHost
	}
	if host == "" {
		return src.Name(), nil
	}
	return rewriteHost(src, host)
}

func (d *engineDest) pull(ctx context.Context, ref, digest, platform string, sink down.Sink) error {
	return d.eng.Pull(ctx, ref, digest, platform, sink)
}

func (d *engineDest) hostPlatform(ctx context.Context) (string, error) {
	return d.eng.Platform(ctx)
}

// rewriteHost replaces the registry host of ref, preserving repo path and tag/digest.
func rewriteHost(ref name.Reference, host string) (string, error) {
	repo := ref.Context().RepositoryStr()
	var out string
	if d, ok := ref.(name.Digest); ok {
		out = host + "/" + repo + "@" + d.DigestStr()
	} else {
		out = host + "/" + repo + ":" + ref.Identifier()
	}
	if _, err := name.ParseReference(out); err != nil {
		return "", z.Err(err, "invalid downstream ref %q", out)
	}
	return out, nil
}
