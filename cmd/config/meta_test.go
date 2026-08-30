package config

import (
	"strings"
	"testing"
)

const metaFleet = `
stores:
  remote:
    kind: meta
    routes:
      - for_repos: ["dist/**"]
        store: cdn
      - store: internal
  cdn:
    kind: oci
    host: registry.hday.io
  internal:
    kind: oci
    host: cr.hday.io
`

func TestMetaStoreLoads(t *testing.T) {
	c, err := evalYAML(t, metaFleet)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	s := c.Stores["remote"]
	if !s.IsMeta() || s.IsRegistry() || s.IsEngine() {
		t.Fatalf("kind predicates disagree: %+v", s)
	}
	if got := s.RouteFor("dist/hday/cove"); got != "cdn" {
		t.Errorf("dist route = %q, want cdn", got)
	}
	if got := s.RouteFor("stage/hday/cove"); got != "internal" {
		t.Errorf("default route = %q, want internal", got)
	}
}

// Each rejection below is a field an operator could reasonably write on a meta
// store and be silently wrong about, so each has to be refused by name.
func TestMetaStoreRejections(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			"host", `
stores:
  remote: {kind: meta, host: registry.hday.io, routes: [{store: cdn}]}
  cdn: {kind: oci, host: registry.hday.io}
`, "no host of its own",
		},
		{
			"credential", `
stores:
  remote: {kind: meta, token_file: /opt/hday/robot/registry-token, routes: [{store: cdn}]}
  cdn: {kind: oci, host: registry.hday.io}
`, "holds no credential",
		},
		{
			"no routes", `
stores:
  remote: {kind: meta}
`, "at least one",
		},
		{
			"routes on a registry", `
stores:
  cdn: {kind: oci, host: registry.hday.io, routes: [{store: other}]}
  other: {kind: oci, host: cr.hday.io}
`, "belongs to a meta store",
		},
		{
			"route to an engine", `
stores:
  remote: {kind: meta, routes: [{store: work}]}
  work: {kind: docker, address: "tcp://192.168.10.33:2376"}
`, "selects a registry to read from",
		},
		{
			"route to another meta store", `
stores:
  remote: {kind: meta, routes: [{store: other}]}
  other: {kind: meta, routes: [{store: cdn}]}
  cdn: {kind: oci, host: registry.hday.io}
`, "selects a registry to read from",
		},
		{
			"undeclared route", `
stores:
  remote: {kind: meta, routes: [{store: nope}]}
`, "not a declared store",
		},
		{
			"host-qualified pattern", `
stores:
  remote:
    kind: meta
    routes:
      - {for_repos: ["cr.hday.io/dist/**"], store: cdn}
  cdn: {kind: oci, host: registry.hday.io}
`, "looks host-qualified",
		},
		{
			"unreachable route", `
stores:
  remote:
    kind: meta
    routes:
      - {store: internal}
      - {for_repos: ["dist/**"], store: cdn}
  cdn: {kind: oci, host: registry.hday.io}
  internal: {kind: oci, host: cr.hday.io}
`, "unreachable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evalYAML(t, tc.src)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
