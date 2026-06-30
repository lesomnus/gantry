package warm

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
)

// compiledRules builds rewrite rules and compiles them via Config.Evaluate
// (RewriteRule.compile is unexported in the config package).
func compiledRules(t *testing.T, pairs ...[2]string) []config.RewriteRule {
	t.Helper()
	st := config.StoreConfig{Name: "cache", Kind: "registry", Host: "cache.local"}
	for _, p := range pairs {
		st.Rewrite = append(st.Rewrite, config.RewriteRule{Pattern: p[0], Template: p[1]})
	}
	c := config.Config{}
	c.Serve.Stores = []config.StoreConfig{st}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate rules: %v", err)
	}
	return c.Serve.Stores[0].Rewrite
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRewrite(t *testing.T) {
	t.Run("identity keeps the whole ref", func(t *testing.T) {
		rules := compiledRules(t, [2]string{"**", "{{.Full}}"})
		got, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis:7"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != "index.docker.io/library/redis:7" {
			t.Errorf("got %q", got.Name())
		}
	})
	t.Run("relocate by repo appends source tag", func(t *testing.T) {
		rules := compiledRules(t, [2]string{"**", "{{.CacheHost}}/{{.Repo}}"})
		got, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis:7"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != "cache.local/library/redis:7" {
			t.Errorf("got %q", got.Name())
		}
	})
	t.Run("registry prefix exposes normalized host", func(t *testing.T) {
		rules := compiledRules(t, [2]string{"**", "{{.CacheHost}}/{{.Registry}}/{{.Repo}}"})
		got, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis:7"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != "cache.local/index.docker.io/library/redis:7" {
			t.Errorf("got %q", got.Name())
		}
	})
	t.Run("first matching rule wins", func(t *testing.T) {
		rules := compiledRules(t,
			[2]string{"ghcr.io/**", "{{.CacheHost}}/gh/{{.Repo}}"},
			[2]string{"**", "{{.CacheHost}}/{{.Repo}}"},
		)
		gh, err := Rewrite(rules, "cache.local", mustRef(t, "ghcr.io/acme/app:2"), false)
		if err != nil {
			t.Fatal(err)
		}
		if gh.Name() != "cache.local/gh/acme/app:2" {
			t.Errorf("ghcr got %q", gh.Name())
		}
		dh, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis:7"), false)
		if err != nil {
			t.Fatal(err)
		}
		if dh.Name() != "cache.local/library/redis:7" {
			t.Errorf("dockerhub got %q", dh.Name())
		}
	})
	t.Run("digest reference is preserved", func(t *testing.T) {
		const dig = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		rules := compiledRules(t, [2]string{"**", "{{.CacheHost}}/{{.Repo}}"})
		got, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis@"+dig), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != "cache.local/library/redis@"+dig {
			t.Errorf("got %q", got.Name())
		}
	})
	t.Run("no matching rule errors", func(t *testing.T) {
		rules := compiledRules(t, [2]string{"ghcr.io/**", "{{.CacheHost}}/{{.Repo}}"})
		if _, err := Rewrite(rules, "cache.local", mustRef(t, "docker.io/library/redis:7"), false); err == nil {
			t.Error("expected no-match error")
		}
	})
}
