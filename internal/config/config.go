// Package config: the spindle configuration (11_config_cli.md — key names, defaults, and
// meanings are normative and byte-compatible with the Rust spec §15.1).
// CONTRACT FILE: the Config struct + duration/byte-size scalar types + compiled-in Defaults().
// Loading (TOML file, SPINDLE__/WALGIT__ env overlay, PORT lockstep, validation, per-repo
// settings merge, setup save) is implemented by the config owner in sibling files.
package config

import (
	"strings"
	"time"
)

// removedTLSHint explains the #165 removal wherever a residual server.tls.*
// setting is rejected (TOML file or env overlay): fail closed with a pointer
// at the reverse proxy, never a bare "unknown key" or a silent ignore.
const removedTLSHint = "server.tls.* was removed (issue #165): spindle serves plain HTTP (h2c); terminate TLS at the reverse proxy in front of it"

// Duration accepts Rust-spec spellings on the wire/TOML: "5ms", "1h", "30d", "7d", "0s".
// (time.ParseDuration has no day/week units; this type adds them.)
type Duration time.Duration

// ByteSize accepts "20GiB", "64MiB", "1GiB", "0B", "512MB" spellings.
type ByteSize int64

// --- Auth ---

// StaticToken is an operator-authored machine credential (server.auth.tokens):
// it authenticates git/API/CLI requests as Principal with the row's Write flag
// and can never administer — host administration is the OIDC admin group and
// nothing else (docs/PLAN-OIDC-ONLY.md §1.3).
type StaticToken struct {
	Principal string `toml:"principal"`
	Token     string `toml:"token"`     // literal secret (one of token/token_env required)
	TokenEnv  string `toml:"token_env"` // env var NAME read at startup; overrides token
	Write     bool   `toml:"write"`
	// Name is the optional stable label for this credential (06 §8.2.1): it
	// keys the last-used record and names the row on the Access page across
	// rotations. Without one a token is identified by a SHA-256 hash prefix —
	// never by the secret. Names must be unique across the token rows.
	Name string `toml:"name,omitempty"`
	// ExpiresAt is the optional RFC 3339 instant at which this credential
	// stops authenticating (e.g. "2026-12-31T23:59:59Z"). Validation refuses
	// an unparseable value; a PAST value is accepted and the token is
	// immediately expired (401). Empty = the credential never expires, which
	// the Access page warns about.
	ExpiresAt string `toml:"expires_at,omitempty"`
}

// Auth is the OIDC authentication configuration. There is no mode: "auth is
// configured" means this set is complete (see Validate/checkAuthConfigured).
type Auth struct {
	AnonymousRead bool          `toml:"anonymous_read"`
	Tokens        []StaticToken `toml:"tokens"`
	// AdminGroups lists the OIDC group values (from GroupsClaim) that grant
	// the global admin flag — and therefore the Administration area. It is
	// required and non-empty: administration exists only through this group.
	// Compared case-insensitively.
	AdminGroups []string `toml:"admin_groups"`
	// GroupsClaim names the ID-token claim carrying group membership. The
	// claim may be a JSON array of strings or a single string. Required (it
	// defaults to "groups", the conventional claim).
	GroupsClaim       string   `toml:"groups_claim"`
	Issuer            string   `toml:"issuer"`
	AllowedDomains    []string `toml:"allowed_domains"`
	AllowedEmails     []string `toml:"allowed_emails"`
	WriteDomains      []string `toml:"write_domains"`
	OAuthClientID     string   `toml:"oauth_client_id"`
	OAuthClientSecret string   `toml:"oauth_client_secret"`
	SessionSecret     string   `toml:"session_secret"`
	SessionTTL        Duration `toml:"session_ttl"`      // 30d
	AccessTokenTTL    Duration `toml:"access_token_ttl"` // 90d
	Audiences         []string `toml:"audiences"`
	TrustedForwarders []string `toml:"trusted_forwarders"`
}

// --- Server ---

type Server struct {
	Listen                string   `toml:"listen"` // default "127.0.0.1:8080"; first run "0.0.0.0:8080"
	HTTP2                 bool     `toml:"http2"`
	MaxConcurrentRequests int      `toml:"max_concurrent_requests"`
	MaxConcurrentPerRepo  int      `toml:"max_concurrent_per_repo"`
	RequestTimeout        Duration `toml:"request_timeout"`
	DrainTimeout          Duration `toml:"drain_timeout"`
	// ServeSyncTimeout bounds how long a serve-level Sync WAITS for pack
	// materialization (packMu acquisition + the (repo,materialize)
	// single-flight join/leader wait — issue #320). Exceeded → the Sync
	// fails with a timeout (HTTP 503 + degraded marker) while the
	// detached materialize body keeps warming the cache in the
	// background. Refs-level syncs never touch packMu and are
	// unaffected. Default 45s (under typical proxy 60s timeouts);
	// non-positive falls back to the default (there is no unbounded
	// serve wait).
	ServeSyncTimeout Duration `toml:"serve_sync_timeout"`
	// ServeMaterializeTimeout caps one materialize BODY (the detached
	// single-flight task all serve syncs join — issue #320). A hung
	// store GET cannot wedge the repo past this bound; the next serve
	// or heal attempt starts a fresh body and resumes at file
	// granularity. Default 10m; non-positive falls back to the
	// default. Must comfortably exceed ServeSyncTimeout (the wait bound
	// is per request; this bound is per body of work).
	ServeMaterializeTimeout Duration `toml:"serve_materialize_timeout"`
	MaxPushBytes            ByteSize `toml:"max_push_bytes"`
	MaxTreeLog              int      `toml:"max_tree_log"` // per-entry tree-date walk cap (§9.4; default 200)
	Roles                   []string `toml:"roles"`        // serve | maintain | events; empty = all
	AutoCreateOnPush        bool     `toml:"auto_create_on_push"`
	AccelRedirect           bool     `toml:"accel_redirect"`
	PublicURL               string   `toml:"public_url"`
	CorsOrigins             []string `toml:"cors_origins"`
	// SecretsKey is the dedicated at-rest/attempt-token key for Threads
	// (15_threads.md §5.3). When empty, server.auth.session_secret is used;
	// when both are empty, repository secrets are unavailable (503) and
	// attempt tokens are process-local.
	SecretsKey string    `toml:"secrets_key"`
	Auth       Auth      `toml:"auth"`
	SSH        ServerSSH `toml:"ssh"`
}

// --- Server SSH (17_ssh.md) ---

// ServerSSH is the SSH git transport listener: disabled unless Listen is set.
// User public keys are NOT config: authenticated users manage their own keys
// through the UI/API, stored in the object store (17_ssh.md §3). The TOML
// surface is the server only: listener + host key.
type ServerSSH struct {
	Listen       string `toml:"listen"`        // e.g. "0.0.0.0:2222"; empty = disabled
	ExternalPort int    `toml:"external_port"` // advertised in ssh:// clone URLs; 0 = the listen port (17_ssh.md §3)
	HostKey      string `toml:"host_key"`      // path to an OpenSSH/PEM private key
	HostKeyEnv   string `toml:"host_key_env"`  // env var NAME holding the private key; overrides host_key
}

// --- Store ---

type S3 struct {
	Endpoint       string `toml:"endpoint"`
	Region         string `toml:"region"`
	AccessKeyEnv   string `toml:"access_key_env"`
	SecretKeyEnv   string `toml:"secret_key_env"`
	ForcePathStyle bool   `toml:"force_path_style"`
}

type Store struct {
	Backend            string   `toml:"backend"` // "s3" | "memory" | "filesystem"
	Bucket             string   `toml:"bucket"`
	Prefix             string   `toml:"prefix"`
	Root               string   `toml:"root"` // filesystem backend root (D4)
	MaxRetries         int      `toml:"max_retries"`
	MultipartThreshold ByteSize `toml:"multipart_threshold"`
	MultipartPartSize  ByteSize `toml:"multipart_part_size"`
	S3                 S3       `toml:"s3"`
}

// --- Cache ---

type Cache struct {
	Dir                 string   `toml:"dir"`
	Mode                string   `toml:"mode"` // "budget" | "disk" | "auto"
	MaxBytes            ByteSize `toml:"max_bytes"`
	DiskHighWatermark   float64  `toml:"disk_high_watermark"`
	EvictIdleAfter      Duration `toml:"evict_idle_after"`
	Prewarm             []string `toml:"prewarm"`
	PrewarmParallelism  int      `toml:"prewarm_parallelism"`
	PrewarmReadyTimeout Duration `toml:"prewarm_ready_timeout"`
	RefAdvertEntries    int      `toml:"ref_advert_entries"`
	ObjectInfoEntries   int      `toml:"object_info_entries"`
	BundleListEntries   int      `toml:"bundle_list_entries"`
	RemoteBlockBytes    ByteSize `toml:"remote_block_bytes"`
	RemoteObjectBytes   ByteSize `toml:"remote_object_bytes"`
	SharedRenderCache   bool     `toml:"shared_render_cache"`
	StoreMount          string   `toml:"store_mount"`
}

// --- WAL ---

type WAL struct {
	BatchWindow           Duration `toml:"batch_window"`
	MaxBatch              int      `toml:"max_batch"`
	PushBrokerURL         string   `toml:"push_broker_url"`
	PushBrokerToken       string   `toml:"push_broker_token"`
	PushBrokerBufferBytes ByteSize `toml:"push_broker_buffer_bytes"`
	SnapshotEveryEntries  uint64   `toml:"snapshot_every_entries"`
	CheckpointInterval    Duration `toml:"checkpoint_interval"`
	CheckpointTailBytes   ByteSize `toml:"checkpoint_tail_bytes"`
	CASMaxRetries         uint32   `toml:"cas_max_retries"`
	FsckObjects           bool     `toml:"fsck_objects"`
	CheckConnectivity     bool     `toml:"check_connectivity"`
	FreshnessTTL          Duration `toml:"freshness_ttl"`
	PrefetchPacks         bool     `toml:"prefetch_packs"`
	PrefetchMaxBytes      ByteSize `toml:"prefetch_max_bytes"`
	RemoteObjects         bool     `toml:"remote_objects"`
}

// --- Maintenance / placement / compaction ---

type Maintenance struct {
	Interval       Duration `toml:"interval"`
	Checkpoints    bool     `toml:"checkpoints"`
	MaxPackBytes   ByteSize `toml:"max_pack_bytes"`
	Disk           string   `toml:"disk"` // "tmpfs" | "ssd"
	Host           string   `toml:"host"`
	FsckInterval   Duration `toml:"fsck_interval"`
	FollowInterval Duration `toml:"follow_interval"`
}

type Placement struct {
	Serve           []string `toml:"serve"`
	ServeExclude    []string `toml:"serve_exclude"`
	Maintain        []string `toml:"maintain"`
	MaintainExclude []string `toml:"maintain_exclude"`
}

type Compaction struct {
	Enabled             bool     `toml:"enabled"`
	Factor              int      `toml:"factor"`
	TriggerPacks        int      `toml:"trigger_packs"`
	TriggerBytes        ByteSize `toml:"trigger_bytes"`
	LeaseTTL            Duration `toml:"lease_ttl"`
	RetentionSuperseded Duration `toml:"retention_superseded"`
	Engine              string   `toml:"engine"` // "git"
}

// --- Bundles ---

type BundleStrategy struct {
	Name        string   `toml:"name"`
	Kind        string   `toml:"kind"` // "full" | "incremental"
	Base        string   `toml:"base,omitempty"`
	Schedule    string   `toml:"schedule"` // 6-field UTC cron
	Keep        int      `toml:"keep,omitempty"`
	BackfillMax int      `toml:"backfill_max"`
	Chain       bool     `toml:"chain,omitempty"`
	Filter      string   `toml:"filter,omitempty"`
	Refs        []string `toml:"refs,omitempty"`
	MinCommits  int      `toml:"min_commits,omitempty"`
}

type Bundles struct {
	Strategy          []BundleStrategy `toml:"strategy"`
	MinCommits        int              `toml:"min_commits"`
	MinBytes          ByteSize         `toml:"min_bytes"`
	MainOnly          bool             `toml:"main_only"`
	ExtraRefs         []string         `toml:"extra_refs"`
	ServeVia          string           `toml:"serve_via"` // "proxy" | "signed_url"
	SignedURLTTL      Duration         `toml:"signed_url_ttl"`
	SignedURLFor      []string         `toml:"signed_url_for"`
	Advertise         bool             `toml:"advertise"`
	AdvertiseFiltered bool             `toml:"advertise_filtered"`
	Require           []string         `toml:"require"`
}

// --- LFS / upstream / git / telemetry / events ---

type LFS struct {
	Enabled        bool     `toml:"enabled"`
	ServeVia       string   `toml:"serve_via"`
	SignedURLTTL   Duration `toml:"signed_url_ttl"`
	MaxObjectBytes ByteSize `toml:"max_object_bytes"`
}

type Upstream struct {
	Git      string   `toml:"git"`
	Lfs      string   `toml:"lfs"`
	TokenEnv string   `toml:"token_env"`
	Follow   []string `toml:"follow"`
}

type Git struct {
	Binary                 string `toml:"binary"`
	UploadPackEngine       string `toml:"upload_pack_engine"` // auto|git|gix (gix == git at load)
	AllowFilter            bool   `toml:"allow_filter"`
	AllowAnySHA1InWant     bool   `toml:"allow_any_sha1_in_want"`
	ObjectFormat           string `toml:"object_format"`
	CommitGraph            bool   `toml:"commit_graph"`
	CommitGraphChangedPath bool   `toml:"commit_graph_changed_paths"`
	HistoryPack            bool   `toml:"history_pack"`
	MaxWants               int    `toml:"max_wants"`
}

type Telemetry struct {
	LogFormat    string   `toml:"log_format"` // "pretty" | "json"
	LogFilter    string   `toml:"log_filter"`
	Metrics      bool     `toml:"metrics"`
	LockWaitWarn Duration `toml:"lock_wait_warn"`
}

type Events struct {
	WebhookURL    string      `toml:"webhook_url"`
	WebhookSecret string      `toml:"webhook_secret"`
	SweepInterval Duration    `toml:"sweep_interval"`
	Sinks         []EventSink `toml:"sinks"`
}

// EventSink is one `[[events.sinks]]` entry (14_extensibility.md §14.6).
// Kind selects the built-in sink implementation; the remaining keys are the
// kind's own:
//
//	kind = "webhook"  url (required), secret_env (optional env var name)
//	kind = "jsonl"    path (required), max_bytes, keep
//
// The [events] webhook_url/webhook_secret pair stays the v1 single-sink
// spelling and is equivalent to one webhook sink.
type EventSink struct {
	Name      string   `toml:"name"`
	Kind      string   `toml:"kind"`
	URL       string   `toml:"url"`
	SecretEnv string   `toml:"secret_env"`
	Path      string   `toml:"path"`
	MaxBytes  ByteSize `toml:"max_bytes"`
	Keep      int      `toml:"keep"`
}

// Event sink kinds (the documented set; events.SinkKinds pins it).
const (
	EventSinkWebhook = "webhook"
	EventSinkJSONL   = "jsonl"
)

// Defaults for the jsonl sink's rotation (the doc's semantics: rotate when the
// next NDJSON frame would push the file past max_bytes, keep `keep` rotated
// files, drop the oldest).
const (
	DefaultEventSinkMaxBytes int64 = 64 << 20
	DefaultEventSinkKeep           = 3
)

// Releases caps the collaboration release surface (docs/features/07 §1.2:
// releases.max_asset_bytes, default 2 GiB → 413 over cap).
type Releases struct {
	MaxAssetBytes ByteSize `toml:"max_asset_bytes"`
}

// Attachments caps pasted/dropped issue images (docs/features/02 §12:
// attachments.max_image_bytes, default 8 MiB → 413 over cap).
type Attachments struct {
	MaxImageBytes ByteSize `toml:"max_image_bytes"`
}

// Webedit bounds the browser file editor (docs/features/17_file_edit.md:
// webedit.max_bytes, default 1 MiB). The cap is the largest single file the
// editor accepts; the commit's pack stays O(KiB) above it.
type Webedit struct {
	MaxBytes ByteSize `toml:"max_bytes"`
}

// StaticSites configures the dedicated static-site host (docs/features/19 §5):
// a second HTTP listener that answers only for its own Host and serves
// committed bytes out of public repositories. Off by default — the listener is
// not started at all when enabled is false, which is why the rest of the
// section is not validated then.
type StaticSites struct {
	Enabled bool `toml:"enabled"`
	// Address is the listener's own host:port. It must differ from
	// server.listen: the isolation comes from the hostname (§2), but two
	// listeners cannot share one socket.
	Address string `toml:"address"`
	// Host is the ONLY Host header value this listener answers for
	// (port-insensitive, case-insensitive). Every other Host is a bodiless
	// 404, so a proxy pointing the app's name here cannot reach user content.
	Host string `toml:"host"`
	// MaxFileBytes caps one served file: the v1 read path buffers the blob, so
	// a larger object is a 404 rather than an unbounded allocation.
	MaxFileBytes ByteSize `toml:"max_file_bytes"`
	// IndexFiles is the directory-index probe order inside a tree. A directory
	// matching none of them is a 404 — never a listing (§3).
	IndexFiles []string `toml:"index_files"`
}

// RateLimit is the request rate-limit surface (PLAN-V2 §M.7, docs/features/16
// §12). Additive section (14_extensibility.md §14.12). Every rate is a token
// bucket spelled "N/s" | "N/min" | "N/h" with N > 0, parsed by ParseRate:
// N tokens per window, refilled continuously. The limiter is in-process
// middleware state (a control-plane cache, never bucket state), so every key
// here is restart-required — the buckets are built at boot and the process
// does not hot-reload.
//
// enabled defaults to false: a zero-config boot changes nothing (L10). Git
// pushes and reads are never rate limited (a CI push storm must not throttle);
// classes cover collaboration writes, issue/PR opens, abuse reports and failed
// authentication attempts.
type RateLimit struct {
	Enabled     bool   `toml:"enabled"`
	CollabWrite string `toml:"collab_write"` // comments, reviews, reactions, review threads/requests
	IssueOpen   string `toml:"issue_open"`   // opening an issue or pull request
	Report      string `toml:"report"`       // filing an abuse report
	AuthFail    string `toml:"auth_fail"`    // failed authentication attempts
	// TrustProxy makes the limiter key anonymous requests on the FIRST hop of
	// X-Forwarded-For. Default false: the header is client-controlled unless a
	// trusted reverse proxy overwrites it, and trusting it unconditionally
	// would let anyone mint unlimited limiter keys (16 §12.2).
	TrustProxy bool `toml:"trust_proxy"`
}

// Search is the code-search surface (docs/features/20_search.md §5.2).
// Additive section (14_extensibility.md §14.12). Two operator-only keys: every
// other cap (pattern bytes, pathspecs, matches, bytes, deadlines, candidate
// and worker counts) is a package constant in internal/search, promoted to a
// config key only when an operator actually hits it. There is deliberately no
// internal/setup schema row beyond what reflection already produces from this
// struct: these are operator knobs, documented in 11_config_cli.md.
type Search struct {
	// Enabled turns the search routes on. It is a *bool so the JSON/TOML
	// round trip can distinguish "absent" from an explicit false, and an
	// absent key means enabled (the compiled default is true): search is part
	// of the ordinary read surface, and only an explicit `enabled = false`
	// turns it off. Use IsEnabled, never a nil dereference.
	Enabled *bool `toml:"enabled"`
	// MaxConcurrent bounds concurrent searches per instance (default 2; 0 =
	// the default; 1..SearchMaxConcurrent). A single search can fan out to
	// four workers, so this is the bound that keeps one public repository from
	// turning a stranger's query into a CPU monopsony (20_search.md §11).
	MaxConcurrent int `toml:"max_concurrent"`
}

// IsEnabled reports the effective search flag: nil means enabled.
func (s Search) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// SearchMaxConcurrent is the validation ceiling for [search] max_concurrent.
// Each concurrent search can hold up to four grep workers and a scan-pool
// slot, so the ceiling is about protecting clone/upload-pack CPU, not a
// capacity figure (20_search.md §11).
const SearchMaxConcurrent = 16

// Bool returns a pointer to v (the *bool spelling config fields use).
func Bool(v bool) *bool { return &v }

// Packages bounds the OCI registry (docs/features/14 §5): one blob upload cap,
// one manifest cap, the chunked-upload staging TTL the maintainer sweep
// honours, and the instance default per-repository byte quota. Additive
// section (14_extensibility.md §14.12 — existing keys never change meaning).
type Packages struct {
	MaxBlobBytes     ByteSize `toml:"max_blob_bytes"`     // one blob object (default 10 GiB, GHCR parity)
	MaxManifestBytes ByteSize `toml:"max_manifest_bytes"` // one manifest body (default 4 MiB)
	UploadTTL        Duration `toml:"upload_ttl"`         // abandoned upload staging (default 24h)
	// QuotaBytes is the instance default registry quota per repository
	// (docs/features/14 §5): the stored blob + manifest bytes a repository
	// may hold. 0 = unlimited — the compatible default, because a quota
	// nobody asked for would start refusing pushes after an upgrade. A
	// repository's settings [packages] quota_bytes overrides it, including
	// an explicit 0 (unlimited for that repository).
	QuotaBytes ByteSize `toml:"quota_bytes"`
}

// Threads bounds the CI surface (docs/features/15_threads.md §8): terminal-run
// retention, the first-time-fork approval gate and the default job-token
// permissions. Additive section (14_extensibility.md §14.12).
type Threads struct {
	// Retention is the terminal-run window: a finished run (with its job
	// records, log frames and queue entries) older than this is deleted by the
	// maintainer's `threads-gc` pass. Default 720h (30 days); non-positive
	// falls back to the default (there is no "keep forever" spelling).
	Retention Duration `toml:"retention"`
	// ApproveFirstTimeForks holds a fork pull request from an author with no
	// merged PR in the repository in `pending_approval` until a maintainer
	// approves it (15 §5.2). Default true.
	ApproveFirstTimeForks bool `toml:"approve_first_time_forks"`
	// DefaultPermissions is the job token's `contents` level when a workflow
	// declares no `permissions:` ("read" | "write"; default read). A fork run
	// is always read regardless.
	DefaultPermissions string `toml:"default_permissions"`
	// MaxArtifactBytesPerRun / MaxArtifactBytesPerFile bound published run
	// artifacts (15 §2.5): the per-run total and one uploaded file. Defaults
	// 2GiB / 256MiB; a per-file value above the per-run value is clamped.
	MaxArtifactBytesPerRun  ByteSize `toml:"max_artifact_bytes_per_run"`
	MaxArtifactBytesPerFile ByteSize `toml:"max_artifact_bytes_per_file"`
	// ImageAllowPrefixes is the instance-wide image allowlist (L2 of
	// docs/PLAN-ARTIFACT-KEEPER.md §3.3): a job's container.image and every
	// services.<id>.image must start with one of these literal host[:port]/path
	// prefixes at a boundary, or the job is refused at creation. Empty = no
	// restriction (the compatible default).
	ImageAllowPrefixes []string `toml:"image_allow_prefixes"`
	// ImageRequireDigest refuses a workflow-named image without @sha256:
	// (independent of the allowlist: a prefix can be allowed and still
	// mutable). Default false; turning it on breaks every tag-named workflow,
	// so it is a deliberate rollout.
	ImageRequireDigest bool `toml:"image_require_digest"`
	// CacheMaxBytes is the per-repository cache budget (PAR-095,
	// threads.cache_max_bytes); 0 selects 5 GiB. Eviction is oldest-saved-first
	// inside that budget.
	CacheMaxBytes ByteSize `toml:"cache_max_bytes"`
	// CacheMaxEntryBytes bounds one cache entry (threads.cache_max_entry_bytes;
	// 0 selects 1 GiB, clamped to CacheMaxBytes).
	CacheMaxEntryBytes ByteSize `toml:"cache_max_entry_bytes"`
	// CacheRetention is the idle window after which an unused cache entry is
	// reclaimed (threads.cache_retention; 0 selects 7 days).
	CacheRetention Duration `toml:"cache_retention"`
	// CacheMaxEntries bounds one repository's cache index (0 selects 256).
	CacheMaxEntries int `toml:"cache_max_entries"`
	// ArtifactRetention is the artifact window (PAR-158), separate from
	// Retention: an artifact whose own window is longer OUTLIVES its run.
	// Zero selects the run window (today's behaviour); valid 24h…8760h.
	ArtifactRetention Duration `toml:"artifact_retention"`
	// KeepForever disables run/log/artifact reclamation entirely (PAR-158).
	// Dead runner records are still pruned: infrastructure, not history.
	KeepForever bool `toml:"keep_forever"`
	// MaxQueueAttempts bounds a job's requeues before it fails (PAR-179;
	// default 3, valid 1…20). Zero selects the default.
	MaxQueueAttempts int `toml:"max_queue_attempts"`
	// JobLease is the base runner claim lease (PAR-179; default 15m, valid
	// 5m…30m). A job's own timeout adds its grace window; the effective lease
	// is capped at 30m because log frames renew it.
	JobLease Duration `toml:"job_lease"`
	// Actions is the remote-action policy (PAR-070/D-5, docs/threads.md
	// §3.4d). It governs `uses: owner/repo[/path]@ref`: default-deny via an
	// allowlist, a full commit SHA required outside it, this instance as the
	// only source unless `source` names one, and a bounded fetch.
	Actions ThreadsActions `toml:"actions"`
	// SMTP is the failure-notification mailer (PAR-197/D-27). It is OFF when
	// host is empty, which is the default: a configured instance sends one
	// message per (subscriber, failed run) to the subscriber's verified email
	// address, with the same failure-only event filter and the same masking as
	// the in-app inbox. Credentials are only accepted over TLS.
	SMTP ThreadsSMTP `toml:"smtp"`
}

// ThreadsSMTP is the stdlib SMTP notifier (threads.smtp.*, D-27). Host empty
// disables the channel entirely; when set, From and a TLS mode are required.
type ThreadsSMTP struct {
	// Host is the SMTP server hostname; empty disables email.
	Host string `toml:"host"`
	// Port is the server port (0 selects 587 for starttls, 465 for implicit).
	Port int `toml:"port"`
	// From is the envelope/From address; required when Host is set.
	From string `toml:"from"`
	// Username and Password authenticate with PLAIN, only over TLS. Leave both
	// empty for an unauthenticated relay. Prefer the env overlay for Password:
	// SPINDLE__THREADS__SMTP__PASSWORD.
	Username string `toml:"username"`
	Password string `toml:"password"`
	// TLS is starttls (default), implicit (SMTPS) or none. `none` is only for
	// a trusted loopback relay: it is refused when a username is set.
	TLS string `toml:"tls"`
	// Timeout bounds one delivery (default 10s).
	Timeout Duration `toml:"timeout"`
}

// ThreadsActions is the remote-action policy (threads.actions.*).
type ThreadsActions struct {
	// Mode is deny | allowlist | open. `allowlist` (the default) permits a
	// mutable ref only for an entry in Allow; `deny` refuses every remote
	// action; `open` accepts mutable refs everywhere (an explicit weakening).
	Mode string `toml:"mode"`
	// Allow lists approved repositories: `owner/repo`, `owner/*`, `*`, or
	// `host/owner/repo` for the configured source host.
	Allow []string `toml:"allow"`
	// RequireSHA requires a full 40-character commit SHA for a mutable ref
	// outside Allow (default true — D-5's decided shape).
	RequireSHA bool `toml:"require_sha"`
	// Source is the optional external action source, a Forgejo/Gitea-compatible
	// base URL (https://git.example.com). Empty resolves from THIS instance.
	Source string `toml:"source"`
	// MaxBytes bounds one fetched action's content (default 16 MiB; 0 selects
	// the default).
	MaxBytes ByteSize `toml:"max_bytes"`
}

// Import bounds the repository-import surface (docs/features/10 §6:
// additive [import] section, 14_extensibility.md §14.12 — existing keys
// never change meaning). All timeouts are per-spawn ctx timeouts; sizes
// gate the scratch checkout (post-clone du) and published packs.
type Import struct {
	CloneTimeout         Duration `toml:"clone_timeout"`          // clone --mirror ctx (default 1800s)
	GitTimeout           Duration `toml:"git_timeout"`            // for-each-ref/verify/repack ctx (default 300s)
	MaxBytes             ByteSize `toml:"max_bytes"`              // scratch du + pack gate (default = server.max_push_bytes at load)
	MaxRefs              int      `toml:"max_refs"`               // ref-enumeration cap (default 100000)
	MaxConcurrent        int      `toml:"max_concurrent"`         // server-side concurrent clones (default 2)
	AllowPrivateNetworks bool     `toml:"allow_private_networks"` // SSRF: allow loopback/RFC1918 sources (default false)
	URLAllowlist         []string `toml:"url_allowlist"`          // empty = public hosts allowed; non-empty = only these hosts
	AllowFileURLs        bool     `toml:"allow_file_urls"`        // file:// sources (default false; tests/fixtures only)
}

// Config is the whole file. Section/field names match the TOML keys exactly.
type Config struct {
	Server      Server      `toml:"server"`
	Store       Store       `toml:"store"`
	Cache       Cache       `toml:"cache"`
	WAL         WAL         `toml:"wal"`
	Maintenance Maintenance `toml:"maintenance"`
	Placement   Placement   `toml:"placement"`
	Compaction  Compaction  `toml:"compaction"`
	Bundles     Bundles     `toml:"bundles"`
	LFS         LFS         `toml:"lfs"`
	Upstream    Upstream    `toml:"upstream"`
	Git         Git         `toml:"git"`
	Telemetry   Telemetry   `toml:"telemetry"`
	Events      Events      `toml:"events"`
	Releases    Releases    `toml:"releases"`
	Import      Import      `toml:"import"`
	Attachments Attachments `toml:"attachments"`
	Packages    Packages    `toml:"packages"`
	Threads     Threads     `toml:"threads"`
	Webedit     Webedit     `toml:"webedit"`
	StaticSites StaticSites `toml:"static_sites"`
	RateLimit   RateLimit   `toml:"rate_limit"`
	Search      Search      `toml:"search"`

	// DataDir is the zero-config root (divergence D5): --data-dir / SPINDLE_DATA_DIR,
	// default ~/.local/share/spindle. Holds store/, cache/, and the saved spindle.toml.
	// Not a TOML key (never loaded from the file); set by the loader.
	DataDir string `toml:"-"`

	// explicit records the dotted keys the config file or the env overlay
	// actually set — provenance for "is this still the compiled default?"
	// decisions (the cache-space clamp, BUG-6). Unexported: never a TOML
	// key, never serialized, and a config built in a test/API caller simply
	// has no map (Explicit reports false).
	explicit map[string]bool
}

// Explicit reports whether the config file or the env overlay set the dotted
// key (case-insensitive). A value equal to the compiled default that was never
// explicitly set reports false.
func (c *Config) Explicit(key string) bool {
	if c == nil || c.explicit == nil {
		return false
	}
	return c.explicit[strings.ToLower(key)]
}

func (c *Config) markExplicit(key string) {
	if c.explicit == nil {
		c.explicit = make(map[string]bool)
	}
	c.explicit[strings.ToLower(key)] = true
}

// Defaults returns the COMPILED-IN defaults (11_config_cli.md §2 — equal to Rust-spec defaults).
// The zero-config first run applies FirstRunDefaults(dataDir) on top (divergence D5, §2.3);
// the config owner implements it in sibling files.
func Defaults() *Config {
	return &Config{
		Server: Server{
			Listen:                  "127.0.0.1:8080",
			HTTP2:                   true,
			MaxConcurrentRequests:   512,
			MaxConcurrentPerRepo:    64,
			RequestTimeout:          Duration(time.Hour),
			DrainTimeout:            Duration(20 * time.Second),
			ServeSyncTimeout:        Duration(45 * time.Second),
			ServeMaterializeTimeout: Duration(10 * time.Minute),
			MaxPushBytes:            64 << 30,
			MaxTreeLog:              200,
			Auth: Auth{
				AnonymousRead:  true,
				AdminGroups:    []string{"spindle-admin"},
				GroupsClaim:    "groups",
				SessionTTL:     Duration(30 * 24 * time.Hour),
				AccessTokenTTL: Duration(90 * 24 * time.Hour),
			},
		},
		Store: Store{
			// filesystem is the documented default (OBS-1): a bucket backend
			// is selected explicitly by the file or the env overlay, never
			// inherited by omission (a file that merely omits store.backend
			// once pointed a deployment at AWS).
			Backend:            "filesystem",
			Bucket:             "walgit",
			MaxRetries:         8,
			MultipartThreshold: 64 << 20,
			MultipartPartSize:  32 << 20,
			S3: S3{
				// No implicit AWS endpoint: selecting s3 requires an
				// explicit endpoint (validation fails loudly otherwise).
				Endpoint:     "",
				Region:       "us-east-1",
				AccessKeyEnv: "AWS_ACCESS_KEY_ID",
				SecretKeyEnv: "AWS_SECRET_ACCESS_KEY",
			},
		},
		Cache: Cache{
			Dir:                "/tmp/walgit",
			Mode:               "auto",
			MaxBytes:           20 << 30,
			DiskHighWatermark:  0.9,
			EvictIdleAfter:     Duration(6 * time.Hour),
			PrewarmParallelism: 2,
			RefAdvertEntries:   256,
			ObjectInfoEntries:  4096,
			BundleListEntries:  128,
			RemoteBlockBytes:   1 << 30,
			RemoteObjectBytes:  256 << 20,
			SharedRenderCache:  true,
		},
		WAL: WAL{
			BatchWindow:           Duration(5 * time.Millisecond),
			MaxBatch:              64,
			PushBrokerBufferBytes: 64 << 20,
			SnapshotEveryEntries:  256,
			CheckpointInterval:    Duration(time.Hour),
			CheckpointTailBytes:   8 << 20,
			CASMaxRetries:         16,
			FsckObjects:           true,
			CheckConnectivity:     true,
			PrefetchPacks:         true,
			PrefetchMaxBytes:      1 << 30,
			RemoteObjects:         true,
		},
		Maintenance: Maintenance{
			Interval:       Duration(time.Minute),
			Checkpoints:    true,
			Disk:           "tmpfs",
			FsckInterval:   Duration(7 * 24 * time.Hour),
			FollowInterval: Duration(30 * time.Second),
		},
		Threads: Threads{
			Retention:               Duration(30 * 24 * time.Hour),
			ApproveFirstTimeForks:   true,
			DefaultPermissions:      "read",
			MaxArtifactBytesPerRun:  2 << 30,
			MaxArtifactBytesPerFile: 256 << 20,
			// Image policy defaults to today's behaviour: no allowlist and no
			// digest requirement (plan §3.3 — shipping an allowlist non-empty
			// would break CI on the first push after the upgrade).
			ImageAllowPrefixes: nil,
			ImageRequireDigest: false,
			// The artifact window defaults to the run window (unchanged
			// behaviour); keep_forever off; 3 attempts and a 15m lease.
			ArtifactRetention: 0,
			// The cache service (PAR-095): 5 GiB per repository, 1 GiB per
			// entry, 256 entries, 7 days idle.
			CacheMaxBytes:      5 << 30,
			CacheMaxEntryBytes: 1 << 30,
			CacheRetention:     Duration(7 * 24 * time.Hour),
			CacheMaxEntries:    256,
			KeepForever:        false,
			MaxQueueAttempts:   3,
			JobLease:           Duration(15 * time.Minute),
			// Remote actions (PAR-070/D-5): allowlist mode, deny by default,
			// SHA required outside the allowlist, this instance as the only
			// source, 16 MiB per action.
			Actions: ThreadsActions{
				Mode:       "allowlist",
				Allow:      nil,
				RequireSHA: true,
				Source:     "",
				MaxBytes:   16 << 20,
			},
			// Email notifications are off by default (D-27); the remaining
			// defaults describe the shape a configured mailer takes.
			SMTP: ThreadsSMTP{Host: "", Port: 0, From: "", TLS: "starttls", Timeout: Duration(10 * time.Second)},
		},
		Placement: Placement{Serve: []string{"*"}, Maintain: []string{"*"}},
		Compaction: Compaction{
			Enabled:             true,
			Factor:              2,
			TriggerPacks:        16,
			TriggerBytes:        1 << 30,
			LeaseTTL:            Duration(10 * time.Minute),
			RetentionSuperseded: Duration(7 * 24 * time.Hour),
			Engine:              "git",
		},
		Bundles: Bundles{
			Strategy: []BundleStrategy{
				{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2, BackfillMax: 1}, // Sunday 23:00 UTC (@weekly; numeric dow — names are rejected by the parser)
				{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "0 0 23 * * *", BackfillMax: 0, Chain: true},
				{Name: "hourly", Kind: "incremental", Base: "daily", Schedule: "0 0 * * * *", BackfillMax: 48},
			},
			MinCommits:   25,
			MainOnly:     true,
			ServeVia:     "proxy",
			SignedURLTTL: Duration(time.Hour),
			Advertise:    true,
		},
		LFS: LFS{
			Enabled:        true,
			ServeVia:       "proxy",
			SignedURLTTL:   Duration(time.Hour),
			MaxObjectBytes: 16 << 30,
		},
		Git: Git{
			Binary:           "git",
			UploadPackEngine: "auto",
			AllowFilter:      true,
			ObjectFormat:     "sha1",
			CommitGraph:      true,
			HistoryPack:      true,
		},
		Telemetry: Telemetry{
			LogFormat:    "pretty",
			LogFilter:    "info,walgit=debug",
			Metrics:      true,
			LockWaitWarn: Duration(time.Second),
		},
		Events: Events{SweepInterval: Duration(5 * time.Minute)},
		Releases: Releases{
			MaxAssetBytes: 2 << 30,
		},
		Import: Import{
			CloneTimeout:  Duration(1800 * time.Second),
			GitTimeout:    Duration(300 * time.Second),
			MaxBytes:      64 << 30,
			MaxRefs:       100000,
			MaxConcurrent: 2,
		},
		Attachments: Attachments{
			MaxImageBytes: 8 << 20,
		},
		Packages: Packages{
			MaxBlobBytes:     10 << 30,
			MaxManifestBytes: 4 << 20,
			UploadTTL:        Duration(24 * time.Hour),
			// Unlimited by default: a compiled-in quota would begin
			// refusing pushes on an upgrade that never asked for one.
			QuotaBytes: 0,
		},
		Webedit: Webedit{MaxBytes: 1 << 20},
		StaticSites: StaticSites{
			// Off by default: a compiled-in second listener would open a new
			// origin on every upgrade that never asked for one.
			Enabled:      false,
			Address:      "127.0.0.1:8081",
			Host:         "static.localhost",
			MaxFileBytes: 8 << 20,
			IndexFiles:   []string{"index.html", "index.htm"},
		},
		RateLimit: RateLimit{
			// Off by default (PLAN-V2 §M.12 open question (c)): enabling
			// limits on a live instance is an operator decision with a
			// visible 429, never a silent zero-config change.
			Enabled:     false,
			CollabWrite: "30/min",
			IssueOpen:   "10/h",
			Report:      "5/h",
			AuthFail:    "10/min",
			TrustProxy:  false,
		},
		Search: Search{
			// Enabled is compiled-in true (docs/features/20_search.md §5.2):
			// search is a read of the serving copy, needs no new bucket key
			// and no index, so a zero-config boot keeps the ordinary surface.
			// The pointer is set (not nil) so the Configuration page renders
			// the switch as on and a save writes the explicit value.
			Enabled:       Bool(true),
			MaxConcurrent: 2,
		},
	}
}
