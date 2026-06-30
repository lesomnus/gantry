package down

import (
	"context"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// containerdTarget pulls via the containerd v2 client. The namespace selects
// which view the image lands in (k3s uses "k8s.io"); pulls unpack so the daemon
// can run the image immediately.
type containerdTarget struct {
	name      string
	namespace string
	cli       *containerd.Client
}

func newContainerdTarget(c config.TargetConfig) (*containerdTarget, error) {
	cli, err := containerd.New(strings.TrimPrefix(c.Address, "unix://"))
	if err != nil {
		return nil, z.Err(err, "containerd client")
	}
	ns := c.Namespace
	if ns == "" {
		ns = "default"
	}
	return &containerdTarget{name: c.Name, namespace: ns, cli: cli}, nil
}

func (t *containerdTarget) Name() string { return t.name }
func (t *containerdTarget) Kind() string { return "containerd" }

func (t *containerdTarget) Ready(ctx context.Context) error {
	_, err := t.cli.Version(namespaces.WithNamespace(ctx, t.namespace))
	return err
}

func (t *containerdTarget) Close() error { return t.cli.Close() }

func (t *containerdTarget) Pull(ctx context.Context, ref string) error {
	ctx = namespaces.WithNamespace(ctx, t.namespace)
	if _, err := t.cli.Pull(ctx, ref, containerd.WithPullUnpack); err != nil {
		return z.Err(err, "pull")
	}
	return nil
}
