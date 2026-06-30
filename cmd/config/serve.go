package config

// ServeConfig configures the `serve` subcommand: the HTTP API, the cache
// registry that workers warm, and the downstream daemons that are triggered.
type ServeConfig struct {
	Addr          string         `yaml:"addr"`
	ShutdownGrace Duration       `yaml:"shutdown_grace"`
	Auth          AuthConfig     `yaml:"auth"`
	Registry      RegistryConfig `yaml:"registry"`
	Warm          WarmConfig     `yaml:"warm"`
	Targets       []TargetConfig `yaml:"targets"`
}

// AuthConfig guards /v1/* (/healthz is always exempt).
type AuthConfig struct {
	// Tokens is a bearer-token whitelist; if non-empty, /v1/* requires one.
	Tokens []string `yaml:"tokens"`
	// ClientCA, if set, accepts a verified mTLS client certificate in lieu of a token.
	ClientCA string `yaml:"client_ca"`
	// TLSCert/TLSKey serve the API over HTTPS; empty means plain HTTP (TLS terminated upstream).
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
}

// RegistryConfig describes the cache registry: copy mode pushes upstream blobs
// into it, proxy mode triggers it to self-fill via pull-through.
type RegistryConfig struct {
	// Mode selects the warming strategy: "copy" (default) or "proxy".
	Mode string `yaml:"mode"`
	// Host is the cache registry host gantry pushes to / reads from, also
	// exposed to rewrite templates as {{.CacheHost}}.
	Host string `yaml:"host"`
	// DownstreamHost, if set, overrides the registry host in the reference that
	// downstream targets are told to pull (e.g. push to "192.168.0.22:5000" but
	// have daemons pull "cache.cr.com" — a name they trust and resolve to it).
	// Per-target pull_host takes precedence.
	DownstreamHost string `yaml:"downstream_host"`
	// Insecure allows plain-HTTP or self-signed cache registries.
	Insecure bool   `yaml:"insecure"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Rewrite maps a source reference to its cache-side reference; rules are
	// evaluated in order and the first matching pattern wins.
	Rewrite []RewriteRule `yaml:"rewrite"`
}

// WarmConfig bounds the warm worker pool.
type WarmConfig struct {
	// Platforms is the fallback platform set when a request omits it; empty
	// means the host GOOS/GOARCH only.
	Platforms []string `yaml:"platforms"`
	// MaxConcurrentJobs caps how many warm jobs run at once (tier-1 worker pool).
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	// MaxConcurrentLayers caps how many layers one job pulls at once (tier-2).
	MaxConcurrentLayers int `yaml:"max_concurrent_layers"`
	// QueueSize is the buffered depth of the pending-job channel.
	QueueSize int `yaml:"queue_size"`
	// JobTTL is how long a completed job record is retained.
	JobTTL Duration `yaml:"job_ttl"`
	// TriggerDownstream warms then fans out to targets by default.
	TriggerDownstream bool `yaml:"trigger_downstream"`
}

// TargetConfig describes one downstream daemon to trigger after warming.
type TargetConfig struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"` // docker | containerd
	Address string `yaml:"address"`
	// Namespace is the containerd namespace (e.g. "k8s.io" for k3s).
	Namespace string `yaml:"namespace"`
	// PullHost overrides the registry host this target is told to pull from,
	// taking precedence over registry.downstream_host. Empty = use the warmed
	// cache reference as-is.
	PullHost string `yaml:"pull_host"`
	// Platforms optionally narrows which platforms this target is told to pull.
	Platforms []string `yaml:"platforms"`
}
