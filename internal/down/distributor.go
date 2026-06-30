package down

import (
	"context"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/z"
)

// Distributor fans a warmed cache reference out to downstream targets. It
// implements warm.Distributor.
type Distributor struct {
	reg   *Registry
	hosts map[string]string // target name -> pull-host override ("" = none)
}

// NewDistributor builds the fan-out. defaultHost overrides the registry host in
// the reference targets are told to pull (registry.downstream_host); a target's
// own pull_host takes precedence.
func NewDistributor(reg *Registry, defaultHost string) *Distributor {
	hosts := make(map[string]string)
	for _, e := range reg.entries {
		if e.cfg.PullHost != "" {
			hosts[e.cfg.Name] = e.cfg.PullHost
		} else {
			hosts[e.cfg.Name] = defaultHost
		}
	}
	return &Distributor{reg: reg, hosts: hosts}
}

// pullRef returns the reference target `name` should pull: the cache reference
// with its host swapped for the target's override, or the cache reference as-is.
func (d *Distributor) pullRef(name, cacheRef string) (string, error) {
	host := d.hosts[name]
	if host == "" {
		return cacheRef, nil
	}
	return rewriteHost(cacheRef, host)
}

// rewriteHost replaces the registry host of ref while preserving the repository
// path and tag/digest.
func rewriteHost(ref, host string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", z.Err(err, "parse cache ref %q", ref)
	}
	repo := r.Context().RepositoryStr()
	var out string
	if d, ok := r.(name.Digest); ok {
		out = host + "/" + repo + "@" + d.DigestStr()
	} else {
		out = host + "/" + repo + ":" + r.Identifier()
	}
	if _, err := name.ParseReference(out); err != nil {
		return "", z.Err(err, "invalid downstream ref %q", out)
	}
	return out, nil
}

type fanoutItem struct {
	name   string
	target Target // nil if the requested name is unknown
}

// Distribute pulls job.CacheRef on every selected target concurrently, recording
// per-target progress. One target's failure does not affect the others.
func (d *Distributor) Distribute(ctx context.Context, job *warm.Job, store warm.Store) {
	items := d.plan(job.RequestedTargets())

	refs := make([]string, len(items))
	refErrs := make([]error, len(items))
	for i, it := range items {
		refs[i], refErrs[i] = d.pullRef(it.name, job.CacheRef)
	}

	store.Update(job.ID, func(j *warm.Job) {
		j.Targets = make([]*warm.TargetProgress, len(items))
		for i, it := range items {
			kind := ""
			if it.target != nil {
				kind = it.target.Kind()
			}
			j.Targets[i] = &warm.TargetProgress{Name: it.name, Kind: kind, Ref: refs[i], State: "pending"}
		}
	})

	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(tp *warm.TargetProgress, t Target, ref string, refErr error) {
			defer wg.Done()
			switch {
			case t == nil:
				store.Update(job.ID, func(*warm.Job) { tp.State, tp.Err = "failed", "unknown target" })
				return
			case refErr != nil:
				store.Update(job.ID, func(*warm.Job) { tp.State, tp.Err = "failed", refErr.Error() })
				return
			}
			store.Update(job.ID, func(*warm.Job) { tp.State = "pulling" })
			err := t.Pull(ctx, ref)
			store.Update(job.ID, func(*warm.Job) {
				if err != nil {
					tp.State, tp.Err = "failed", err.Error()
				} else {
					tp.State = "pulled"
				}
			})
		}(job.Targets[i], items[i].target, refs[i], refErrs[i])
	}
	wg.Wait()
}

// plan resolves the requested names (empty = all) to targets, keeping unknown
// names so they surface as failed rows.
func (d *Distributor) plan(names []string) []fanoutItem {
	if len(names) == 0 {
		all := d.reg.All()
		items := make([]fanoutItem, len(all))
		for i, t := range all {
			items[i] = fanoutItem{name: t.Name(), target: t}
		}
		return items
	}
	items := make([]fanoutItem, len(names))
	for i, n := range names {
		t, _ := d.reg.Get(n)
		items[i] = fanoutItem{name: n, target: t}
	}
	return items
}
