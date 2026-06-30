package config

import (
	"os"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/z"
)

var DefaultConfigPaths = []string{
	"gantry.yaml",
	"gantry.yml",
}

type Config struct {
	path string

	Greet GreetConfig

	Otel OtelConfig

	Serve ServeConfig
}

func ReadFromFile(p string) (*Config, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, z.Err(err, "open")
	}

	var c Config
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		return nil, z.Err(err, "decode")
	}

	c.path = p
	return &c, nil
}

func (c *Config) Path() string {
	return c.path
}

func (c *Config) Evaluate() error {
	z.FallbackP(&c.Greet.Format, "Hello, %s!")

	z.FallbackP(&c.Serve.Addr, ":8080")
	z.FallbackP((*time.Duration)(&c.Serve.ShutdownGrace), 15*time.Second)
	z.FallbackP(&c.Serve.Warm.MaxConcurrentJobs, 2)
	z.FallbackP(&c.Serve.Warm.MaxConcurrentLayers, 4)
	z.FallbackP(&c.Serve.Warm.QueueSize, 256)
	z.FallbackP((*time.Duration)(&c.Serve.Warm.JobTTL), 30*time.Minute)

	seen := map[string]bool{}
	for i := range c.Serve.Stores {
		s := &c.Serve.Stores[i]
		if s.Name == "" {
			return z.Err(nil, "store[%d] has no name", i)
		}
		if seen[s.Name] {
			return z.Err(nil, "duplicate store name %q", s.Name)
		}
		seen[s.Name] = true

		switch s.Kind {
		case "registry":
			z.FallbackP(&s.Host, s.Name) // a store named "docker.io" defaults its host
			z.FallbackP(&s.Mode, "copy")
			if s.Mode != "copy" && s.Mode != "proxy" {
				return z.Err(nil, "store %q: unknown mode %q", s.Name, s.Mode)
			}
			if len(s.Rewrite) == 0 {
				s.Rewrite = []RewriteRule{{Pattern: "**", Template: "{{.CacheHost}}/{{.Repo}}"}}
			}
			for j := range s.Rewrite {
				if err := s.Rewrite[j].compile(); err != nil {
					return z.Err(err, "store %q rewrite[%d]", s.Name, j)
				}
			}
		case "docker", "containerd":
			// engine store; address is validated when the store is dialed
		default:
			return z.Err(nil, "store %q: unknown kind %q", s.Name, s.Kind)
		}
	}

	return nil
}
