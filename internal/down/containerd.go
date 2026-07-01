package down

import (
	"context"
	"strings"
	"time"

	apievents "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
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

func (e *containerdEngine) Pull(ctx context.Context, ref string, sink Sink) error {
	ctx = namespaces.WithNamespace(ctx, e.namespace)
	done := make(chan struct{})
	go e.poll(ctx, sink, done)
	_, err := e.cli.Pull(ctx, ref, containerd.WithPullUnpack)
	close(done)
	if err != nil {
		return z.Err(err, "pull")
	}
	return nil
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
		return RemoveResult{}, z.Err(err, "delete image")
	}
	return RemoveResult{Deleted: []string{ref}}, nil
}
