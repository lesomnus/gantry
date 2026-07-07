package down

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/go-containerregistry/pkg/name"
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

	// pullMu serializes ReapUntagged against pull registration: a pull already
	// registered makes the reaper skip its image, and a pull registering while a
	// reap runs waits until the removals finish, so it re-fetches whatever was
	// deleted instead of racing the removal with ImageTag.
	pullMu   sync.Mutex
	inflight map[string]int // pull reference -> active pull count
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
	return &dockerEngine{name: c.Name, cli: cli, inflight: map[string]int{}}, nil
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

func (e *dockerEngine) Pull(ctx context.Context, ref string, digest string, platform string, as []string, sink Sink) error {
	pull_ref := ref
	if digest != "" {
		var err error
		if pull_ref, err = anchoredRef(ref, digest); err != nil {
			return err
		}
	}
	defer e.trackPull(pull_ref)()
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
	// Record the names the deployment knows the image by: the requested `as`
	// names, or — for an anchored pull, which leaves the content untagged —
	// the tag form of the pull reference.
	anchored := pull_ref != ref
	names := as
	if len(names) == 0 {
		if !anchored {
			return nil
		}
		names = []string{ref}
	}
	tagged := false
	for _, n := range names {
		if !anchored && n == ref {
			tagged = true // the pull itself created this tag
			continue
		}
		if err := e.cli.ImageTag(ctx, pull_ref, n); err != nil {
			// Best-effort fast cleanup (the untagged reaper is the eventual
			// backstop, but it can be configured off), only while the content has
			// no visible name yet — a partially named image stays. Skip when
			// another pull of the same reference is in flight and about to tag
			// it. The pull context may already be canceled (a likely cause of the
			// failure), so detach from it.
			if anchored && !tagged && e.pullCount(pull_ref) == 1 {
				dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, _ = e.cli.ImageRemove(dctx, pull_ref, image.RemoveOptions{})
			}
			return z.Err(err, "tag %q", n)
		}
		tagged = true
	}
	if !anchored && !slices.Contains(names, ref) {
		// The caller renamed the image away from the pull-created tag; drop it.
		// Content held by the names above stays. Skip when another pull of the
		// same reference is in flight and racing to tag it.
		if e.pullCount(ref) == 1 {
			if _, err := e.cli.ImageRemove(ctx, ref, image.RemoveOptions{}); err != nil {
				return z.Err(err, "untag %q", ref)
			}
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
	var rr RemoveResult
	if err := e.remove(ctx, ref, &rr); err != nil {
		return RemoveResult{}, z.Err(err, "image remove")
	}
	return rr, nil
}

// remove deletes one reference, accumulating what the daemon reports into rr.
// A reference already gone (removed out-of-band, e.g. `docker rmi`) is success,
// so callers sync their retention index instead of erroring forever.
func (e *dockerEngine) remove(ctx context.Context, ref string, rr *RemoveResult) error {
	resp, err := e.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil
		}
		return err
	}
	for _, d := range resp {
		if d.Untagged != "" {
			rr.Untagged = append(rr.Untagged, d.Untagged)
		}
		if d.Deleted != "" {
			rr.Deleted = append(rr.Deleted, d.Deleted)
		}
	}
	return nil
}

// canonRef normalizes a reference to one canonical spelling, so gantry-side
// refs ("index.docker.io/library/nginx@sha256:x") and the daemon's familiar
// forms ("nginx@sha256:x") compare equal. An unparseable ref (e.g. a bare
// image ID) passes through unchanged.
func canonRef(ref string) string {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ref
	}
	switch t := r.(type) {
	case name.Digest:
		return t.Context().Name() + "@" + t.DigestStr()
	case name.Tag:
		return t.Context().Name() + ":" + t.TagStr()
	}
	return ref
}

// trackPull registers an in-flight pull of ref so ReapUntagged will not delete
// the image the pull is about to (re)reference; the returned func deregisters.
// Registration blocks while a reap is mid-removal (see pullMu), never for the
// duration of another pull.
func (e *dockerEngine) trackPull(ref string) func() {
	key := canonRef(ref)
	e.pullMu.Lock()
	if e.inflight == nil {
		e.inflight = map[string]int{}
	}
	e.inflight[key]++
	e.pullMu.Unlock()
	return func() {
		e.pullMu.Lock()
		if e.inflight[key]--; e.inflight[key] <= 0 {
			delete(e.inflight, key)
		}
		e.pullMu.Unlock()
	}
}

// pullCount reports how many pulls of ref are currently registered.
func (e *dockerEngine) pullCount(ref string) int {
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	return e.inflight[canonRef(ref)]
}

// Images snapshots the daemon's image store for inventory reconciliation.
func (e *dockerEngine) Images(ctx context.Context) (Inventory, error) {
	imgs, err := e.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return Inventory{}, z.Err(err, "image list")
	}
	var inv Inventory
	for _, s := range imgs {
		if tags := realRefs(s.RepoTags); len(tags) > 0 {
			inv.Refs = append(inv.Refs, tags...)
			continue
		}
		inv.Untagged = append(inv.Untagged, UntaggedImage{ID: s.ID, RepoDigests: realRefs(s.RepoDigests)})
	}
	return inv, nil
}

// realRefs drops the "<none>:<none>" / "<none>@<none>" placeholders older
// daemon API versions (< 1.43) report instead of an empty list.
func realRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if !strings.HasPrefix(r, "<none>") {
			out = append(out, r)
		}
	}
	return out
}

// ReapUntagged deletes one untagged image, re-checking the daemon first. The
// digest references are removed one at a time — the daemon frees content when
// the last one goes and never demands force the way a multi-repo by-ID delete
// does; only an image with no references at all is deleted by bare ID.
func (e *dockerEngine) ReapUntagged(ctx context.Context, id string) (RemoveResult, bool, error) {
	// pullMu is held for the whole reap so a pull registering mid-removal waits
	// and then re-fetches whatever was deleted. Bound the daemon calls so a
	// wedged daemon cannot hold pull registration hostage.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	e.pullMu.Lock()
	defer e.pullMu.Unlock()

	var rr RemoveResult
	ins, err := e.cli.ImageInspect(ctx, id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return rr, true, nil // already gone: converged
		}
		return rr, false, z.Err(err, "image inspect")
	}
	if len(realRefs(ins.RepoTags)) > 0 {
		return rr, false, nil // re-tagged since the scan (e.g. a manual rollback)
	}
	digests := realRefs(ins.RepoDigests)
	for _, d := range digests {
		if e.inflight[canonRef(d)] > 0 {
			return rr, false, nil // a pull is about to re-reference it
		}
	}
	// A stopped container blocks the delete without force; skip instead of
	// erroring every pass. (Running containers were kept at plan time already —
	// InUse — but re-check here since plan and apply are not atomic.)
	cs, err := e.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return rr, false, z.Err(err, "container list")
	}
	for _, c := range cs {
		if c.ImageID == id || c.Image == id {
			return rr, false, nil
		}
	}
	// remove maps a daemon conflict (dependent child images, a container created
	// since the check above) to a transient skip: retry next pass, converge when
	// the conflict clears.
	remove := func(ref string) (skip bool, err error) {
		if err := e.remove(ctx, ref, &rr); err != nil {
			if cerrdefs.IsConflict(err) {
				return true, nil
			}
			return false, z.Err(err, "remove %q", ref)
		}
		return false, nil
	}
	if len(digests) == 0 {
		// No references at all: the by-ID delete destroys content directly, and a
		// tag pull the in-flight registry cannot correlate to this ID may have
		// (re)referenced it — look one more time immediately before deleting.
		ins, err := e.cli.ImageInspect(ctx, id)
		if err != nil {
			if client.IsErrNotFound(err) {
				return rr, true, nil
			}
			return rr, false, z.Err(err, "image inspect")
		}
		if len(realRefs(ins.RepoTags))+len(realRefs(ins.RepoDigests)) > 0 {
			return rr, false, nil
		}
		skip, err := remove(id)
		return rr, !skip && err == nil, err
	}
	for _, d := range digests {
		skip, err := remove(d)
		if err != nil {
			return rr, false, err
		}
		if skip {
			return rr, false, nil
		}
	}
	// Content is freed when the last digest ref goes; no by-ID pass. If a racing
	// re-reference kept the content alive, the next scan reconciles.
	return rr, true, nil
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
