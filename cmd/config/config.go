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

	for name, s := range c.Serve.Stores {
		if name == "" {
			return z.Err(nil, "a store has an empty name")
		}
		s.Name = name
		switch s.Kind {
		case "oci":
			z.FallbackP(&s.Host, name) // a store named "docker.io" defaults its host
			z.FallbackP(&s.Mode, "copy")
			if s.Mode != "copy" && s.Mode != "proxy" {
				return z.Err(nil, "store %q: unknown mode %q", name, s.Mode)
			}
			if len(s.Rewrite) == 0 {
				s.Rewrite = DefaultRewrite()
			} else {
				for j := range s.Rewrite {
					if err := s.Rewrite[j].compile(); err != nil {
						return z.Err(err, "store %q rewrite[%d]", name, j)
					}
				}
			}
		case "docker", "containerd":
			// engine store; address is validated when the store is dialed
		default:
			return z.Err(nil, "store %q: unknown kind %q", name, s.Kind)
		}
		c.Serve.Stores[name] = s
	}

	if c.Serve.Retention.Enabled() {
		z.FallbackP((*time.Duration)(&c.Serve.Retention.Interval), time.Hour)
		z.FallbackP((*time.Duration)(&c.Serve.Retention.MinInterval), time.Minute)
		// Grace defaults to MaxAge: protect everything for one max-age window after
		// startup so the downtime gap can't trigger wrongful deletion.
		if c.Serve.Retention.Grace == 0 {
			c.Serve.Retention.Grace = c.Serve.Retention.MaxAge
		}
	}

	return nil
}
