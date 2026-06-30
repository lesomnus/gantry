package warm

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/z"
)

// RefVars are the fields exposed to rewrite templates.
type RefVars struct {
	Ref        string // the parsed reference as a string
	Full       string // fully-qualified name (registry/repo:tag)
	CacheHost  string // registry.host
	Registry   string // e.g. "index.docker.io"
	Repo       string // e.g. "library/redis"
	Tag        string // set for tag references
	Digest     string // set for digest references
	Identifier string // tag or digest
}

func refVars(ref name.Reference, cacheHost string) RefVars {
	repo := ref.Context()
	v := RefVars{
		Ref:        ref.String(),
		Full:       ref.Name(),
		CacheHost:  cacheHost,
		Registry:   repo.RegistryStr(),
		Repo:       repo.RepositoryStr(),
		Identifier: ref.Identifier(),
	}
	switch r := ref.(type) {
	case name.Tag:
		v.Tag = r.TagStr()
	case name.Digest:
		v.Digest = r.DigestStr()
	}
	return v
}

// Rewrite maps a source reference to its cache-side reference using the first
// matching rule. If the rendered reference omits a tag/digest, the source
// identifier is carried over so "cache/{{.Repo}}" keeps the original ":7".
func Rewrite(rules []config.RewriteRule, cacheHost string, ref name.Reference, insecure bool) (name.Reference, error) {
	full := ref.Name()
	vars := refVars(ref, cacheHost)
	for i := range rules {
		ok, err := doublestar.Match(rules[i].Pattern, full)
		if err != nil {
			return nil, z.Err(err, "match glob %q", rules[i].Pattern)
		}
		if !ok {
			continue
		}
		out, err := rules[i].Render(vars)
		if err != nil {
			return nil, err
		}
		out = ensureIdentifier(out, ref)

		var opts []name.Option
		if insecure {
			opts = append(opts, name.Insecure)
		}
		dst, err := name.ParseReference(out, opts...)
		if err != nil {
			return nil, z.Err(err, "parse rewritten ref %q", out)
		}
		return dst, nil
	}
	return nil, z.Err(nil, "no rewrite rule matched %q", full)
}

// ensureIdentifier appends the source reference's tag/digest when the rendered
// string has none. A ':' is only treated as a tag when it is in the last path
// segment (otherwise it is a registry port).
func ensureIdentifier(s string, src name.Reference) string {
	last := s
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		last = s[i+1:]
	}
	if strings.ContainsAny(last, ":@") {
		return s
	}
	if d, ok := src.(name.Digest); ok {
		return s + "@" + d.DigestStr()
	}
	return s + ":" + src.Identifier()
}
