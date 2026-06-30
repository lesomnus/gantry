package down

import (
	"context"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
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
