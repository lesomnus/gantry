package config

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/z"
)

// RewriteRule maps a source reference glob to a destination reference template.
// In YAML it is written as a single-key mapping, e.g. {"ghcr.io/**": "{{.CacheHost}}/{{.Repo}}"},
// so that an ordered array of rules keeps its priority (first match wins).
type RewriteRule struct {
	Pattern  string
	Template string

	tmpl *template.Template
}

// UnmarshalYAML implements goccy's BytesUnmarshaler so a rule reads as a
// one-entry mapping while the surrounding sequence preserves ordering.
func (r *RewriteRule) UnmarshalYAML(b []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(m) != 1 {
		return fmt.Errorf("rewrite rule must be a single {pattern: template}, got %d entries", len(m))
	}
	for k, v := range m {
		r.Pattern = k
		r.Template = v
	}
	return nil
}

// MarshalYAML implements goccy's InterfaceMarshaler so a rule is emitted in the
// same single-key {pattern: template} form it is read from.
func (r RewriteRule) MarshalYAML() (interface{}, error) {
	return map[string]string{r.Pattern: r.Template}, nil
}

// compile parses the destination template; Config.Evaluate calls it to fail fast.
func (r *RewriteRule) compile() error {
	t, err := template.New(r.Pattern).Parse(r.Template)
	if err != nil {
		return z.Err(err, "parse template %q", r.Template)
	}
	r.tmpl = t
	return nil
}

// Render executes the destination template against data (the parsed reference's fields).
func (r *RewriteRule) Render(data any) (string, error) {
	if r.tmpl == nil {
		return "", fmt.Errorf("rewrite rule %q is not compiled", r.Pattern)
	}
	var sb strings.Builder
	if err := r.tmpl.Execute(&sb, data); err != nil {
		return "", z.Err(err, "render template %q", r.Template)
	}
	return sb.String(), nil
}
