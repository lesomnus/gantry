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
	AllowUnknownStores bool `yaml:"allow_unknown_stores"`
	// Stores are keyed by name; order is not significant.
	Stores    map[string]StoreConfig `yaml:"stores"`
	Warm      WarmConfig             `yaml:"warm"`
	Retention RetentionConfig        `yaml:"retention"`
	Health    HealthConfig           `yaml:"health"`
	Verify    VerifyConfig           `yaml:"verify"`
	Events    EventsConfig           `yaml:"events"`
}

// EventsConfig governs the audit log (GET /v1/event). An empty Path disables it.
// It is independent of retention so the log works even when GC is off.
type EventsConfig struct {
	// Path is the bbolt file for the audit ring. Empty disables the log.
	Path string `yaml:"path"`
	// Cap is the maximum number of entries retained (oldest evicted). Default 10000.
	Cap int `yaml:"cap"`
}

// Enabled reports whether the audit log is configured.
func (c EventsConfig) Enabled() bool { return c.Path != "" }

// VerifyMode selects how source-image signatures are checked at job admission.
type VerifyMode string

const (
	VerifyOff       VerifyMode = "off"               // no verification
	VerifyIfPresent VerifyMode = "verify-if-present" // verify when a signature exists; allow unsigned
	VerifyRequire   VerifyMode = "require"           // signature mandatory; reject unsigned & invalid
)

// Valid reports whether m is a recognized mode (empty is treated as off).
func (m VerifyMode) Valid() bool {
	switch m {
	case "", VerifyOff, VerifyIfPresent, VerifyRequire:
		return true
	default:
		return false
	}
}

// VerifyConfig governs source-image signature verification (Notary Project /
// notation). Verification runs synchronously at job creation; failure rejects
// the job. Disabled unless Mode is verify-if-present or require.
type VerifyConfig struct {
	// Mode is the global default enforcement level: off | verify-if-present | require.
	Mode VerifyMode `yaml:"mode"`
	// Provider selects the signature scheme; only "notation" is supported (default).
	Provider string `yaml:"provider"`
	// TrustStore is a directory of trusted CA certificate files (PEM: *.crt/*.pem/*.cert).
	// Required when verification is enabled; there is no OS/system-root fallback.
	TrustStore string `yaml:"trust_store"`
	// TrustPolicy is an optional notation trust policy JSON. When empty, gantry
	// synthesizes a policy that trusts any signature chaining to TrustStore
	// (registryScopes ["*"], trustedIdentities ["*"], the configured Level).
	TrustPolicy string `yaml:"trust_policy"`
	// Level is the synthesized policy's verification level: strict (default) |
	// permissive | audit. Ignored when TrustPolicy is set.
	Level string `yaml:"level"`
	// Timeout bounds a single verification (registry resolve + signature fetch +
	// verify). Default 15s.
	Timeout Duration `yaml:"timeout"`
}

// Enabled reports whether the global default turns verification on.
func (c VerifyConfig) Enabled() bool {
	return c.Mode != "" && c.Mode != VerifyOff
}

// VerifyEnabled reports whether verification runs anywhere: the global default
// is on, or any store overrides its mode to a non-off value. A per-store
// override must build the verifier even when the global default is off.
func (c ServeConfig) VerifyEnabled() bool {
	if c.Verify.Enabled() {
		return true
	}
	for _, s := range c.Stores {
		if s.Verify != nil && s.Verify.Mode != "" && s.Verify.Mode != VerifyOff {
			return true
		}
	}
	return false
}

// EffectiveMode resolves the enforcement mode for a source store: a non-empty
// per-store override wins, else the global default.
func (c VerifyConfig) EffectiveMode(store StoreConfig) VerifyMode {
	if store.Verify != nil && store.Verify.Mode != "" {
		return store.Verify.Mode
	}
	if c.Mode == "" {
		return VerifyOff
	}
	return c.Mode
}

// StoreVerify overrides the global verify mode for one source registry store.
type StoreVerify struct {
	Mode VerifyMode `yaml:"mode"`
}

// HealthConfig governs the cached per-store health probe (GET /v1/store/{name}/health).
type HealthConfig struct {
	// CacheTTL is how long a store's probe result is cached before the next call
	// re-probes. Default 5s.
	CacheTTL Duration `yaml:"cache_ttl"`
	// ProbeTimeout bounds a single store probe (engine ready-check or registry
	// /v2/ ping). Default 3s.
	ProbeTimeout Duration `yaml:"probe_timeout"`
	// ReadyStores are the store names GET /readyz gates on. Empty means every
	// engine store — a remote upstream registry must not flap node readiness,
	// so registries join the gate only by being listed here.
	ReadyStores []string `yaml:"ready_stores"`
}

// RetentionConfig governs image GC on engine stores. An empty Path disables
// retention entirely (no usage watcher, no GC capability).
type RetentionConfig struct {
	// Path is the bbolt file for the last-used / pin index. Empty disables GC.
	Path string `yaml:"path"`
	// MaxAge deletes an image whose last-used age exceeds it; zero disables age GC.
	MaxAge Duration `yaml:"max_age"`
	// KeepN keeps the N most-recently-used tags per repository (even if old);
	// zero disables keep-N.
	KeepN int `yaml:"keep_n"`
	// Pins are never GC'd: exact references, or doublestar patterns matched
	// against the full ref, its name:tag short form, and the bare tag.
	Pins []string `yaml:"pins"`
	// Interval is the scheduler's safety/idle cadence — the longest it waits
	// between GC checks. The scheduler wakes earlier when a record is about to
	// age out or a usage event arrives.
	Interval Duration `yaml:"interval"`
	// MinInterval rate-limits GC runs (debounce for event bursts).
	MinInterval Duration `yaml:"min_interval"`
	// Grace holds off age-based deletion for this long after startup, since the
	// usage index has no history for the downtime. Defaults to MaxAge.
	Grace Duration `yaml:"grace"`
}

// Enabled reports whether retention/GC is configured.
func (c RetentionConfig) Enabled() bool { return c.Path != "" }

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

// StoreConfig describes one image store. kind "oci" is an OCI distribution
// registry gantry reads from and writes to; kind "docker"/"containerd" is a
// daemon gantry triggers to pull. Fields not relevant to a kind are ignored.
type StoreConfig struct {
	Name string `yaml:"-"`    // set from the stores map key
	Kind string `yaml:"kind"` // oci | docker | containerd

	// --- oci registry ---
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
	// Verify overrides the global serve.verify.mode for images pulled from this
	// source registry (nil = inherit the global default).
	Verify *StoreVerify `yaml:"verify"`

	// --- engine (docker / containerd) ---
	Address string `yaml:"address"`
	// Namespace is the containerd namespace (e.g. "k8s.io" for k3s, "moby" for docker's).
	Namespace string `yaml:"namespace"`
	// PullHost overrides the registry host this engine is told to pull from,
	// taking precedence over the source registry's downstream_host.
	PullHost string `yaml:"pull_host"`
}

// IsRegistry reports whether the store is an OCI distribution registry.
func (s StoreConfig) IsRegistry() bool { return s.Kind == "oci" }

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
