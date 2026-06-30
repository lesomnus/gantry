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
	z.FallbackP(&c.Serve.Registry.Mode, "copy")
	z.FallbackP(&c.Serve.Warm.MaxConcurrentJobs, 2)
	z.FallbackP(&c.Serve.Warm.MaxConcurrentLayers, 4)
	z.FallbackP(&c.Serve.Warm.QueueSize, 256)
	z.FallbackP((*time.Duration)(&c.Serve.Warm.JobTTL), 30*time.Minute)

	for i := range c.Serve.Targets {
		if c.Serve.Targets[i].Kind == "containerd" {
			z.FallbackP(&c.Serve.Targets[i].Namespace, "k8s.io")
		}
	}

	if len(c.Serve.Registry.Rewrite) == 0 {
		c.Serve.Registry.Rewrite = []RewriteRule{{Pattern: "**", Template: "{{.CacheHost}}/{{.Repo}}"}}
	}
	for i := range c.Serve.Registry.Rewrite {
		if err := c.Serve.Registry.Rewrite[i].compile(); err != nil {
			return z.Err(err, "rewrite[%d]", i)
		}
	}

	return nil
}
