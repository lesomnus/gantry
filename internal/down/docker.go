package down

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// dockerTarget pulls via the Docker Engine API. Credentials and insecure-registry
// policy for the cache are the daemon's own configuration, not gantry's.
type dockerTarget struct {
	name string
	cli  *client.Client
}

func newDockerTarget(c config.TargetConfig) (*dockerTarget, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost(c.Address)),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, z.Err(err, "docker client")
	}
	return &dockerTarget{name: c.Name, cli: cli}, nil
}

// dockerHost normalizes a configured address into an engine host: a bare path
// becomes a unix socket; an empty value falls back to the client default (env).
func dockerHost(addr string) string {
	switch {
	case addr == "":
		return client.DefaultDockerHost
	case strings.Contains(addr, "://"):
		return addr
	default:
		return "unix://" + addr
	}
}

func (t *dockerTarget) Name() string { return t.name }
func (t *dockerTarget) Kind() string { return "docker" }

func (t *dockerTarget) Ready(ctx context.Context) error {
	_, err := t.cli.Ping(ctx)
	return err
}

func (t *dockerTarget) Close() error { return t.cli.Close() }

func (t *dockerTarget) Pull(ctx context.Context, ref string) error {
	rc, err := t.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return z.Err(err, "image pull")
	}
	defer rc.Close()

	// The pull only completes once the JSONMessage stream is fully drained;
	// errors are reported in-band rather than as a transport error.
	dec := json.NewDecoder(rc)
	for {
		var m jsonmessage.JSONMessage
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return z.Err(err, "decode pull stream")
		}
		if m.Error != nil {
			return fmt.Errorf("pull: %s", m.Error.Message)
		}
	}
}
