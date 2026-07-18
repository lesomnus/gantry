package config

import (
	"strconv"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lesomnus/z"
)

// ServeConfig configures the `serve` subcommand's gRPC API. The image stores
// and the worker pool are top-level config sections (Config.Stores, Config.Worker).
type ServeConfig struct {
	Addr          string     `yaml:"addr"`
	ShutdownGrace Duration   `yaml:"shutdown_grace"`
	Auth          AuthConfig `yaml:"auth"`
	// AllowUnknownStores permits a job to reference a registry by a bare host
	// that is not a declared store. Engine stores (docker/containerd) must always
	// be declared. Default false: only declared stores may be used.
	AllowUnknownStores bool         `yaml:"allow_unknown_stores"`
	Health             HealthConfig `yaml:"health"`
	Verify             VerifyConfig `yaml:"verify"`
	Events             EventsConfig `yaml:"events"`
}

// EventsConfig governs the audit log (EventService). An empty Path disables it.
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
	// Level is the synthesized policy's verification level: strict (default) or
	// permissive. "audit" is rejected — it disables trust-anchor enforcement,
	// defeating the gate. Ignored when TrustPolicy is set.
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
func (c Config) VerifyEnabled() bool {
	if c.Serve.Verify.Enabled() {
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

// HealthConfig governs the cached per-store health probe (StoreService.Health).
type HealthConfig struct {
	// CacheTTL is how long a store's probe result is cached before the next call
	// re-probes. Default 5s.
	CacheTTL Duration `yaml:"cache_ttl"`
	// ProbeTimeout bounds a single store probe (engine ready-check or registry
	// /v2/ ping). Default 3s.
	ProbeTimeout Duration `yaml:"probe_timeout"`
	// ReadyStores are the store names the gRPC health readiness gates on. Empty means every
	// engine store — a remote upstream registry must not flap node readiness,
	// so registries join the gate only by being listed here.
	ReadyStores []string `yaml:"ready_stores"`
}

// StoreRetention configures image GC for one engine store. It is set per-store
// (stores.<name>.retention); there is no global retention policy. Each store has
// its own usage index, scheduler cadence, grace window, and per-repo rules.
type StoreRetention struct {
	// Path is the bbolt file for this store's last-used / pin index. Required.
	Path string `yaml:"path"`
	// Interval is the scheduler's safety/idle cadence — the longest it waits
	// between GC checks. It wakes earlier when a record is about to age out or a
	// usage event arrives. Default 1h.
	Interval Duration `yaml:"interval"`
	// MinInterval rate-limits GC runs (debounce for event bursts). Default 1m.
	MinInterval Duration `yaml:"min_interval"`
	// Grace holds off deletion this long after startup, since the usage index has
	// no history for the downtime. Default 1h.
	Grace Duration `yaml:"grace"`
	// Heartbeat periodically stamps LastUsed=now for images a live container
	// references, covering containers whose start event the usage watcher missed
	// (so the image survives age GC after the container later stops). Cheap: one
	// container list per tick. Default 5m; a pointer so "0s" (off) differs from
	// unset (default on).
	Heartbeat *Duration `yaml:"heartbeat"`
	// UntaggedAfter reaps an image this long after gantry first observes it with
	// no tags — e.g. the previous image of a tag that was re-pulled. Untagged
	// images bypass the per-repo rules (there is no tag for a rule to manage);
	// running containers, digest-pinned records, and repo@digest pins still
	// protect them. docker stores only: containerd reclaims untagged content by
	// itself. A pointer so an explicit "0s" (reaper off) differs from unset
	// (default 1h on docker stores).
	UntaggedAfter *Duration `yaml:"untagged_after"`
	// Rules are per-repo retention policies. For a given repo, the matching rules
	// (doublestar patterns) are resolved field-by-field: each field takes the
	// value from the most specific matching rule that sets it (longest literal
	// prefix wins); pins are the union of all matching rules. A repo that matches
	// no rule is left unmanaged (never GC'd). Order is not significant.
	Rules []RetentionRule `yaml:"rules"`
}

// RetentionRule is one per-repo retention policy. Scalar fields are pointers so
// an unset field (inherit from a less specific rule) is distinct from an explicit
// zero (disable that dimension: max_age 0 = no age GC, max_n 0 = no cap).
type RetentionRule struct {
	// Repo is a doublestar pattern matched against the repository name (no tag),
	// e.g. "registry.internal/prod/**" or "**".
	Repo string `yaml:"repo"`
	// MaxAge deletes an image whose last-used age exceeds it.
	MaxAge *Duration `yaml:"max_age"`
	// KeepN keeps the N most-recently-used tags in the repo, even if old.
	KeepN *int `yaml:"keep_n"`
	// MaxN caps the tags kept: the oldest beyond the cap are deleted even before
	// max_age. When both are set MaxN must be >= KeepN.
	MaxN *int `yaml:"max_n"`
	// MaxIdle is a hard idle cap: an image unused longer than this is deleted
	// regardless of keep_n/max_n (only in-use and pins protect it). Zero disables.
	MaxIdle *Duration `yaml:"max_idle"`
	// Pins are never GC'd within a matching repo: exact refs or doublestar
	// patterns matched against the full ref, its name:tag short form, and the tag.
	Pins []string `yaml:"pins"`
}

// Enabled reports whether retention/GC is configured for this store.
func (c *StoreRetention) Enabled() bool { return c != nil && c.Path != "" }

// UntaggedReapAfter is the effective untagged-image reap delay after defaults
// are applied; zero means the reaper is off.
func (c *StoreRetention) UntaggedReapAfter() time.Duration {
	if c == nil || c.UntaggedAfter == nil {
		return 0
	}
	return time.Duration(*c.UntaggedAfter)
}

// HeartbeatInterval is the effective in-use heartbeat cadence after defaults;
// zero (an explicit "0s") disables it.
func (c *StoreRetention) HeartbeatInterval() time.Duration {
	if c == nil || c.Heartbeat == nil {
		return 0
	}
	return time.Duration(*c.Heartbeat)
}

// evaluateRetention applies defaults and validates the store's retention config.
// Retention is only supported on engine stores.
func (s *StoreConfig) evaluateRetention() error {
	r := s.Retention
	if r == nil {
		return nil
	}
	if !s.IsEngine() {
		return z.Err(nil, "retention is only supported on engine stores (docker/containerd), not kind %q", s.Kind)
	}
	if r.Path == "" {
		return z.Err(nil, "retention.path is required")
	}
	z.FallbackP((*time.Duration)(&r.Interval), time.Hour)
	z.FallbackP((*time.Duration)(&r.MinInterval), time.Minute)
	z.FallbackP((*time.Duration)(&r.Grace), time.Hour)
	if r.Heartbeat == nil {
		d := Duration(5 * time.Minute)
		r.Heartbeat = &d
	}
	switch s.Kind {
	case "docker":
		if r.UntaggedAfter == nil {
			d := Duration(time.Hour) // default-on: gantry is the node's image manager
			r.UntaggedAfter = &d
		}
		if *r.UntaggedAfter < 0 {
			return z.Err(nil, "retention.untagged_after must not be negative")
		}
	default:
		if r.UntaggedAfter != nil {
			return z.Err(nil, "retention.untagged_after is only supported on docker stores; containerd reclaims untagged content itself")
		}
	}
	for i := range r.Rules {
		rule := &r.Rules[i]
		if rule.Repo == "" {
			return z.Err(nil, "retention.rules[%d]: repo pattern is required", i)
		}
		if !doublestar.ValidatePattern(rule.Repo) {
			return z.Err(nil, "retention.rules[%d]: invalid repo pattern %q", i, rule.Repo)
		}
		for j, pin := range rule.Pins {
			if !doublestar.ValidatePattern(pin) {
				return z.Err(nil, "retention.rules[%d].pins[%d]: invalid pattern %q", i, j, pin)
			}
		}
		if rule.KeepN != nil && *rule.KeepN < 0 {
			return z.Err(nil, "retention.rules[%d]: keep_n must not be negative", i)
		}
		if rule.MaxN != nil && *rule.MaxN < 0 {
			return z.Err(nil, "retention.rules[%d]: max_n must not be negative", i)
		}
		if rule.MaxN != nil && *rule.MaxN > 0 && rule.KeepN != nil && *rule.KeepN > *rule.MaxN {
			return z.Err(nil, "retention.rules[%d]: max_n (%d) must be >= keep_n (%d)", i, *rule.MaxN, *rule.KeepN)
		}
	}
	return nil
}

// AuthConfig guards the API (the health and reflection services are always exempt).
type AuthConfig struct {
	// Tokens is a bearer-token whitelist; if non-empty, every RPC requires one.
	Tokens []string `yaml:"tokens"`
	// TLSCert/TLSKey serve the API over TLS; empty means plaintext (TLS terminated upstream).
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

	// Retention configures per-repo image GC for this store (engine stores only).
	// nil disables GC for the store. See StoreRetention.
	Retention *StoreRetention `yaml:"retention"`

	// --- mTLS client auth via a TPM-backed key ---
	// When TPMHandle is set, gantry authenticates to this store with a client
	// certificate whose private key never leaves the TPM. The key is addressed by
	// its persistent handle; the leaf (+ chain) is read from TPMCert and its
	// public key must match the key at the handle. For an oci registry this
	// applies to every outbound direction (pull, push, referrer copy) including
	// the bearer-token endpoint; for a docker engine it is the client certificate
	// presented to the daemon's TLS port (tcp mTLS).
	TPMDevice string `yaml:"tpm"`        // TPM device path (default /dev/tpmrm0)
	TPMHandle string `yaml:"tpm_handle"` // persistent handle, hex e.g. "0x81000001"
	TPMCert   string `yaml:"tpm_cert"`   // client certificate (leaf + chain), PEM
	// CACert verifies the registry/token server. Empty uses the system roots (or
	// is skipped when insecure is set).
	CACert string `yaml:"ca_cert"`

	// --- engine (docker / containerd) ---
	Address string `yaml:"address"`
	// Namespace is the containerd namespace (e.g. "k8s.io" for k3s); defaults to
	// "default" when empty. containerd stores only — the docker engine ignores it.
	Namespace string `yaml:"namespace"`
	// PullHost overrides the registry host this engine is told to pull from,
	// taking precedence over the source registry's downstream_host.
	PullHost string `yaml:"pull_host"`
}

// IsRegistry reports whether the store is an OCI distribution registry.
func (s StoreConfig) IsRegistry() bool { return s.Kind == "oci" }

// IsEngine reports whether the store is a daemon gantry triggers to pull.
func (s StoreConfig) IsEngine() bool { return s.Kind == "docker" || s.Kind == "containerd" }

// HasTPM reports whether the store authenticates with a TPM-backed client
// certificate (mTLS).
func (s StoreConfig) HasTPM() bool { return s.TPMHandle != "" }

// TPMHandleValue parses the persistent handle. It accepts hex ("0x81000001") or
// decimal; the value must fit in 32 bits, as a TPM handle is a uint32.
func (s StoreConfig) TPMHandleValue() (uint32, error) {
	v, err := strconv.ParseUint(s.TPMHandle, 0, 32)
	if err != nil {
		return 0, z.Err(err, "tpm_handle %q is not a valid 32-bit integer (use hex, e.g. 0x81000001)", s.TPMHandle)
	}
	return uint32(v), nil
}

// validateTPM checks the TPM mTLS fields are internally consistent. ca_cert is
// intentionally excluded from the trigger: it configures server verification and
// is valid on its own (with or without a client certificate). The device and
// certificate files are validated lazily on first use, so a missing TPM does not
// block startup for stores that do not use it.
func (s StoreConfig) validateTPM() error {
	if s.TPMHandle == "" && s.TPMCert == "" && s.TPMDevice == "" {
		return nil // no TPM client-certificate auth configured
	}
	if s.TPMHandle == "" {
		return z.Err(nil, "tpm_handle is required when TPM mTLS is configured")
	}
	if s.TPMCert == "" {
		return z.Err(nil, "tpm_cert is required when TPM mTLS is configured")
	}
	if s.Insecure {
		// mTLS requires TLS, but insecure enables plain HTTP on the oras path
		// (PlainHTTP) — the two are mutually exclusive and would silently drop the
		// client certificate. To trust a self-signed mTLS server, set ca_cert.
		return z.Err(nil, "insecure cannot be combined with TPM mTLS; use ca_cert to trust a self-signed server")
	}
	if _, err := s.TPMHandleValue(); err != nil {
		return err
	}
	return nil
}

// WorkerConfig bounds the job worker pool that runs image moves.
type WorkerConfig struct {
	// MaxConcurrentJobs caps how many jobs run at once (tier-1 worker pool).
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	// MaxConcurrentLayers caps how many layers one transfer moves at once (tier-2).
	MaxConcurrentLayers int `yaml:"max_concurrent_layers"`
	// QueueSize is the buffered depth of the pending-job channel.
	QueueSize int `yaml:"queue_size"`
	// JobTTL is how long a finished job record is retained.
	JobTTL Duration `yaml:"job_ttl"`
}
