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

// Redacted returns a shallow copy with secret values masked, for safe display
// (e.g. the `config` subcommand). Non-empty bearer tokens and store passwords
// are replaced with a placeholder.
func (c *Config) Redacted() *Config {
	const mask = "***"
	cp := *c
	cp.Serve.Auth.Tokens = append([]string(nil), c.Serve.Auth.Tokens...)
	for i, t := range cp.Serve.Auth.Tokens {
		if t != "" {
			cp.Serve.Auth.Tokens[i] = mask
		}
	}
	cp.Stores = make(map[string]StoreConfig, len(c.Stores))
	for name, s := range c.Stores {
		if s.Password != "" {
			s.Password = mask
		}
		cp.Stores[name] = s
	}
	return &cp
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
		// Env-expand credentials so ${VAR} works like it does for auth.tokens.
		s.Username = os.ExpandEnv(s.Username)
		s.Password = os.ExpandEnv(s.Password)
		switch s.Kind {
		case "oci":
			z.FallbackP(&s.Host, name) // a store named "docker.io" defaults its host
			z.FallbackP(&s.Mode, "copy")
			if s.Mode != "copy" && s.Mode != "proxy" {
				return z.Err(nil, "store %q: unknown mode %q", name, s.Mode)
			}
		case "docker", "containerd":
			// engine store; address is validated when the store is dialed
		default:
			return z.Err(nil, "store %q: unknown kind %q", name, s.Kind)
		}
		// A cred (client mTLS) applies to both registry (pull/push) and engine
		// (daemon) connections, so validate and default the device for any store
		// kind.
		if err := s.validateCred(); err != nil {
			return z.Err(err, "store %q", name)
		}
		if s.Cred.IsTPM() {
			z.FallbackP(&s.Cred.Device, "/dev/tpmrm0")
		}
		if s.Verify != nil && !s.Verify.Mode.Valid() {
			return z.Err(nil, "store %q: verify.mode %q is not one of off/verify-if-present/require", name, s.Verify.Mode)
		}
		if err := s.evaluateRetention(); err != nil {
			return z.Err(err, "store %q", name)
		}
		c.Stores[name] = s
	}

	// bbolt files take an exclusive lock, so two components sharing one path make
	// the second Open fail with an opaque timeout at startup. Every bbolt file —
	// per-store retention indexes, the audit log, and the verify cache — must have
	// its own path.
	bboltPaths := map[string]string{}
	claimBbolt := func(path, owner string) error {
		if path == "" {
			return nil
		}
		if other, dup := bboltPaths[path]; dup {
			return z.Err(nil, "%s and %s share bbolt path %q; each needs its own file", other, owner, path)
		}
		bboltPaths[path] = owner
		return nil
	}
	for name, s := range c.Stores {
		if !s.Retention.Enabled() {
			continue
		}
		if err := claimBbolt(s.Retention.Path, "stores."+name+".retention.path"); err != nil {
			return err
		}
	}
	if err := claimBbolt(c.Serve.Events.Path, "serve.events.path"); err != nil {
		return err
	}
	if err := claimBbolt(c.Serve.Verify.Cache.Path, "serve.verify.cache.path"); err != nil {
		return err
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
	// Build/validate the verifier config when verification runs anywhere OR
	// enforcement is on: enforcement needs the verifier + trust store even when
	// the admission verify mode is off.
	if c.NeedVerifier() {
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
	if err := c.evaluateVerifyCache(); err != nil {
		return err
	}
	if err := c.evaluateEnforce(); err != nil {
		return err
	}

	return nil
}

// evaluateVerifyCache applies defaults and validates the verdict cache config.
func (c *Config) evaluateVerifyCache() error {
	vc := &c.Serve.Verify.Cache
	if !vc.Enabled() {
		return nil
	}
	z.FallbackP((*time.Duration)(&vc.TTL), 28*24*time.Hour) // 4w
	if vc.TTL <= 0 {
		return z.Err(nil, "serve.verify.cache.ttl must be positive")
	}
	// Default refresh to 2w, but never above ttl — otherwise a config that only
	// shortens ttl (e.g. "ttl: 1w") would be rejected by the refresh<=ttl check
	// for a value the operator never set.
	if vc.Refresh <= 0 {
		def := Duration(14 * 24 * time.Hour) // 2w
		if vc.TTL < def {
			def = vc.TTL
		}
		vc.Refresh = def
	}
	if vc.Refresh > vc.TTL {
		return z.Err(nil, "serve.verify.cache.refresh (%s) must be <= ttl (%s)", time.Duration(vc.Refresh), time.Duration(vc.TTL))
	}
	return nil
}

// evaluateEnforce applies defaults and validates runtime enforcement config.
func (c *Config) evaluateEnforce() error {
	e := &c.Serve.Enforce
	// Validate the mode always so a typo (e.g. "quarintine") fails loudly rather
	// than silently disabling a security control.
	switch e.Mode {
	case "", "off", "quarantine":
	default:
		return z.Err(nil, "serve.enforce.mode %q is not one of off/quarantine", e.Mode)
	}
	if !e.Enabled() {
		return nil
	}
	z.FallbackP(&e.OnUnavailable, "grace")
	switch e.OnUnavailable {
	case "grace", "kill", "allow":
	default:
		return z.Err(nil, "serve.enforce.on_unavailable %q is not one of grace/kill/allow", e.OnUnavailable)
	}
	if len(e.Stores) == 0 {
		return z.Err(nil, "serve.enforce.stores must name at least one engine store when mode is quarantine")
	}
	for _, n := range e.Stores {
		s, ok := c.Stores[n]
		if !ok {
			return z.Err(nil, "serve.enforce.stores: unknown store %q", n)
		}
		if s.Kind != "docker" {
			return z.Err(nil, "serve.enforce.stores: store %q is kind %q; runtime enforcement currently supports only docker stores", n, s.Kind)
		}
	}
	// Offline verdicts are the whole point of enforcement surviving a registry
	// outage; grace has nothing to honor without a cache.
	if !c.Serve.Verify.Cache.Enabled() {
		return z.Err(nil, "serve.enforce requires serve.verify.cache.path (offline verdicts)")
	}
	// A trust store is required to verify anything at all.
	if c.Serve.Verify.TrustStore == "" {
		return z.Err(nil, "serve.enforce requires serve.verify.trust_store (the Root CA(s) to trust)")
	}
	return nil
}
