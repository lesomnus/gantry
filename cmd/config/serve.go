package config

import (
	"slices"
	"strconv"
	"strings"
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
	AllowUnknownStores bool          `yaml:"allow_unknown_stores"`
	Health             HealthConfig  `yaml:"health"`
	Verify             VerifyConfig  `yaml:"verify"`
	Events             EventsConfig  `yaml:"events"`
	Enforce            EnforceConfig `yaml:"enforce"`
}

// EnforceConfig governs runtime signature enforcement ("quarantine"): gantry
// watches engine container-start events and force-removes a container — and its
// image — whose image is not signed by a trusted Root CA. This is post-hoc
// quarantine (the container is already running when the start event fires), not
// admission control. Disabled unless Mode is "quarantine". Enforcement reuses the
// same trust store as serve.verify and consults, in order: the verdict cache,
// the local signature layout (serve.verify.local_layout), then a live registry.
type EnforceConfig struct {
	// Mode is the enforcement level: off (default) or quarantine.
	Mode string `yaml:"mode"`
	// Stores names the engine stores (kind docker/containerd) to police. Empty
	// with mode quarantine is a config error.
	Stores []string `yaml:"stores"`
	// OnUnavailable is the fallback when no verdict can be obtained live and no
	// usable cached verdict exists: grace (default — honor an expired-but-known
	// trusted verdict, else allow-and-log), kill (fail closed), or allow.
	OnUnavailable string `yaml:"on_unavailable"`
	// SelfContainer is gantry's own container id or name so enforcement never
	// removes the container gantry runs in. Empty falls back to the hostname and
	// then /proc/self/cgroup. May also be provided via the GANTRY_SELF_ID env.
	SelfContainer string `yaml:"self_container"`
}

// Enabled reports whether runtime enforcement is turned on.
func (c EnforceConfig) Enabled() bool { return c.Mode == "quarantine" }

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
	// LocalLayout is a directory holding a local OCI image layout of pre-signed
	// bootstrap images (subject manifests + signature referrers). It is consulted
	// as an offline signature source before the live registry and verified
	// against the same trust store, so it is unspoofable by naming. Empty
	// disables it. See LocalLayoutEnabled.
	LocalLayout string `yaml:"local_layout"`
	// Cache configures the durable verdict cache (see VerifyCacheConfig). An empty
	// cache.path disables it. Required when serve.enforce is on.
	Cache VerifyCacheConfig `yaml:"cache"`
}

// LocalLayoutEnabled reports whether an offline local-layout signature source is
// configured.
func (c VerifyConfig) LocalLayoutEnabled() bool { return c.LocalLayout != "" }

// VerifyCacheConfig configures the durable verification-result cache: a bbolt
// store keyed by content digest so a verified image's trust decision survives a
// registry outage. It is shared by admission verification (which writes verdicts
// as a side effect) and runtime enforcement (which reads them offline). An empty
// Path disables it.
type VerifyCacheConfig struct {
	// Path is the bbolt file. Empty disables the cache. Must be distinct from
	// every retention.path and the events.path (bbolt takes an exclusive lock).
	Path string `yaml:"path"`
	// TTL is the hard max-age of a cached verdict: past it the verdict is unusable
	// and the image must be re-verified. Default 4w.
	TTL Duration `yaml:"ttl"`
	// Refresh is the soft revalidation age: past it a background sweeper
	// re-verifies the entry against its source, but the verdict stays usable until
	// TTL. Must be <= TTL. Default 2w.
	Refresh Duration `yaml:"refresh"`
}

// Enabled reports whether the verdict cache is configured.
func (c VerifyCacheConfig) Enabled() bool { return c.Path != "" }

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

// NeedVerifier reports whether the signature verifier must be built at all:
// admission verification is enabled anywhere, or runtime enforcement is on.
// Enforcement needs the verifier (and its trust store) to classify images even
// when the admission verify mode is off, so it cannot key off VerifyEnabled.
func (c Config) NeedVerifier() bool {
	return c.VerifyEnabled() || c.Serve.Enforce.Enabled()
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
	// Host is the registry host. When this store is a copy destination, the
	// in-store reference is the source repo/tag under this host.
	Host string `yaml:"host"`
	// Insecure allows plain-HTTP or self-signed registries.
	Insecure bool   `yaml:"insecure"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// TokenFile is a file holding a bearer token, sent as `Authorization:
	// Bearer` instead of username/password. It is **re-read when it changes**,
	// which is the whole reason it is a file rather than a value: a token that
	// expires is replaced on disk by whatever mints it, and gantry picks the new
	// one up without a restart.
	//
	// It is not the token a registry hands out after a `WWW-Authenticate:
	// Bearer` challenge — that exchange is ggcr's and happens on its own, from
	// whatever credential is configured here. This is a credential put in place
	// out of band, for a registry that has no such endpoint.
	//
	// Mutually exclusive with username/password.
	TokenFile string `yaml:"token_file"`
	// Mode selects how gantry fills this registry when it is a copy destination:
	// "copy" (default) pushes blobs, "proxy" reads through to self-fill.
	Mode string `yaml:"mode"`
	// DownstreamHost overrides the host engine stores are told to pull from when
	// pulling out of this registry (e.g. push to an IP, have daemons pull a name).
	DownstreamHost string `yaml:"downstream_host"`
	// Verify overrides the global serve.verify.mode for images pulled from this
	// source registry (nil = inherit the global default).
	Verify *StoreVerify `yaml:"verify"`
	// Cache names another declared registry that holds copies of this store's
	// content — a site registry standing in front of a cloud one. gantry may then
	// satisfy a job that reads from this store by going through that one instead,
	// filling it first if it does not already hold the image, so the content is
	// fetched from here once rather than once per destination. It is a routing
	// decision gantry makes for its own cost efficiency: the caller neither names
	// the cache nor sees a different result. Empty (the default) means jobs read
	// from this store directly. See docs/stores.md.
	Cache string `yaml:"cache"`
	// Caches is the scoped form of Cache: an ordered list of routes, the first
	// matching one of which is used. A route with no scope matches every job, so
	// `cache: site` is exactly `caches: [{store: site}]` — Evaluate normalizes the
	// shorthand into this field, and everything downstream reads only this one.
	// Setting both is an error rather than a merge.
	//
	// Scoping is what expresses a topology one cache per source cannot: a per-rack
	// cache is several routes on the SAME origin, each scoped to the targets it
	// serves, rather than a chain (routing is one level deep — see docs/stores.md).
	Caches []CacheRoute `yaml:"caches"`

	// Retention configures per-repo image GC for this store (engine stores only).
	// nil disables GC for the store. See StoreRetention.
	Retention *StoreRetention `yaml:"retention"`

	// Cred is the client-mTLS credential gantry presents to this store; nil
	// means no client certificate. For an oci registry it applies to every
	// outbound direction (pull, push, referrer copy) including the bearer-token
	// endpoint; for a docker engine it is the client certificate presented to
	// the daemon's TLS port (tcp mTLS).
	Cred *CredConfig `yaml:"cred"`

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

// CredConfig is a store's client-mTLS credential — how gantry proves its
// identity to the store. kind selects where the private key lives; cert is
// common to every kind. Unlike a store, a field of another kind is rejected
// rather than ignored: a stray key or handle usually means the wrong kind was
// picked, and this is security configuration.
type CredConfig struct {
	Kind string `yaml:"kind"` // tpm | file

	// Cert is the client certificate (leaf + chain), PEM. Its public key must
	// match the private key the kind selects, checked at transport build time.
	Cert string `yaml:"cert"`

	// --- kind "tpm": the key is sealed in a TPM and never leaves it ---
	Device string `yaml:"device"` // TPM device path (default /dev/tpmrm0)
	Handle string `yaml:"handle"` // persistent handle, hex e.g. "0x81000001"

	// --- kind "file": an ordinary PEM key pair on disk ---
	Key string `yaml:"key"` // private key, PEM (PKCS#8, SEC1 EC, or PKCS#1 RSA)
}

// IsTPM reports whether the credential signs with a TPM-sealed key. Safe on a
// nil receiver (no credential).
func (c *CredConfig) IsTPM() bool { return c != nil && c.Kind == "tpm" }

// HandleValue parses the persistent handle. It accepts hex ("0x81000001") or
// decimal; the value must fit in 32 bits, as a TPM handle is a uint32.
func (c CredConfig) HandleValue() (uint32, error) {
	v, err := strconv.ParseUint(c.Handle, 0, 32)
	if err != nil {
		return 0, z.Err(err, "cred.handle %q is not a valid 32-bit integer (use hex, e.g. 0x81000001)", c.Handle)
	}
	return uint32(v), nil
}

// validateCred checks the cred block is internally consistent for its kind.
// ca_cert is intentionally separate: it configures server verification and is
// valid on its own (with or without a credential). The device, certificate,
// and key files are validated lazily on first use, so a missing TPM or key
// file does not block startup for stores that do not use it.
func (s StoreConfig) validateCred() error {
	c := s.Cred
	if c == nil {
		return nil // no client-certificate auth configured
	}
	if s.Insecure {
		// mTLS requires TLS, but insecure enables plain HTTP on the oras path
		// (PlainHTTP) — the two are mutually exclusive and would silently drop the
		// client certificate. To trust a self-signed mTLS server, set ca_cert.
		return z.Err(nil, "insecure cannot be combined with cred (client mTLS); use ca_cert to trust a self-signed server")
	}
	if c.Cert == "" {
		return z.Err(nil, "cred.cert is required")
	}
	switch c.Kind {
	case "tpm":
		if c.Key != "" {
			return z.Err(nil, `cred.key is a kind "file" field; cred.kind is "tpm"`)
		}
		if c.Handle == "" {
			return z.Err(nil, `cred.handle is required for kind "tpm"`)
		}
		if _, err := c.HandleValue(); err != nil {
			return err
		}
	case "file":
		if c.Handle != "" || c.Device != "" {
			return z.Err(nil, `cred.handle and cred.device are kind "tpm" fields; cred.kind is "file"`)
		}
		if c.Key == "" {
			return z.Err(nil, `cred.key is required for kind "file"`)
		}
	default:
		return z.Err(nil, `cred.kind %q is not one of "tpm" or "file"`, c.Kind)
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
	// FallbackToOrigin is the default for a job that does not set
	// fallback_to_origin: an engine pull its source could not serve is
	// re-attempted against the registry named in the job's ref, so a cache is an
	// optimization rather than a dependency. Default false — the behavior of a
	// deployment that has not opted in does not change.
	FallbackToOrigin bool `yaml:"fallback_to_origin"`
	// SourceWait is how long a move that its source could not serve waits for an
	// active job that is filling that source with exactly this image, before
	// giving up on it. It costs nothing when the source can serve the image — the
	// wait happens only after a real miss — so it does not slow a warm source
	// down.
	//
	// It is what collapses a burst onto one origin read: N destinations submitted
	// together for a cold image all see the same fill in flight, and waiting for
	// it is the difference between one transfer out of the origin and N. Default
	// 30s; set it explicitly to 0 to disable waiting entirely.
	//
	// Waiters are capped at MaxConcurrentJobs-1 so a pool of two or more always
	// keeps a worker free to run the fills. With MaxConcurrentJobs 1 the pipeline
	// is serial and nothing can be filling anything while the move runs, so a wait
	// could only spend its bound: the default is forced to 0 there.
	SourceWait *Duration `yaml:"source_wait"`
	// RequireAuthority is the default for a job that does not set
	// require_authority: refuse a job whose authority — the store the caller named
	// as its source — could not confirm what its tag means, instead of serving
	// whatever a nearer cache of that store happens to hold. Default false: the
	// cache keeps working while the registry behind it does not, which is usually
	// the point of having it. Only ever consulted for a job gantry routed; for an
	// unrouted job the source the caller named IS the authority.
	RequireAuthority bool `yaml:"require_authority"`
	// AdmissionTimeout bounds the registry requests admission makes before a job is
	// created — settling a tag at its authority and probing a cache for the digest.
	// It exists so an unresponsive registry delays one submit rather than holding it
	// open indefinitely; on expiry the job is planned as if the store had not
	// answered. Default 10s.
	AdmissionTimeout Duration `yaml:"admission_timeout"`
}

// SourceWaitOr is how long a move waits for an in-flight fill of its source.
// Reading it through a method keeps the nil (un-Evaluated) case from silently
// meaning "wait forever" or panicking in a caller that builds a WorkerConfig by
// hand; Evaluate always sets it.
func (c WorkerConfig) SourceWaitOr() time.Duration {
	if c.SourceWait == nil {
		return 0
	}
	return time.Duration(*c.SourceWait)
}

// CacheRoute is one route gantry may take when reading from the store that
// declares it: a nearer registry holding copies of that store's content, and the
// jobs it applies to. It is gantry's own cost optimization — the caller neither
// names the route nor sees a different result — so a scope that excludes a job
// means only that the job reads its source directly.
type CacheRoute struct {
	// Store is the declared registry store to read through. Required.
	Store string `yaml:"store"`
	// ForTargets limits the route to jobs delivering to one of these stores.
	// Empty matches every target. This is how a per-rack cache is expressed: one
	// route per rack on the same origin, each naming that rack's nodes.
	ForTargets []string `yaml:"for_targets"`
	// ForRepos limits the route to repositories matching one of these doublestar
	// patterns, e.g. "team/**". Empty matches every repository. The pattern is
	// matched against the repository PATH alone ("team/app") — the host is the
	// declaring store's own, so including it could never match.
	ForRepos []string `yaml:"for_repos"`
}

// matches reports whether this route applies to a job delivering repo to target.
func (r CacheRoute) matches(target, repo string) bool {
	if len(r.ForTargets) > 0 && !slices.Contains(r.ForTargets, target) {
		return false
	}
	if len(r.ForRepos) == 0 {
		return true
	}
	for _, p := range r.ForRepos {
		if ok, err := doublestar.Match(p, repo); err == nil && ok {
			return true
		}
	}
	return false
}

// CacheFor is the store a job delivering repo to target should be read through,
// or "" when this store declares no route that applies. First match wins, so an
// unscoped route placed last reads as the default and one placed first shadows
// everything after it — which the loader rejects rather than let it look like a
// working config.
func (c StoreConfig) CacheFor(target, repo string) string {
	for _, r := range c.Caches {
		if r.matches(target, repo) {
			return r.Store
		}
	}
	return ""
}

// RouteAliases expands a retention/enforcement repository pattern across the
// caches its registry declares.
//
// Retention rules are doublestar patterns over HOST-QUALIFIED repositories, and a
// routed job deliberately lands the image under the cache's host — so a rule an
// operator wrote for the origin does not match what the node actually holds, and
// the image is left `unmanaged` and never collected. Rather than make the
// operator restate every rule per cache (and remember to, whenever a route is
// added), the pattern is expanded here: `cr.example.com/team/**` also covers
// `registry.corp/team/**` for as long as `cr.example.com` declares
// `registry.corp` as a cache.
//
// target is the store the rules belong to, so a route scoped to other targets is
// not expanded into them. Only a pattern whose first segment is a declared
// registry's literal host expands; anything else (a glob host, a bare repository
// pattern like `**`) already matches host-agnostically or was never about a host.
// The returned slice excludes the input.
func (c Config) RouteAliases(target, pattern string) []string {
	host, rest, ok := strings.Cut(pattern, "/")
	if !ok || host == "" {
		return nil
	}
	var src StoreConfig
	for _, s := range c.Stores {
		if s.IsRegistry() && s.Host == host {
			src = s
			break
		}
	}
	if len(src.Caches) == 0 {
		return nil
	}
	var out []string
	for _, r := range src.Caches {
		if len(r.ForTargets) > 0 && !slices.Contains(r.ForTargets, target) {
			continue // this route can never deliver to the store these rules govern
		}
		cache, ok := c.Stores[r.Store]
		if !ok || cache.Host == "" || cache.Host == host {
			continue
		}
		alias := cache.Host + "/" + rest
		if !slices.Contains(out, alias) {
			out = append(out, alias)
		}
	}
	return out
}
