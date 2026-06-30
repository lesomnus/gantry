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

// dockerEngine pulls via the Docker Engine API. Credentials and insecure-registry
// policy for the source are the daemon's own configuration, not gantry's.
type dockerEngine struct {
	name string
	cli  *client.Client
}

func newDockerEngine(c config.StoreConfig) (*dockerEngine, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost(c.Address)),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, z.Err(err, "docker client")
	}
	return &dockerEngine{name: c.Name, cli: cli}, nil
}

// dockerHost normalizes a configured address into an engine host.
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

func (e *dockerEngine) Name() string { return e.name }
func (e *dockerEngine) Kind() string { return "docker" }

func (e *dockerEngine) Ready(ctx context.Context) error {
	_, err := e.cli.Ping(ctx)
	return err
}

func (e *dockerEngine) Close() error { return e.cli.Close() }

func (e *dockerEngine) Pull(ctx context.Context, ref string, sink Sink) error {
	rc, err := e.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return z.Err(err, "image pull")
	}
	defer rc.Close()

	// The pull completes only once the JSONMessage stream is drained; per-layer
	// download bytes are reported as they arrive, errors come in-band.
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
		if u, ok := layerUpdate(m); ok {
			sink.Layer(u)
		}
	}
}

// layerUpdate maps a JSONMessage to a LayerUpdate, or ok=false for lines that
// carry no per-layer byte progress (overall status, extracting, etc.).
func layerUpdate(m jsonmessage.JSONMessage) (LayerUpdate, bool) {
	if m.ID == "" {
		return LayerUpdate{}, false
	}
	u := LayerUpdate{Digest: m.ID}
	switch m.Status {
	case "Already exists":
		u.State = "exists"
	case "Downloading":
		u.State = "pulling"
		if d := m.Progress; d != nil {
			u.Done, u.Total = d.Current, d.Total
		}
	case "Download complete", "Pull complete":
		u.State = "done"
		if d := m.Progress; d != nil && d.Total > 0 {
			u.Done, u.Total = d.Total, d.Total
		}
	default:
		return LayerUpdate{}, false
	}
	return u, true
}
