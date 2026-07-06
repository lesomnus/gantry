package down

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/containerd/platforms"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/z"
)

// dockerEngine pulls via the Docker Engine API. Credentials and insecure-registry
// policy for the source are the daemon's own configuration, not gantry's.
type dockerEngine struct {
	name string
	cli  *client.Client

	mu   sync.Mutex
	plat string // cached daemon host platform; only a successful probe is kept
}

func newDockerEngine(c config.StoreConfig) (*dockerEngine, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}

	// Optional mTLS to the daemon over tcp: a TPM-backed client certificate,
	// a private CA to verify the daemon, or a TLS-verify skip — the same store
	// transport the registry paths use. When set, the docker client detects the
	// *http.Transport's TLSClientConfig and dials the daemon over https.
	rt, err := xport.Transport(c)
	if err != nil {
		return nil, z.Err(err, "docker client transport")
	}
	if rt != nil {
		// Must precede WithHost: WithHost runs sockets.ConfigureTransport on this
		// client's transport, so the client has to be installed first.
		opts = append(opts, client.WithHTTPClient(&http.Client{Transport: rt}))
	}
	opts = append(opts, client.WithHost(dockerHost(c.Address)))

	cli, err := client.NewClientWithOpts(opts...)
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

// Platform reports the daemon host's platform in OCI form, probed from the
// daemon's Info (its OSType/Architecture, e.g. "linux"/"x86_64"). The first
// successful probe is cached; failures are retried on the next call.
func (e *dockerEngine) Platform(ctx context.Context) (string, error) {
	e.mu.Lock()
	if e.plat != "" {
		p := e.plat
		e.mu.Unlock()
		return p, nil
	}
	e.mu.Unlock()

	info, err := e.cli.Info(ctx)
	if err != nil {
		return "", z.Err(err, "docker info")
	}
	p, err := ociPlatform(info.OSType, info.Architecture)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.plat = p
	e.mu.Unlock()
	return p, nil
}

// ociPlatform normalizes a daemon-reported os/arch pair ("linux"/"x86_64") into
// the OCI platform form pulls use ("linux/amd64").
func ociPlatform(osType, arch string) (string, error) {
	p, err := platforms.Parse(osType + "/" + arch)
	if err != nil {
		return "", z.Err(err, "parse daemon platform %q/%q", osType, arch)
	}
	return platforms.Format(platforms.Normalize(p)), nil
}

func (e *dockerEngine) Close() error { return e.cli.Close() }

func (e *dockerEngine) Pull(ctx context.Context, ref string, digest string, platform string, sink Sink) error {
	pull_ref := ref
	if digest != "" {
		var err error
		if pull_ref, err = anchoredRef(ref, digest); err != nil {
			return err
		}
	}
	// platform is passed through as-is: if the image has no such platform the
	// daemon's error surfaces through the pull stream below.
	rc, err := e.cli.ImagePull(ctx, pull_ref, image.PullOptions{Platform: platform})
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
			if !errors.Is(err, io.EOF) {
				return z.Err(err, "decode pull stream")
			}
			break
		}
		if m.Error != nil {
			return fmt.Errorf("pull: %s", m.Error.Message)
		}
		if u, ok := layerUpdate(m); ok {
			sink.Layer(u)
		}
	}
	if pull_ref != ref {
		// The digest pull leaves the image untagged; give it the tag callers,
		// containers, and the retention index know it by.
		if err := e.cli.ImageTag(ctx, pull_ref, ref); err != nil {
			// Best-effort: an untagged image is invisible to the retention index
			// and would never be reclaimed. Untagging the canonical ref is safe —
			// content held by other tags stays. The pull context may already be
			// canceled (a likely cause of the failure), so detach from it.
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = e.cli.ImageRemove(dctx, pull_ref, image.RemoveOptions{})
			return z.Err(err, "tag %q", ref)
		}
	}
	return nil
}

func (e *dockerEngine) InUse(ctx context.Context) (map[string]bool, error) {
	cs, err := e.cli.ContainerList(ctx, container.ListOptions{All: false}) // running only
	if err != nil {
		return nil, z.Err(err, "container list")
	}
	m := make(map[string]bool, len(cs)*2)
	for _, c := range cs {
		if c.Image != "" {
			m[c.Image] = true
		}
		if c.ImageID != "" {
			m[c.ImageID] = true
		}
	}
	return m, nil
}

func (e *dockerEngine) SeedUsage(ctx context.Context, sink UsageSink) error {
	cs, err := e.cli.ContainerList(ctx, container.ListOptions{All: true}) // incl. stopped
	if err != nil {
		return z.Err(err, "container list")
	}
	for _, c := range cs {
		if c.Image != "" {
			sink(c.Image, time.Unix(c.Created, 0))
		}
	}
	return nil
}

func (e *dockerEngine) WatchUsage(ctx context.Context, sink UsageSink) error {
	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))
	f.Add("event", string(events.ActionStart))
	f.Add("event", string(events.ActionRestart))
	f.Add("event", string(events.ActionUnPause))
	msgs, errc := e.cli.Events(ctx, events.ListOptions{Filters: f})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case m := <-msgs:
			if img := m.Actor.Attributes["image"]; img != "" {
				sink(img, time.Unix(0, m.TimeNano))
			}
		}
	}
}

func (e *dockerEngine) Remove(ctx context.Context, ref string) (RemoveResult, error) {
	resp, err := e.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	if err != nil {
		if client.IsErrNotFound(err) {
			// Already gone (removed out-of-band, e.g. `docker rmi`). Report success
			// so the caller syncs its retention index instead of erroring forever.
			return RemoveResult{}, nil
		}
		return RemoveResult{}, z.Err(err, "image remove")
	}
	var rr RemoveResult
	for _, d := range resp {
		if d.Untagged != "" {
			rr.Untagged = append(rr.Untagged, d.Untagged)
		}
		if d.Deleted != "" {
			rr.Deleted = append(rr.Deleted, d.Deleted)
		}
	}
	return rr, nil
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
