package down

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apievents "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/reference"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/typeurl/v2"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// containerdEngine pulls via the containerd v2 client. Progress is sampled from
// the content store while the pull runs, since client.Pull has no progress stream.
type containerdEngine struct {
	name      string
	namespace string
	cli       *containerd.Client
}

func newContainerdEngine(c config.StoreConfig) (*containerdEngine, error) {
	cli, err := containerd.New(strings.TrimPrefix(c.Address, "unix://"))
	if err != nil {
		return nil, z.Err(err, "containerd client")
	}
	ns := c.Namespace
	if ns == "" {
		ns = "default"
	}
	return &containerdEngine{name: c.Name, namespace: ns, cli: cli}, nil
}

func (e *containerdEngine) Name() string { return e.name }
func (e *containerdEngine) Kind() string { return "containerd" }

func (e *containerdEngine) Ready(ctx context.Context) error {
	_, err := e.cli.Version(namespaces.WithNamespace(ctx, e.namespace))
	return err
}

func (e *containerdEngine) Close() error { return e.cli.Close() }

// Platform reports the daemon host's platform in OCI form. containerd exposes no
// daemon-host-platform API, so this is the local host's default platform — exact
// for the unix-socket deployments containerd stores are (the daemon shares the
// host), an approximation for a remote daemon on a different arch.
func (e *containerdEngine) Platform(context.Context) (string, error) {
	return platforms.Format(platforms.DefaultSpec()), nil
}

func (e *containerdEngine) Pull(ctx context.Context, ref string, digest string, platform string, as []string, anchor *AnchorBlob, sink Sink) ([]string, error) {
	ctx = namespaces.WithNamespace(ctx, e.namespace)
	pull_ref := ref
	if digest != "" {
		var err error
		if pull_ref, err = anchoredRef(ref, digest); err != nil {
			// A ref this engine cannot form is not the registry's fault; the same
			// input fails identically wherever it would have pulled from.
			return nil, fmt.Errorf("%w: %w", ErrEngine, err)
		}
	}
	opts := []containerd.RemoteOpt{containerd.WithPullUnpack}
	if platform != "" {
		// Passed through as-is; a platform the image does not provide surfaces as
		// the daemon's pull error.
		opts = append(opts, containerd.WithPlatform(platform))
	}
	done := make(chan struct{})
	go e.poll(ctx, sink, done)
	img, err := e.cli.Pull(ctx, pull_ref, opts...)
	close(done)
	if err != nil {
		return nil, z.Err(err, "pull")
	}
	// Record the names the deployment knows the image by: the requested `as`
	// names (tags, or digest references carrying the anchored digest), or —
	// for an anchored pull, whose only record is digest-named — the tag form
	// of the pull reference.
	anchored := pull_ref != ref
	names := as
	if len(names) == 0 {
		if !anchored {
			return []string{ref}, nil // the pull itself recorded this name
		}
		names = []string{ref}
	}
	var recorded []string
	for _, n := range names {
		if n == pull_ref {
			recorded = append(recorded, n) // the pull itself recorded this name
			continue
		}
		// A digest name must carry the digest of the content it will point at
		// (containerd records the name verbatim over img.Target()) — anything
		// else registers a lying reference.
		if r, err := reference.Parse(n); err == nil && r.Digest() != "" && r.Digest() != img.Target().Digest {
			if anchored && len(recorded) == 0 {
				e.untrack(ctx, pull_ref)
			}
			return nil, fmt.Errorf("%w: digest name %q does not carry the pulled digest %s", ErrEngine, n, img.Target().Digest)
		}
		rec := images.Image{Name: n, Target: img.Target()}
		if _, err := e.cli.ImageService().Create(ctx, rec); err != nil {
			if !cerrdefs.IsAlreadyExists(err) {
				if anchored && len(recorded) == 0 {
					e.untrack(ctx, pull_ref) // don't leave content rooted by an invisible record
				}
				return nil, fmt.Errorf("%w: %w", ErrEngine, z.Err(err, "tag %q", n))
			}
			if _, err := e.cli.ImageService().Update(ctx, rec, "target"); err != nil {
				if anchored && len(recorded) == 0 {
					e.untrack(ctx, pull_ref)
				}
				return nil, fmt.Errorf("%w: %w", ErrEngine, z.Err(err, "retag %q", n))
			}
		}
		recorded = append(recorded, n)
	}
	if !slices.Contains(names, pull_ref) {
		// Drop the pull-created record: the caller renamed the image away from
		// it — and for an anchored pull the retention index tracks only the
		// names the caller asked for, so this unrequested digest-named record
		// would root the content forever. (A digest name the caller DID request
		// via `as` is stamped into the index and reclaimed by name.)
		if err := e.cli.ImageService().Delete(ctx, pull_ref); err != nil && !cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %w", ErrEngine, z.Err(err, "untrack %q", pull_ref))
		}
	}
	return recorded, nil
}

// untrack best-effort deletes an image record on an error path; the pull
// context may already be canceled, so it detaches from it.
func (e *containerdEngine) untrack(ctx context.Context, ref string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = e.cli.ImageService().Delete(dctx, ref)
}

// poll samples the content store while the pull runs, reporting per-blob bytes.
// It observes all active ingests in the namespace (best-effort; concurrent pulls
// to the same engine could overlap).
func (e *containerdEngine) poll(ctx context.Context, sink Sink, done <-chan struct{}) {
	cs := e.cli.ContentStore()
	seen := map[string]bool{}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	sample := func() {
		statuses, err := cs.ListStatuses(ctx)
		if err != nil {
			return
		}
		active := map[string]bool{}
		for _, st := range statuses {
			dg := st.Expected.String()
			if dg == "" {
				dg = digestOf(st.Ref)
			}
			if dg == "" {
				continue
			}
			active[dg] = true
			seen[dg] = true
			sink.Layer(LayerUpdate{Digest: dg, Total: st.Total, Done: st.Offset, State: "pulling"})
		}
		for dg := range seen {
			if !active[dg] {
				sink.Layer(LayerUpdate{Digest: dg, State: "done"})
			}
		}
	}

	for {
		select {
		case <-done:
			sample()
			return
		case <-tick.C:
			sample()
		}
	}
}

// digestOf extracts a "sha256:..." digest from a content-store ref such as
// "layer-sha256:abc..." or "manifest-sha256:...".
func digestOf(ref string) string {
	if i := strings.Index(ref, "sha256:"); i >= 0 {
		return ref[i:]
	}
	return ""
}

func (e *containerdEngine) ns(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, e.namespace)
}

func (e *containerdEngine) InUse(ctx context.Context) (map[string]bool, error) {
	ctx = e.ns(ctx)
	cs, err := e.cli.Containers(ctx)
	if err != nil {
		return nil, z.Err(err, "containers")
	}
	m := make(map[string]bool, len(cs))
	for _, c := range cs {
		if info, err := c.Info(ctx); err == nil && info.Image != "" {
			m[info.Image] = true
		}
	}
	return m, nil
}

func (e *containerdEngine) SeedUsage(ctx context.Context, sink UsageSink) error {
	ctx = e.ns(ctx)
	cs, err := e.cli.Containers(ctx)
	if err != nil {
		return z.Err(err, "containers")
	}
	for _, c := range cs {
		info, err := c.Info(ctx)
		if err != nil || info.Image == "" {
			continue
		}
		t := info.CreatedAt
		if info.UpdatedAt.After(t) {
			t = info.UpdatedAt
		}
		sink(info.Image, t)
	}
	return nil
}

func (e *containerdEngine) WatchUsage(ctx context.Context, sink UsageSink) error {
	nctx := e.ns(ctx)
	envs, errc := e.cli.Subscribe(nctx, `topic=="/tasks/start"`, `topic=="/containers/create"`)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case env := <-envs:
			if env == nil {
				continue
			}
			v, err := typeurl.UnmarshalAny(env.Event)
			if err != nil {
				continue
			}
			switch ev := v.(type) {
			case *apievents.ContainerCreate:
				if ev.Image != "" {
					sink(ev.Image, env.Timestamp)
				}
			case *apievents.TaskStart:
				if c, err := e.cli.ContainerService().Get(nctx, ev.ContainerID); err == nil && c.Image != "" {
					sink(c.Image, env.Timestamp)
				}
			}
		}
	}
}

func (e *containerdEngine) Remove(ctx context.Context, ref string) (RemoveResult, error) {
	ctx = e.ns(ctx)
	if err := e.cli.ImageService().Delete(ctx, ref, images.SynchronousDelete()); err != nil {
		if cerrdefs.IsNotFound(err) {
			// Already gone (removed out-of-band, e.g. `ctr images rm`). Report success
			// so the caller syncs its retention index instead of erroring forever.
			return RemoveResult{}, nil
		}
		return RemoveResult{}, z.Err(err, "delete image")
	}
	return RemoveResult{Deleted: []string{ref}}, nil
}
