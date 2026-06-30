package config

// ServeConfig configures the `serve` subcommand: the HTTP API, the image stores
// gantry can move images between, and the warm worker pool.
type ServeConfig struct {
	Addr          string     `yaml:"addr"`
	ShutdownGrace Duration   `yaml:"shutdown_grace"`
	Auth          AuthConfig `yaml:"auth"`
	// AllowUnknownStores permits a job to reference a registry by a bare host
	// that is not a declared store. Engine stores (docker/containerd) must always
	// be declared. Default false: only declared stores may be used.
	AllowUnknownStores bool          `yaml:"allow_unknown_stores"`
	Stores             []StoreConfig `yaml:"stores"`
	Warm               WarmConfig    `yaml:"warm"`
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

// StoreConfig describes one image store. kind "registry" is a container registry
// gantry reads from and writes to; kind "docker"/"containerd" is a daemon gantry
// triggers to pull. Fields not relevant to a kind are ignored.
type StoreConfig struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"` // registry | docker | containerd

	// --- registry ---
	// Host is the registry host, exposed to rewrite templates as {{.CacheHost}}.
	Host string `yaml:"host"`
	// Insecure allows plain-HTTP or self-signed registries.
	Insecure bool   `yaml:"insecure"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Mode selects how gantry fills this registry when it is a copy destination:
	// "copy" (default) pushes blobs, "proxy" reads through to self-fill.
	Mode string `yaml:"mode"`
	// Rewrite maps a source reference to its reference in this registry (used when
	// this store is a copy destination). Ordered; first match wins.
	Rewrite []RewriteRule `yaml:"rewrite"`
	// DownstreamHost overrides the host engine stores are told to pull from when
	// pulling out of this registry (e.g. push to an IP, have daemons pull a name).
	DownstreamHost string `yaml:"downstream_host"`

	// --- engine (docker / containerd) ---
	Address string `yaml:"address"`
	// Namespace is the containerd namespace (e.g. "k8s.io" for k3s, "moby" for docker's).
	Namespace string `yaml:"namespace"`
	// PullHost overrides the registry host this engine is told to pull from,
	// taking precedence over the source registry's downstream_host.
	PullHost string `yaml:"pull_host"`
}

// IsRegistry reports whether the store is a container registry.
func (s StoreConfig) IsRegistry() bool { return s.Kind == "registry" }

// IsEngine reports whether the store is a daemon gantry triggers to pull.
func (s StoreConfig) IsEngine() bool { return s.Kind == "docker" || s.Kind == "containerd" }

// WarmConfig bounds the warm worker pool.
type WarmConfig struct {
	// Platforms is the fallback platform set when a request omits it; empty
	// means the host GOOS/GOARCH only.
	Platforms []string `yaml:"platforms"`
	// MaxConcurrentJobs caps how many jobs run at once (tier-1 worker pool).
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	// MaxConcurrentLayers caps how many layers one transfer moves at once (tier-2).
	MaxConcurrentLayers int `yaml:"max_concurrent_layers"`
	// QueueSize is the buffered depth of the pending-job channel.
	QueueSize int `yaml:"queue_size"`
	// JobTTL is how long a finished job record is retained.
	JobTTL Duration `yaml:"job_ttl"`
	// DistributeByDefault fans out to all engine stores when a job omits the
	// distribute list.
	DistributeByDefault bool `yaml:"distribute_by_default"`
}
