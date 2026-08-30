package store

import (
	"strings"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
)

// fleet is the shape this resolution exists for: three docker daemons that are
// told apart by address, plus a registry that must never answer an engine
// lookup.
func fleet(t *testing.T) *Set {
	t.Helper()
	s, err := NewSet(map[string]config.StoreConfig{
		"rt":     {Kind: "docker", Address: "tcp://192.168.10.2:2376"},
		"work":   {Kind: "docker", Address: "tcp://192.168.10.33:2376"},
		"hday":   {Kind: "docker", Address: "tcp://192.168.10.34:2376"},
		"local":  {Kind: "oci", Host: "127.0.0.1:5000"},
		"socket": {Kind: "docker", Address: "unix:///var/run/docker.sock"},
	}, false)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return s
}

// TestEngineByName is the behaviour that existed before selectors and must not
// have moved: a declared name resolves, and nothing else changed about it.
func TestEngineByName(t *testing.T) {
	s := fleet(t)
	for _, name := range []string{"rt", "work", "hday", "socket"} {
		e, err := s.Engine(name)
		if err != nil {
			t.Errorf("Engine(%q): %v", name, err)
			continue
		}
		if e.Name() != name {
			t.Errorf("Engine(%q).Name() = %q", name, e.Name())
		}
	}
}

// TestEngineBySelector is the point of the change: a caller that knows where a
// daemon is does not also have to know what this config called it.
func TestEngineBySelector(t *testing.T) {
	s := fleet(t)
	for _, tc := range []struct{ ref, want string }{
		{"docker:192.168.10.34", "hday"},     // kind and host
		{"192.168.10.34", "hday"},            // host alone
		{"192.168.10.34:2376", "hday"},       // host and port
		{"tcp://192.168.10.34:2376", "hday"}, // the address verbatim
		{"192.168.10.2", "rt"},
		{"192.168.10.33", "work"},
	} {
		e, err := s.Engine(tc.ref)
		if err != nil {
			t.Errorf("Engine(%q): %v", tc.ref, err)
			continue
		}
		if e.Name() != tc.want {
			t.Errorf("Engine(%q) = %q, want %q", tc.ref, e.Name(), tc.want)
		}
	}
}

// TestEngineSelectorRefusesWrongKindAndPort: a selector that names something
// specific must not match something else. Being wrong here means warming onto
// the wrong daemon, which nothing downstream would notice.
func TestEngineSelectorRefusesWrongKindAndPort(t *testing.T) {
	s := fleet(t)
	for _, ref := range []string{
		"containerd:192.168.10.34", // right host, wrong kind
		"192.168.10.34:9999",       // right host, wrong port
		"192.168.10.99",            // no such daemon
		"127.0.0.1:5000",           // a registry, not an engine
	} {
		if _, err := s.Engine(ref); err == nil {
			t.Errorf("Engine(%q) should not resolve", ref)
		}
	}
}

// TestEngineSelectorRefusesAmbiguity: two stores on one daemon is a
// configuration somebody meant something by, and picking one would be a warm
// that silently lands in the wrong place.
func TestEngineSelectorRefusesAmbiguity(t *testing.T) {
	s, err := NewSet(map[string]config.StoreConfig{
		"a": {Kind: "docker", Address: "tcp://10.0.0.1:2376"},
		"b": {Kind: "docker", Address: "tcp://10.0.0.1:2376"},
	}, false)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	_, err = s.Engine("10.0.0.1")
	if err == nil {
		t.Fatal("an ambiguous selector should be an error, not a pick")
	}
	if !strings.Contains(err.Error(), "matches 2 stores") {
		t.Errorf("the error should say what is ambiguous, got: %v", err)
	}
}

// TestEngineSocketStoreNeverMatchesASelector: a unix socket names no host, so
// no host selector may reach it. Otherwise an empty host would match everything.
func TestEngineSocketStoreNeverMatchesASelector(t *testing.T) {
	s := fleet(t)
	for _, ref := range []string{"", "docker:", "/var/run/docker.sock", "unix:///var/run/docker.sock"} {
		if e, err := s.Engine(ref); err == nil {
			t.Errorf("Engine(%q) resolved to %q; a socket store has no host to select", ref, e.Name())
		}
	}
}

// TestEngineErrorNamesTheAlternatives: the failure a caller actually hits is a
// name that does not exist, and the useful answer is which ones do.
func TestEngineErrorNamesTheAlternatives(t *testing.T) {
	s := fleet(t)
	_, err := s.Engine("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"rt", "work", "hday"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list declared engine %q, got: %v", want, err)
		}
	}
	if _, err := s.Engine("local"); err == nil || !strings.Contains(err.Error(), "not an engine") {
		t.Errorf("a declared registry should say it is not an engine, got: %v", err)
	}
}

// TestEngineConfigAgreesWithEngine: the decision "is this target an engine" and
// the dial that follows must read a reference the same way. They did not --
// the decision was a name-map lookup, so a selector fell through to the
// registry path and failed as an unknown REGISTRY, naming the wrong half of the
// config and never reaching the engine resolution at all.
func TestEngineConfigAgreesWithEngine(t *testing.T) {
	s := fleet(t)
	for _, ref := range []string{"hday", "192.168.10.34", "docker:192.168.10.34", "tcp://192.168.10.34:2376"} {
		c, ok := s.EngineConfig(ref)
		if !ok {
			t.Errorf("EngineConfig(%q) = not an engine; Engine() resolves it", ref)
			continue
		}
		if c.Name != "hday" {
			t.Errorf("EngineConfig(%q).Name = %q, want hday", ref, c.Name)
		}
		if _, err := s.Engine(ref); err != nil {
			t.Errorf("Engine(%q): %v", ref, err)
		}
	}
	// A registry, an unknown host and an ambiguous selector are all "not an
	// engine" here, so the caller falls through to the registry path as before.
	for _, ref := range []string{"local", "127.0.0.1:5000", "10.9.9.9", "nope"} {
		if c, ok := s.EngineConfig(ref); ok {
			t.Errorf("EngineConfig(%q) = %q; want not-an-engine", ref, c.Name)
		}
	}
}

// routed is the fleet this feature exists for: one name a job points at, and
// two registries behind it that hold different things.
func routed(t *testing.T) *Set {
	t.Helper()
	s, err := NewSet(map[string]config.StoreConfig{
		"remote": {Kind: "meta", Routes: []config.Route{
			{ForRepos: []string{"dist/**"}, Store: "cdn"},
			{Store: "internal"},
		}},
		"cdn":      {Kind: "oci", Host: "registry.hday.io"},
		"internal": {Kind: "oci", Host: "cr.hday.io"},
	}, false)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return s
}

// The whole point: the same store name answers with a different registry
// depending on the repository, and the caller gets a config it can connect to.
func TestMetaRoutesByRepository(t *testing.T) {
	s := routed(t)
	for _, tc := range []struct{ repo, want string }{
		{"dist/hday/cove", "registry.hday.io"},
		{"dist/infra", "registry.hday.io"},
		{"stage/hday/cove", "cr.hday.io"},
		{"hday/kamino", "cr.hday.io"},
	} {
		c, err := s.Registry("remote", tc.repo)
		if err != nil {
			t.Fatalf("Registry(remote, %q): %v", tc.repo, err)
		}
		if c.Host != tc.want {
			t.Errorf("repo %q routed to %q, want %q", tc.repo, c.Host, tc.want)
		}
		if c.IsMeta() {
			t.Errorf("repo %q resolved to a meta store; meta must never leave Registry", tc.repo)
		}
	}
}

// A destination has no repository to route on, and a policy is not a place to
// push to. The error has to say so rather than pick the first route.
func TestMetaIsNotADestination(t *testing.T) {
	s := routed(t)
	_, err := s.Registry("remote", "")
	if err == nil {
		t.Fatal("a meta store resolved with no repository")
	}
	if !strings.Contains(err.Error(), "only be a source") {
		t.Errorf("error does not say why: %v", err)
	}
}

// A meta store that covers only some repositories is a deliberate config, so
// the uncovered case is an error naming the repository and what is covered.
func TestMetaWithoutAMatchingRouteSaysWhatItCovers(t *testing.T) {
	s, err := NewSet(map[string]config.StoreConfig{
		"remote": {Kind: "meta", Routes: []config.Route{
			{ForRepos: []string{"dist/**"}, Store: "cdn"},
		}},
		"cdn": {Kind: "oci", Host: "registry.hday.io"},
	}, false)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	_, err = s.Registry("remote", "stage/hday/cove")
	if err == nil {
		t.Fatal("an uncovered repository resolved")
	}
	if !strings.Contains(err.Error(), "stage/hday/cove") || !strings.Contains(err.Error(), "dist/**") {
		t.Errorf("error names neither the repository nor the routes: %v", err)
	}
}

// A meta store is not an engine and must not answer an engine lookup, the same
// way a registry does not.
func TestMetaIsNotAnEngine(t *testing.T) {
	s := routed(t)
	if _, ok := s.EngineConfig("remote"); ok {
		t.Fatal("a meta store answered an engine lookup")
	}
}

// StoreStatuses walks every store, and a meta store has no engine to probe --
// the branch that assumed everything not a registry is one used to panic here.
func TestMetaStatusDoesNotProbe(t *testing.T) {
	s := routed(t)
	var seen bool
	for _, st := range s.StoreStatuses(t.Context()) {
		if st.Name != "remote" {
			continue
		}
		seen = true
		if st.Host != "" {
			t.Errorf("meta store reports host %q; it has none", st.Host)
		}
		if !st.Ready {
			t.Errorf("meta store is not ready: %s", st.Error)
		}
	}
	if !seen {
		t.Fatal("meta store is missing from the status list")
	}
}
