package config

import (
	"os"
	"strings"
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

	// Stores are the image stores gantry can move images between, keyed by name
	// (order is not significant). A store is an OCI registry (kind "oci") or an
	// engine daemon (kind "docker"/"containerd").
	Stores map[string]StoreConfig

	// Worker bounds the job worker pool that runs image moves.
	Worker WorkerConfig
}

func ReadFromFile(p string) (*Config, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, z.Err(err, "open")
	}

	var c Config
	// Strict decode: an unknown key is an error, not a silent no-op. A typo like
	// a misindented `mode:` under serve.verify must fail loudly rather than
	// quietly disabling a security control.
	if err := yaml.NewDecoder(f, yaml.DisallowUnknownField()).Decode(&c); err != nil {
		return nil, z.Err(err, "decode")
	}

	c.path = p
	return &c, nil
}

func (c *Config) Path() string {
	return c.path
}

// engineHost normalizes an engine address the way the docker store dials it
// (internal/down dockerHost): empty means the default local socket, a bare
// path gets the unix scheme. Keep the two in sync.
func engineHost(addr string) string {
	switch {
	case addr == "":
		return "unix:///var/run/docker.sock"
	case strings.Contains(addr, "://"):
		return addr
	default:
		return "unix://" + addr
	}
}

func (c *Config) Evaluate() error {
	z.FallbackP(&c.Greet.Format, "Hello, %s!")

	z.FallbackP(&c.Serve.Addr, ":8080")
	z.FallbackP((*time.Duration)(&c.Serve.ShutdownGrace), 15*time.Second)
	z.FallbackP(&c.Worker.MaxConcurrentJobs, 2)
	z.FallbackP(&c.Worker.MaxConcurrentLayers, 4)
	z.FallbackP(&c.Worker.QueueSize, 256)
	z.FallbackP((*time.Duration)(&c.Worker.JobTTL), 30*time.Minute)

	for name, s := range c.Stores {
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
		// TPM mTLS applies to both registry (pull/push) and engine (daemon)
		// connections, so validate and default the device for any store kind.
		if err := s.validateTPM(); err != nil {
			return z.Err(err, "store %q", name)
		}
		if s.HasTPM() {
			z.FallbackP(&s.TPMDevice, "/dev/tpmrm0")
		}
		if s.Verify != nil && !s.Verify.Mode.Valid() {
			return z.Err(nil, "store %q: verify.mode %q is not one of off/verify-if-present/require", name, s.Verify.Mode)
		}
		if err := s.evaluateRetention(); err != nil {
			return z.Err(err, "store %q", name)
		}
		c.Stores[name] = s
	}

	// Two stores must not share a retention index file: bbolt takes an exclusive
	// lock, so the second Open would fail with an opaque timeout at startup.
	retentionPaths := map[string]string{}
	for name, s := range c.Stores {
		if !s.Retention.Enabled() {
			continue
		}
		if other, dup := retentionPaths[s.Retention.Path]; dup {
			return z.Err(nil, "stores %q and %q share retention.path %q; each store needs its own index file", other, name, s.Retention.Path)
		}
		retentionPaths[s.Retention.Path] = name
	}

	// Two docker stores must not reap untagged images on the same daemon: each
	// would run an independent reap clock and pin set over the same image store,
	// so one store's reaper deletes what the other believes it protects. The
	// comparison is by normalized address spelling — symlinked socket paths or
	// host aliases of one daemon still evade it.
	reapAddrs := map[string]string{}
	for name, s := range c.Stores {
		if s.Kind != "docker" || !s.Retention.Enabled() || s.Retention.UntaggedReapAfter() <= 0 {
			continue
		}
		addr := engineHost(s.Address)
		if other, dup := reapAddrs[addr]; dup {
			return z.Err(nil, "stores %q and %q reap untagged images on the same docker daemon (%s); turn one off with retention.untagged_after: \"0s\"", other, name, addr)
		}
		reapAddrs[addr] = name
	}

	z.FallbackP((*time.Duration)(&c.Serve.Health.CacheTTL), 5*time.Second)
	z.FallbackP((*time.Duration)(&c.Serve.Health.ProbeTimeout), 3*time.Second)
	for _, n := range c.Serve.Health.ReadyStores {
		// A typo here would otherwise make the node permanently NotReady with no
		// startup diagnostic (Check returns unknown-store forever).
		if _, ok := c.Stores[n]; !ok {
			return z.Err(nil, "serve.health.ready_stores: unknown store %q", n)
		}
	}

	if !c.Serve.Verify.Mode.Valid() {
		return z.Err(nil, "serve.verify.mode %q is not one of off/verify-if-present/require", c.Serve.Verify.Mode)
	}
	if c.VerifyEnabled() {
		z.FallbackP(&c.Serve.Verify.Provider, "notation")
		z.FallbackP(&c.Serve.Verify.Level, "strict")
		z.FallbackP((*time.Duration)(&c.Serve.Verify.Timeout), 15*time.Second)
		if c.Serve.Verify.Provider != "notation" {
			return z.Err(nil, "serve.verify.provider %q is not supported (only \"notation\")", c.Serve.Verify.Provider)
		}
		switch c.Serve.Verify.Level {
		case "strict", "permissive":
		default:
			// "audit" is intentionally rejected: it downgrades trust-anchor
			// (authenticity) checks to log-only, so a job would be admitted even
			// for a signature that does not chain to the trust store.
			return z.Err(nil, "serve.verify.level %q is not one of strict/permissive", c.Serve.Verify.Level)
		}
	}

	return nil
}
