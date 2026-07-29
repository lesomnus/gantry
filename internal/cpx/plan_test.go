package cpx

import (
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
)

// stubDest is a destination that is neither pushed to nor pulls; validate() only
// needs its identity.
type stubDest struct{ name string }

func (d stubDest) Name() string { return d.name }
func (d stubDest) Kind() string { return "oci" }

func planWith(target string, steps ...*execStep) *execPlan {
	p := &execPlan{target: stubDest{target}, steps: steps}
	for i, st := range p.steps {
		st.idx = i
	}
	return p
}

func step(store string, attempts int) *execStep {
	st := &execStep{
		dst:      stubDest{store},
		newMover: func(*Copier, *execAttempt) (mover, error) { return nil, nil },
	}
	for range attempts {
		st.attempts = append(st.attempts, &execAttempt{})
	}
	return st
}

// A plan's invariants are cheap to check at admission and expensive to debug from
// a client reporting an intermediate store as a job's target.
func TestPlanValidate(t *testing.T) {
	t.Run("a single delivering step", func(t *testing.T) {
		if err := planWith("cache", step("cache", 1)).validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("a route ending at the target", func(t *testing.T) {
		if err := planWith("local", step("site", 1), step("local", 2)).validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	for _, tc := range []struct {
		name string
		plan *execPlan
		want string
	}{
		{"no steps", planWith("cache"), "no steps"},
		{
			// The job would report a finished move while a later hop overwrote it.
			"an early step delivers to the target",
			planWith("local", step("local", 1), step("local", 1)),
			"only the last step may target",
		},
		{
			// Nothing would put the image where the caller asked.
			"the last step misses the target",
			planWith("local", step("site", 1), step("other", 1)),
			"only the last step may target",
		},
		{"a step with no attempts", planWith("cache", step("cache", 0)), "no attempts"},
		{
			// Each attempt can move a whole image, so the bound is a cost bound.
			"more attempts than allowed",
			planWith("local", step("site", 4), step("local", 4)),
			"more than the",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}

	t.Run("a step without a runner", func(t *testing.T) {
		p := planWith("cache", step("cache", 1))
		p.steps[0].newMover = nil
		if err := p.validate(); err == nil || !strings.Contains(err.Error(), "no runner") {
			t.Fatalf("err = %v, want one mentioning the missing runner", err)
		}
	})
}

// The plan is what the job reports and what every hop is anchored to, so pinning
// must reach each attempt built after it.
func TestPlanPinReachesEveryAttempt(t *testing.T) {
	const dg = "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: "up.example", Insecure: true},
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	ref, _ := name.ParseReference("up.example/a/b:1", name.Insecure)
	p := &execPlan{repo: "a/b", id: ":1", authorityRef: ref}

	if got := p.digest(); got != "" {
		t.Fatalf("an unpinned plan reports digest %q", got)
	}
	p.pin(dg)
	if got := p.digest(); got != dg {
		t.Fatalf("digest = %q, want %q", got, dg)
	}
	cache, _ := w.stores.Config("cache")
	at, err := w.planAttemptRef(p, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := at.(name.Digest); !ok {
		t.Errorf("attempt ref = %q, want the pinned digest form", at.Name())
	}
	if want := "cache.example/a/b@" + dg; at.Name() != want {
		t.Errorf("attempt ref = %q, want %q", at.Name(), want)
	}
}
