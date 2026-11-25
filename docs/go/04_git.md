# 04 — Git layer (`internal/git`)
> Source: MASTER_RUST_SPEC.md §7 (7.1–7.9), with §8.4 gates and §15 config keys · Status: normative for the spindle Go implementation.


`internal/git` owns every interaction with the `git` binary: pack ingest, ref storage, pkt-line
advertisements, receive-pack and upload-pack flows, repack/commit-graph maintenance plumbing, bundle
primitives, and upstream-repair/follow helpers. Git is ALWAYS the subprocess `git` binary with exact
argv — never go-git or any VCS library (hard dependency rule). Zero third-party imports in this
package; the concurrency playbook is `13_concurrency.md`.

Interfaces this package exposes (all cross-doc references by file name only):

```go
// internal/git
type Layer struct { /* binary path, scratch root, config, blocking pool */ }
func (l *Layer) Ingest(ctx, repo *Repo, pack io.Reader, maxBytes int64, thin, fsck bool) (*IngestResult, error)
func (l *Layer) ApplyRefTxn(ctx, repo *Repo, txn []RefUpdate, checkOld bool) error
func (l *Layer) Snapshot(repo *Repo) (*RefSnapshot, error)
func (l *Layer) Advertisement(repo *Repo, svc Service, v2 bool, version string) ([]byte, error)
func (l *Layer) LsRefs(repo *Repo, args LsRefsArgs) ([]byte, error)
func (l *Layer) UploadPack(ctx, repo *Repo, body io.Reader, out io.Writer, protocol string) error   // --stateless-rpc (HTTP framing)
func (l *Layer) UploadPackSSH(ctx, repo *Repo, body io.Reader, out io.Writer, protocol string) error  // no --stateless-rpc (17_ssh.md §2)
func (l *Layer) ParsePushRequestStream(repo *Repo, r io.Reader) (*PushRequest, io.Reader, error)      // commands only; pack stays a stream (17_ssh.md §5)
func (l *Layer) IngestStream(ctx, repo *Repo, pack io.Reader, maxBytes int64, thin, fsck bool) (*IngestResult, error) // pack piped into index-pack (17_ssh.md §5)
func (l *Layer) Grep(ctx, repo *Repo, rev string, q GrepQuery) (*GrepResult, error) // code search at a resolved sha (§17)
```

Errors are sentinel-wrapped values the callers map: `ErrMaxBytes`, `ErrPackRejected`, `ErrNoSpace`,
`ErrMissingObject`, `ErrRefConflict`, `ErrTooManyWants`. The WAL publish path (`05_wal_engine.md`) and
HTTP layer (`06_server_http.md`) consume these; policy evaluation (`policy.json`, §14) happens in
`internal/policy`, not here.

## 1. Repo identity and local layout (§7.1)

### 1.1 RepoId validation

`RepoId{Owner, Name string}`; parse from `owner/name` or `owner/name.git` (strip one trailing `.git`).
Per part, ALL of:

| Rule | Value |
|---|---|
| Charset | ASCII `[A-Za-z0-9._-]` only (reject non-ASCII, whitespace, `/`, `+`, `%`, `:`) |
| Length | 1..=100 chars |
| No leading `.` | reject `.foo` (also blocks hidden dirs) |
| Not `..` | exact match rejected |

`ValidateRepoPath(s string) (RepoId, error)` is the single entry point; every route handler MUST call it
before touching the filesystem or the store. The store prefix is `repos/<owner>/<repo>/` (doc 02) and the
local dir is `<cache.dir>/<owner>/<name>.git`.

### 1.2 Local bare layout

The local directory is a **standard bare repo** readable by stock git (and by any future in-process
engine): `objects/pack/*`, loose refs + `packed-refs`, `HEAD`, `config`.

Init sequence for a new repo:

```
git init --bare --object-format=sha256    # ONLY when the repo's format is sha256
git init --bare                           # for sha1 repos (no flag)
```

Then write repo-local config (append to `config`; idempotent re-init rewrites the same keys):

```ini
[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
	allowSidebandAll = true
[pack]
	writeReverseIndex = true
```

Then write `HEAD` directly: `ref: refs/heads/main\n`.

Object formats are sha1/sha256 everywhere: 40/64-hex oids, all-zero zero-oids (`strings.Repeat("0", n)`),
the `object-format=<fmt>` capability, and the per-repo format persisted in the manifest/checkpoint/snapshot
(doc 02/05). Every oid type in this package is `type Oid string` validated by format.

#### Subprocess
`git init` is synchronous and fast: run via the shared exec helper (§2) with a 30 s
`context.WithTimeout`. No blocking-pool needed, but count it against the pool anyway for uniform
accounting.

## 2. Subprocess discipline and the blocking pool (normative for EVERY git call)

Every git spawn in this package follows ONE shape:

```go
cmd := exec.CommandContext(ctx, l.Binary, args...)   // ctx always has a timeout
cmd.Env = []string{"GIT_DIR=" + gitDir, /* extra env below */}
cmd.Dir = workDir
// stdin/stdout discipline:
//   - feed stdin from a helper goroutine, then cmd.StdinPipe().Close()
//   - drain stdout with io.Copy (to the consumer or io.Discard) on the calling goroutine
//   - drain stderr into a bounded ring buffer (capped 8 KiB, kept for error text)
//   - ONLY THEN cmd.Wait()
```

**The deadlock rule**: never `Wait()` while a stdin writer and a stdout reader both live on the same
goroutine and neither is closed; git blocks writing to a full stdout pipe while we block writing stdin.
Concretely: large-stdin commands (`index-pack`, `upload-pack`, `update-ref --stdin`, `bundle create`,
`pack-objects`) get a feeder goroutine that `io.Copy`s from the source and closes the pipe (close errors
are swallowed — the child may have exited); the parent reads stdout to EOF, then `Wait`s.

Environment variables used across this package (set them all explicitly; do not inherit the ambient env
except `PATH`): `GIT_DIR`, `GIT_TERMINAL_PROMPT=0` (every call — git must never hang on a credential
prompt), `GIT_TRACE2_EVENT` (ingest only), `GIT_PROTOCOL` (upload-pack only), credential-helper config
passed as `-c` argv (upstream helpers only). `git.binary` (default `"git"`) supplies the executable path.

**Blocking pool.** Rust used `spawn_blocking`; Go's equivalent is a bounded semaphore of goroutines:

```go
type Pool struct{ sem chan struct{} }        // capacity = git.max_git_procs (default: 4 * GOMAXPROCS)
func (p *Pool) Run(ctx context.Context, fn func() error) error {
    select {
    case p.sem <- struct{}{}:
        defer func() { <-p.sem }()
        return fn()
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

One **derived** pool sits beside it: `Layer.ScanPool` (2 slots, §17.2) is used only by the
long read-only `git grep` scan, so a broad search can never starve clone/upload-pack and a
clone can never starve search. It is never the pool above.

### Concurrency
- **Hazard:** unbounded concurrent git processes exhaust CPU/memory (each `upload-pack` can be ~RSS of the
  pack); a push handler holding a pool slot forever starves the server.
- **Avoidance:** every heavyweight git exec (`index-pack`, `upload-pack`, `repack`, `commit-graph`,
  `pack-objects`, connectivity pipeline, `bundle create`) runs inside `Pool.Run`. Per-repo request
  parallelism is additionally capped by the `server.max_concurrent_per_repo` semaphore taken by the HTTP
  layer BEFORE the handler (doc 06). Timeouts: ingest 600 s (config `git.ingest_timeout`), upload-pack and
  connectivity bound by the request context, maintenance commands 1800 s. Context cancellation kills the
  process (`exec.CommandContext` sends SIGKILL); scratch cleanup still runs (§3). Channel/pipe ownership:
  the feeder goroutine owns and closes the stdin pipe; the caller owns and drains stdout. Canonical
  patterns: `13_concurrency.md` §"bounded parallelism", §"drain".

## 3. Pack ingest (receive path) (§7.2)

`Ingest` runs `git index-pack` in a **scratch git-dir per ingest — never the serving copy**.

### 3.1 Scratch dir

Path: `<repo>/walgit-ingest-<pid>-<nanos>/` under the repo's local dir. Contents created by hand (no
subprocess): `objects/pack/` (empty), `objects/info/alternates` containing one line: the repo's
`objects/` dir absolute path (so `--fix-thin`/`--fsck-objects` resolve bases), empty `refs/`, a `HEAD`
copy (`ref: refs/heads/main\n` — format-irrelevant here), and a byte copy of the repo's `config`.

Removal on EVERY exit path: `defer os.RemoveAll(scratch)` after the defer list also sweeps git's leaked
tmp files (`scratch/tmp_*`, `scratch/objects/pack/tmp_*`). A rejected push leaves nothing behind.

### 3.2 Spawn

```
git index-pack --stdin --keep --rev-index --threads=0 [--fix-thin] [--fsck-objects]
```

- `--fsck-objects` present when `wal.fsck_objects` (default true); `--fix-thin` always for receive-pack
  (`thin: true`); neither for bundle backfill ingest paths that pass complete packs.
- Env: `GIT_DIR=<scratch>`, `GIT_TRACE2_EVENT=<tmp>/walgit-index-pack-<suffix>.jsonl` (suffix = the
  nanos already in the scratch name).
- stdin = the streamed pack, piped. `max_bytes` = `server.max_push_bytes` is enforced WHILE streaming:
  the feeder goroutine copies 64 KiB chunks into a temp file, aborting with `ErrMaxBytes` ("pack exceeds
  max_bytes") the moment the cap is crossed; the temp file's fd is then piped into git's stdin.
- Runs via `Pool.Run` (§2), timeout `git.ingest_timeout` (default 600 s), under the per-repo ingest lock
  (see Concurrency).

### 3.3 After success

1. Parse the trailing checksum line from index-pack's stdout (`pack\t<sha>\t<n>`? — git prints
   `pack\t<pack-checksum>` plus keep/idx notices; take the LAST 40/64-hex token as the pack checksum).
2. **Move order idx → rev → pack**, atomic renames into the repo's `objects/pack/pack-<hex>.*`
   (`<hex>` = checksum, lowercase). Pack LAST so an interrupt never leaves a pack without an idx.
   The `.keep` file is discarded — publish (the WAL manifest CAS, doc 05) is the commit point.
3. `object_count` is read from the **idx fanout**: after the 4-byte magic `\xfftOc`, 4-byte version, the
   next 1024 bytes are 256 big-endian fanout counts; `count = fanout[255]`. No subprocess.
4. Zero-object pack (ref-only push) → install nothing, publish no pack, return `object_count == 0`.
5. Parse GIT_TRACE2_EVENT JSONL for metrics: git duration = the `t_abs` (float seconds) of the last
   `region_leave` event (fallback: Go-measured wall time around the exec); region phases from
   `region_enter`/`region_leave` pairs — feed time measured by the Go feeder goroutine (`phase="feed"`),
   git's own regions reported by `region` label (`phase=<label>`). Metrics are advisory
   (`git_index_pack_duration_seconds`, `git_index_pack_phase_seconds{phase}`); a malformed trace file is
   ignored, never an error.

`IngestResult{Checksum Oid, ObjectCount uint64, KeepPath discarded, Trace *IngestMetrics}`.

### 3.4 Failure classification (space vs rejection)

Every failed ingest maps through one classifier (`packFailure`):

- **Out of space / quota (`ErrNoSpace`)** — index-pack's stderr says `No space left on device` or
  `Disk quota exceeded`, or a scratch/rename/temp write failed with `ENOSPC`/`EDQUOT`. The returned
  `*SpaceError` names the write path, the repo cache dir (`cache.dir`), the filesystem type and its
  free/total bytes, and the remedy (`cache.dir` quota or `cache.max_bytes`); it still satisfies
  `errors.Is(err, ErrPackRejected)`. `internal/server` appends the configured
  `cache.max_bytes`/`cache.dir` and logs the failure at Error level before answering the push. This is
  the BUG-6 fix: a full cache used to reach the client as a bare `unpacker error`.
- **Anything else** — `*PackRejectedError{Detail: <stderr tail>}` wrapped around `ErrPackRejected`.
- **`max_bytes` crossed** — `ErrMaxBytes` (mapped before classification), never misread as space.

### 3.5 Concurrency
- **Hazard:** two concurrent ingests into one repo interleave pack renames and corrupt the local odb;
  crash mid-move leaves a torn pack set.
- **Avoidance:** ingest is serialized per repo by the ingest lock — one `sync.Mutex` per repo handle,
  owned by `internal/wal`'s registry (doc 05) and acquired by the receive-pack flow BEFORE streaming the
  body (never while holding the WAL sync lock; lock order: ingest lock → wal sync, never reversed).
  Scratch dirs are unique per call (pid+nanos) so crash debris never collides. The move order idx→rev→pack
  is itself the crash-safety mechanism: a pack without idx is unusable, so partial state is inert and
  swept by the maintainer (doc 10). After a successful ingest the caller refreshes the repo handle (odb
  re-open). Reference: `13_concurrency.md` §"lock ordering".

## 4. Refs (§7.3)

### 4.1 Reading

- `HEAD`: read the file; `ref: <target>` prefix → symbolic (`head_target`), else detached oid.
- `packed-refs`: line format `<oid> <name>`; a following line `^<peeled>` attaches `Peeled` to the
  previous ref. Header line `# pack-refs with: peeled fully-peeled sorted` skipped.
- Loose refs: walk `refs/` recursively (`filepath.WalkDir`), skipping `*.lock`; a loose file starting
  `ref: ` is a symref resolved ONLY when its target is already known in the snapshot (packed or
  previously read); otherwise record unresolved and skip. Loose overrides packed.
- Tag peeling: only `refs/tags/*`; per-oid cache (`map[Oid]Oid` + mutex, per repo, LRU-capped at
  `cache.ref_advert_entries`); max 16 tag hops, deeper chains treated as unpeelable. Peeling uses one
  persistent `git cat-file --batch` process per repo (send `<oid>\n`, read `<oid> <type> <size>\n` +
  payload; for `tag` parse the `object <oid>` header line and repeat).
- Result: `RefSnapshot` — name-sorted, each entry `{Name, Oid, Peeled, HeadTarget}`; `head_target` on the
  snapshot itself.

### 4.2 Snapshot and cache types

```go
type RefEntry struct {
    Name   string // "refs/heads/main"
    Oid    Oid
    Peeled Oid // "" unless an annotated tag was peeled
}
type RefSnapshot struct {
    Gen        uint64      // writer generation at load
    Refs       []RefEntry  // sorted by Name
    HeadTarget string      // "refs/heads/main" or ""
    HeadOid    Oid         // resolved HEAD, "" if unborn
    Fingerprint            // {Gen, PackedLen, PackedMtime, HeadMtime}
}
type RefCache struct {
    mu      sync.RWMutex
    base    *RefSnapshot            // last full parse
    pending map[string]Oid          // name→oid overlay from in-flight txns (nil = deleted)
    peel    map[Oid]Oid             // tag peel cache
}
```

Cache validity is keyed by `(Gen, packed-refs len, packed-refs mtime, HEAD mtime)` — `os.Stat` the two
files, compare, re-parse only on mismatch. Pending txns are folded in memory (O(n) patch over the sorted
slice, no re-parse) when the WAL publish confirms a txn (doc 05 calls `Patch(txn)` after its CAS
succeeds). `RefView()` is O(k): binary-search the base snapshot, merge the pending map — it NEVER
materializes a full copy for a 500 k-ref repo. The `refs_parses` counter (Prometheus, doc 06 metrics) is
the O(refs) gauge; it increments only on real parses.

### Concurrency
- **Hazard:** a writer patching `Refs` while a reader binary-searches it; a pending overlay updated while
  `RefView` iterates it.
- **Avoidance:** `RWMutex`: readers take RLock for the whole view call; writers take Lock for patch or
  re-parse. `RefView` copies nothing — the returned slice aliases the immutable base (base snapshots are
  never mutated in place; patches produce a NEW slice under the write lock, then swap the pointer). Peel
  cache has its own mutex (it is written on read paths). Lock order: ref cache RWMutex → peel mutex;
  never the reverse; never acquire ref cache locks while holding the ingest lock and vice versa.
  `13_concurrency.md` §"copy-on-write snapshots".

### 4.3 Writing: `apply_ref_txn` (authoritative path)

Validation first (reject before any subprocess):

- **Ref names:** `HEAD` is always OK. Everything else MUST start with `refs/`; forbidden bytes
  ``space \n \r ~ ^ : ? * [ \``; no `..`; no `@{`; no `//`; no leading or trailing `/`; no trailing `.`;
  no trailing `.lock`.
- **Oids:** empty or all-zero = absent marker (allowed); otherwise exactly 40 (sha1) or 64 (sha256)
  lowercase hex.
- If `check_old`, verify each old value against the current snapshot view: old = zero → ref MUST NOT
  exist; old set → MUST match. Any mismatch → `ErrRefConflict{Ref, Expected, Actual}`.

Then ONE transaction against git:

```
git update-ref --stdin
```

with stdin (newline-terminated, non-`-z` grammar):

```
start
create <ref> <new> [<old>]      # or: update <ref> <new> [<old>] / delete <ref> [<old>]
...
prepare
commit
```

`<old>` is emitted only when checked; git interprets a zero old oid as "must not exist". Symbolic updates
(`HEAD`) are NOT passed to update-ref: they are applied afterwards by direct file write
(`ref: <target>\n`, target validated as a ref name). On failure (non-zero exit, or `fatal:` on stderr
before `commit`): parse the refname from stderr — the first single-quoted token (`'refs/heads/x'` or
`'HEAD'`) in the offending line — into the `ErrRefConflict`; re-verify against the snapshot so the caller
gets expected/actual.

After success: bump the writer generation and patch the cached snapshot with the txn instead of re-parsing
(§4.2).

### 4.4 Snapshot load (replicas) and offline txns

`LoadSnapshot(refs map[string]RefEntry, head ...)` writes `packed-refs` **atomically** (temp file +
`rename`): header `# pack-refs with: peeled fully-peeled sorted`, entries sorted by name, `^<peeled>`
continuation lines after annotated tags; then remove the loose `refs/` tree and recreate `refs/heads/` +
`refs/tags/` skeletons (empty dirs); then rewrite `HEAD`; then refresh the cache. `ApplyRefTxnsOffline`
is the same merge performed purely in memory (works when objects are absent) and is what snapshot/replay
apply (doc 05). `pack_refs` is `git pack-refs --all --prune` (run in the repo git-dir, 60 s timeout).

### 4.5 Commit-date derivation (Forgejo #247)

One fact git knows and the bucket does not: a commit's dates. Both the
push-path hint (server `WalEngine`, objects just ingested) and the sweep
backfill (maintainer resolver, objects serve-synced) read it with one light
recipe — `Layer.CommitDates`, exact argv (run in the repo git-dir, 30 s
timeout, pool-accounted like every other Layer exec):

`git log -1 --format=%cI%x00%aI <sha>`

(`%x00` is ASCII text in argv — argv can never contain a NUL byte; git
expands it. Same discipline as the §9.6 log recipe in `internal/api`.)
Output is `<committer-date>\x00<author-date>`; either date parses via strict
RFC 3339 (garbage → zero, not an error). Input validation first (no
subprocess on garbage): nil/empty repo, non-`ValidOid` sha, and all-zero
oids (a delete tip has no commit) are `errInvalidInput`. Subprocess
failure, missing objects, and malformed field counts are hard errors —
the CALLER degrades to null activity and never fails its own operation
(R1 B3). `PickCommitTime` is the pure #142 semantic (commit_date first,
author_date fallback; both zero → !ok, record nulls).

## 5. Pkt-line codec

Hand-rolled in this package (`pktline.go`), used by advertisements, receive-pack parsing, and
report-status:

- One line = 4 hex length bytes (total length incl. the 4) + payload. Max total 65520 → payload ≤ 65516.
- Special lengths: `0000` flush-pkt, `0001` delim-pkt, `0002` response-end (v2).
- Encoder: `Pkt(s string)` (payload + `\n` if absent — callers pass exact payloads), `Flush()`,
  `Delim()`. Decoder: `Next() (payload []byte, kind)`; a decoded payload longer than 65516 or non-hex
  length is a protocol error → the request is rejected with pkt `ERR`.
- The v0 first line embeds capabilities after a single NUL byte (`...\0cap1 cap2\n`); parsing splits on
  the first `\x00` only.

## 6. Advertisements (§7.4)

Selection: the `Git-Protocol: version=2` header (case-insensitive token match) selects v2; otherwise v0.
The HTTP layer (doc 06) prepends `# service=<svc>\n` + flush for `info/refs`; `Advertisement` returns only
the body.

### 6.1 v0 (`info/refs`)

Body = refs in name-sorted order, one `<oid> <name>\n` pkt per ref, with the capability line appended to
the FIRST ref line: `<oid> <name>\0<caps>\n`. Peeled lines follow each annotated tag:
`<peeled> <name>{}\n`. For upload-pack, a final `<oid> HEAD\n` pkt is appended if HEAD resolves. Empty
repo → single line `<zero-oid> capabilities^{}\0<caps>\n`. Ends with flush-pkt.

Capability lines, **copied verbatim** (single spaces, this exact order):

| Service | Capabilities |
|---|---|
| receive-pack | `report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta push-options object-format=<fmt> agent=walgit/<version>` |
| upload-pack | `multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want filter object-format=<fmt> agent=walgit/<version>` |

`<fmt>` = `sha1` or `sha256` from the repo format. (Note: repo init sets `uploadpack.allowAnySHA1InWant=true`
unconditionally — that is what makes the repair path's wants-by-SHA work; the separate config key
`git.allow_any_sha1_in_want` may additionally gate advertisement, default false — the known §20
discrepancy, resolved in favor of init-always-true.)

### 6.2 v2 capability advertisement

Rendered by hand (we own ls-refs; `fetch` is delegated to stock git, §8):

```
version 2
agent=walgit/<version>
ls-refs=unborn
fetch=thin-pack ofs-delta sideband-all wait-for-done shallow deepen-since deepen-not deepen-relative filter include-tag
object-format=<fmt>
<flush>
```

Each as its own pkt-line. Advertised `fetch` features MUST be a subset of what stock git accepts in the
delegated request; `packfile-uris` is deliberately absent (no bundle-uri-in-fetch in v1).

### 6.3 v2 `ls-refs`

Args parsed from the client request: `symrefs`, `peel`, `unborn` (flags), `ref-prefix <p>` — also
tolerate `ref-prefix=<p>` — terminated by flush.

**Prefix filtering, O(log n + k)** over the name-sorted snapshot: sort + dedupe the prefixes; for each
prefix `p`, binary-search the lower bound in `Refs`, then walk forward emitting refs whose name starts
with `p` and stops when `name > p` (i.e. when the next ref's name does not have `p` as a prefix — compare
`strings.HasPrefix` and break when `!HasPrefix && name > p`). Merge overlapping prefix ranges (dedupe by
skipping refs already emitted — track the previous emitted index; ranges arrive in ascending prefix
order, so a single lastEmittedIdx suffices). Total cost: k prefixes × O(log n) search + O(matched refs).

HEAD is resolved from `head_target` BEFORE prefix filtering — a prefix that excludes the target must not
hide HEAD; HEAD is advertised when prefixes are empty or one of them is exactly `HEAD`. Unborn HEAD → the
`unborn` pseudo-oid plus ` symref-target:<t>` when `unborn` requested.

Line rendering per ref: `<oid> <name>` + ` symref-target:<t>` (when `symrefs` requested and the ref is a
symref, or for unborn HEAD) + ` peeled:<oid>` (when `peel` requested and `Peeled != ""`). Flush-pkt ends
the response.

## 7. Receive-pack server flow (§7.5), in order

The HTTP handler (doc 06) does placement/drain gates and the `.git` URL suffix check FIRST (non-`.git`
URL → pkt-line refusal before any work), takes the per-repo `server.max_concurrent_per_repo` semaphore,
then calls this flow. Request/response may be gzip `Content-Encoding` (handled in doc 06); git responses
carry the no-cache header triple (doc 06).

1. **Parse the request** from the full body: a leading `shallow <oid>` section (a push from a shallow
   clone) is REFUSED with `git.ErrShallowPush` — receive-pack v1 does not advertise `shallow`, so the
   exchange cannot succeed, and the refusal names the cause and the remedy (`fetch-depth: 0` /
   `git fetch --unshallow`) instead of a bad-command-line protocol error (closing-audit Observation 2;
   see the Decisions entry). Otherwise: first pkt-line `<old> <new> <ref>\0<caps>`, then further
   `<old> <new> <ref>` lines until flush; caps parsed:
   `report-status`, `report-status-v2` (parsed but option lines deliberately never emitted, §7.8 step 8),
   `side-band-64k`, `atomic`, `quiet`, `push-options`, `ofs-delta`, `agent=`, `object-format=` (validate
   against repo format; mismatch → refuse). If the client sent `push-options` (and we advertise it):
   the next section is push-option lines until flush. The REMAINDER of the body is the raw pack bytes
   (may be absent for a pure delete push).
2. **Ingest** the pack (§3): `thin=true`, `fsck=wal.fsck_objects`, `max_bytes=server.max_push_bytes`.
   Failure → `unpack ng <msg>` and skip to step 8 emitting per-ref `ng` refusals (with sideband, the
   message goes on band 2 FIRST, then the report).
3. **Connectivity** (when `wal.check_connectivity`): see below.
4. **Policy** (§14, `internal/policy`): classify each update (create/update/delete; force = non-FF
   determined via `git merge-base --is-ancestor <old> <new>` after ingest — exit 0 = fast-forward; exit 1
   = force; other = treat as force-with-verification-needed → force). Disallowed refs →
   `ng "rejected by rule '<name>'"`.
5. **Publish only the allowed subset** via the WAL (doc 05, group-committed, `new_peeled` filled per
   §6.3 of the spec); per-ref results from policy + verify overwrite by name. If none allowed → report
   and done.
6. **Report status** (step 8 below).

### 7.1 Connectivity — DECISION: subprocess pipeline, not an in-process walker

The Rust implementation walks with gix. **Go uses stock git semantics:**

```
git rev-list --objects --stdin --not --all        # in the repo git-dir
  stdout | git cat-file --batch-check             # existence check for every listed oid
```

- Tips (non-zero new oids) are written one per line to rev-list's stdin (`--stdin` avoids ARG_MAX);
  `--not --all` hides history reachable from existing refs — exactly the spec's
  `stop_at_existing_refs` (`hidden = existing ref tips`).
- `rev-list --objects` must LOAD every tree/commit it traverses, so a missing tree/commit makes rev-list
  itself fail (non-zero exit, `unable to read`/`missing` on stderr) → `ErrMissingObject`.
- `cat-file --batch-check` is fed rev-list's stdout through an `io.Pipe` and reports `<oid> missing` for
  any missing tree/blob; missing oids (retained up to 16) → `ErrMissingObject{Oids}`. `gitlink` (submodule)
  entries are skipped by matching the mode in rev-list's `--objects` output line format
  (`<oid> <path>` — filter: lines whose path component has no type check possible; simplest: treat
  `missing` submodules by ignoring oids whose path is a recorded gitlink — rev-list marks them; accept
  rev-list's failure as the only commit/tree error source). Peel tips first: each tip oid is verified to
  exist via the same batch-check stream before the walk (`peel tips, verify existence`).
- Per-ref attribution: on joint failure, re-run the pipeline once per tip (rare path, bounded by ref
  count) to name the offending ref → per-ref `ng "connectivity: missing <oid>"`.

### Subprocess
Two processes, one pipeline, via `Pool.Run`. Discipline: rev-list's stdin written by a feeder goroutine
(closes on tips-EOF); `io.Pipe` connects rev-list stdout → cat-file stdin (the copier goroutine closes
the write end); the caller drains cat-file stdout, then `Wait`s rev-list first (its exit status carries
the walk verdict), then cat-file. Timeout: the request context plus a hard cap of `git.connectivity_timeout`
(default 300 s). stderr of both → bounded buffers for error text.

### 7.2 Report status emission (normative)

Body: `unpack ok` or `unpack ng <msg>`, then per-ref `ok <ref>` / `ng <ref> <reason>`. **Plain
report-status lines are emitted EVEN WHEN report-status-v2 was negotiated** — an `option atomic` line
after an `ng` confuses clients, and a rejected atomic txn must be plain `ng` per command (§20 item 12).
Encoding: pkt-lines; then, when the client requested `side-band-64k`, the ENTIRE report is wrapped in
band-1 frames (≤ 65516-byte payload chunks), an empty band-1 frame is NOT sent, and the message text of
failures was already emitted on band 2 before the report. Ends with flush-pkt. `quiet` suppresses only
band-2 progress chatter, never the report.

### Concurrency
- **Hazard:** the whole flow holds the per-repo ingest lock while streaming a 64 GiB push; ref cache
  patched concurrently with a snapshot load from a Serve sync.
- **Avoidance:** lock order is ingest lock → (policy, publish) → WAL sync → ref cache write. The body is
  streamed to a temp file BEFORE the ingest lock is taken when possible (buffering rule from doc 06), so
  a slow client does not hold the lock. Ref cache patches are sub-millisecond under the write lock.
  Everything blocking (ingest, connectivity, `merge-base`) runs in `Pool.Run` with contexts derived from
  the request — a client disconnect cancels all of it. `13_concurrency.md` §"lock ordering", §"drain".

## 8. Upload-pack (§7.6)

### 8.1 Engine selection — DECISION (Go v1)

| `git.upload_pack_engine` | Rust behavior | spindle Go v1 |
|---|---|---|
| repo base is remote-served (no store mount) | gix engine + remote-reader faulter | **refuse** (see below) |
| `auto` (default) | stock git | stock git |
| `git` | stock git | stock git |
| `gix` | gix | stock git + WARN log (config accepted for bucket compatibility; in-process engine deferred) |

**Refusal (remote-served base):** pkt-line
`ERR walgit: <owner>/<repo> is served through a remote base that is not mounted on this host; fetch from the serving host, or set cache.store_mount so the base is local (this spindle build has no remote-reader fetch engine)`
followed by flush, HTTP 503 + `Retry-After: 15`. The engine field is kept in config and surfaced in
`/services/api/instance` so a future in-process engine slots in without config churn (doc 14).

### 8.2 Stock git spawn

```
git -c uploadpack.allowSidebandAll=true upload-pack --stateless-rpc .
```

with `GIT_DIR=<repo>`, `GIT_PROTOCOL=version=2` or `version=0` (matching the negotiated version), env
`GIT_TERMINAL_PROMPT=0`, on `Pool.Run`. stdin = request body (streamed), stdout = response
(`Content-Type: application/x-git-upload-pack-result`, no-cache triple — doc 06). Discipline: close stdin
after EOF, drain stdout to the client writer concurrently, drain stderr to a bounded buffer, then Wait;
stderr content on non-zero exit → `Subprocess error` (500-shaped; doc 06).

For v2 the request body is parsed (pkt-lines) BEFORE spawning for the guards below, but is passed
through **byte-for-byte** — spindle never re-encodes client pkt-lines (only v0 advertisement and ls-refs
are hand-rendered). If a future need requires building a v2 request, the arg grammar is: `command=fetch`,
`thin-pack`, `ofs-delta`, `no-progress`, `include-tag`, `sideband-all`, `wait-for-done`, `filter <f>`,
`want <oid>`, `have <oid>`, `shallow`, `deepen`, `deepen-since`, `deepen-not`, `want-ref <ref>`, `done`.

**max_wants guard:** when `git.max_wants > 0`, count `want <oid>` lines (v0 and v2 alike) while parsing;
exceeding the cap → pkt-line `ERR walgit: fetch wants more than git.max_wants=<n> objects; use --sparse or --no-checkout for blobless/partial workflows`
+ flush, no git spawn.

**D17 forcing (`bundles.require`, fetch path):** an **unbounded zero-have** fetch (no `deepen*`, no
`filter`, no `have`) of a repo listed in `bundles.require` is refused with the exact fix in the error
text: `ERR walgit: <owner>/<repo> requires bundle-uri clones; use bundle-uri (pass -c transfer.bundleURI=false for shallow/CI fetches)`.
Bounded zero-have fetches (CI `--depth`/`--filter`) and all fetches with haves proceed. **One-shot
fallback:** a principal that fetched this repo's `bundles/list` within the last hour demonstrably tried
bundle-uri → ONE upload-pack full clone per 6 h with a loud band-2 `WARNING ... bundle-uri fallback clone`
frame; the next one and everyone else is refused. State: per-principal LRU (principal, repo, last-grant)
in `internal/bundle` (doc 08 owns the ledger; this package queries it).

### Subprocess
`upload-pack` lifetime = request lifetime: the request context cancels it (client disconnect → SIGKILL →
scratch/pipes cleaned by defer). No internal timeout beyond `server.request_timeout` (doc 06 middleware).
Feeder goroutine streams the body into stdin and closes; the handler copies stdout to the response
writer as it arrives (git's own band framing provides the flush boundaries; no re-buffering). stderr
drained to a bounded buffer on a goroutine that exits at Wait.

## 9. Repack, commit-graph, history pack (§7.7)

All maintenance commands run in the repo git-dir (or scratch as noted), `Pool.Run`, 1800 s timeout,
under the per-repo pack_mutex (doc 05/10). Exact argv:

- **Geometric compaction (maintainer):**
  `git repack -d --geometric=<factor> --write-midx [--write-bitmap-index] [--keep-pack <name>]…`
  (keep-pack for the base and every history pack that must survive). Diff the `objects/pack/*.idx` set
  before/after → new packs + removed packs (returned to the caller for store reconciliation).
- **Full repack (base rebuild):** `git repack -a -d --threads=0 --write-bitmap-index --write-midx
  [--keep-pack …]`; delete stray `*.keep` markers first.
- **Split commit-graph:** `git commit-graph write --reachable --split=replace [--changed-paths]`
  (`git.commit_graph` / `git.commit_graph_changed_paths`). Identify the last chain layer by its trailing
  checksum (the file name under `objects/info/commit-graphs/`); copy it out as the `wal/<checksum>.commit-graph`
  side-file (store key, doc 02). On a reader, install the side-file as the chain BASE:
  `objects/info/commit-graphs/graph-<hash>.graph` + `commit-graph-chain` naming only it.
- **Incremental fold (downloaded packs):** `git commit-graph write --split --stdin-packs` fed, one per
  line on stdin, the **pack names** under `objects/pack` (`pack-<checksum>.idx`; a bare
  `<checksum>.idx` is resolved relative to `objects/pack` and fails "error adding pack"), then close
  stdin, drain, Wait. The fold is best-effort: a failure logs a warning and never fails the push.
- **History pack (D18, when `git.history_pack`):** pipeline
  `git pack-objects --filter=blob:none --revs --delta-base-offset --stdout -q` (all ref oids on stdin)
  piped into `git index-pack --stdin` in a scratch with alternates → rename to
  `pack-<sha>.pack/.idx/.rev` + a `.history` marker file naming the base. Store kind = HISTORY,
  `derived_from=<base>`. A **history midx**: `git multi-pack-index write --stdin-packs
  --preferred-pack=<history idx>` covering history packs + installed bases; removed when no history packs
  exist. History packs never supersede anything; they are superseded with their base (doc 10).

### 9.11 Push-payload commit listing (events bridge)

The events bridge's `repo-hooks` sink enriches a push/ref event with real commit summaries
(docs/features/06 §5.3.1). Exact argv, `Layer.LogRange` (`internal/git/log.go`), run in the repo
git-dir, 30 s timeout, `Pool`-accounted:

```
git log --no-renames --max-count=<limit> --format=%H%x00%an%x00%ae%x00%aI%x00%s%x1e <new> [--not <old>]
```

- `<limit>` is the caller's bound (default 50, hard cap 200; `notify.MaxPushCommits` is the payload
  bound, which is the production caller).
- `--not <old>` is omitted when `old` is empty or the all-zero OID (a create): the listing is then the
  newest `<limit>` commits reachable from `<new>`, which is what a bounded create payload can honestly
  say.
- Records are `<oid>\0<author>\0<author-email>\0<author-date>\0<subject>\x1e`; the subject only (a
  full message can exceed the delivery-body cap). An unparseable record is skipped.
- Every error degrades to "no commit summaries" at the caller — payload enrichment never fails a
  webhook delivery.

## 10. Bundle creation primitives (§7.8)

- **Stock create:** `git bundle create <out> --stdin` fed `<ref>` lines and `^<oid>` excludes
  (one per line, stdin closed after). Blocking, `Pool.Run`. Returns size + the byte offset of the
  `PACK` magic — scanned with a 3-byte overlap between chunks (magic may straddle a boundary); offset
  = header/pack split used by composition (doc 08).
- **gix variant:** N/A in Go v1 (no in-process engine) — composed bundles are always header ∘ existing
  pack bytes (below).
- **Header rendering (no git):** `# v2 git bundle\n` + `-<oid> \n` per prerequisite + `<oid> <name>\n`
  per ref + `\n`. (Exact bytes: `-` + oid + one space + `\n`.) A **full bundle** = header ∘ an existing
  pack's bytes — this is how composed weeklies and import bundles are built with zero pack bytes through
  the host. `BundleHeader(refs, prereqs)` is pure and unit-tested byte-for-byte (doc 15 fixtures).

## 11. Upstream-git helpers (repair and follow) (§7.9)

Config: `upstream.git` (source URL), `upstream.lfs`, `upstream.token_env` (env var NAME holding the
token — never the token), `upstream.follow`. Credentials are passed as an **inline config-pair credential
helper**, never on argv or the environment of git itself beyond that env:

```
-c credential.helper= -c credential.helper=!f(){ echo username=x-access-token; echo password=$WALGIT_UPSTREAM_TOKEN; };f
```

(the empty helper first clears inherited helpers). `WALGIT_UPSTREAM_TOKEN` is set in the child env from
the named env var; `GIT_TERMINAL_PROMPT=0` always. For LFS Basic auth: `x-access-token:<token>`.

### 11.1 repair (`fetch_objects_as_pack`)

Scratch bare repo; fetch missing oids in **500-oid batches**:

```
git -c fetch.negotiationAlgorithm=noop -c protocol.version=2 fetch --no-tags --no-write-fetch-head --quiet --depth=1 <upstream> <oid>…
```

then `git pack-objects --no-reuse-delta --compression=6` over exactly the requested oids (no closure);
verify EVERY requested oid is present in the resulting idx (`git verify-pack -v` or idx lookup via the
fanout + binary search — idx lookup preferred, no subprocess); a refused want is an error, never a silent
hole. Publish via add-pack (tier 0, doc 05).

### 11.2 follow (`fetch_refs`)

Persistent scratch `<cache.dir>/follow/<owner>/<name>.git` with `objects/info/alternates` → the serving
objects dir. Set `refs/follow/<ref>` to the WAL values via `git update-ref --stdin` (§4.3 grammar); then:

```
git -c fetch.unpackLimit=1 -c transfer.unpackLimit=1 -c fetch.writeCommitGraph=false -c gc.auto=0 -c protocol.version=2 fetch <upstream> +<ref>:refs/follow/<ref>…
```

Read tips back via `git for-each-ref refs/follow/` (`--format=%(objectname) %(refname)`); the fetched
pack is discarded after ingest (the scratch's packs are trash — alternates make objects resolve anyway).
Scheduling cadence `maintenance.follow_interval` and the D33 loop live in doc 10.

### Subprocess
Both helpers run under `Pool.Run` with a 900 s timeout per fetch batch; stdin/stdout discipline as §2;
`credential.helper` argv order is significant (clear-then-set). The follow scratch is shared per repo —
serialized by the same repo mutex as maintenance (doc 10 owns it).

### 11.3 Source archives (`git archive`, PLAN-V1 WP3.1)

Served by `internal/archive` (`GET|HEAD /{o}/{r}/archive/<ref>.tar.gz|.zip`), one streamed subprocess per
request under a dedicated bounded pool (never the control-plane pool):

```
git rev-parse --verify --quiet --end-of-options <ref>^{tree}     # tree id ⇒ ETag
git archive --format=tar --prefix=<repo>/ <ref>^{tree}           # .tar.gz lane (plain tar; gzipped in-process)
git archive --format=zip -0 --prefix=<repo>/ <ref>^{tree}        # .zip lane (-0 = store, no deflate)
```

The `.tar.gz` lane pipes the tar stream through a `compress/gzip.Writer` as it arrives (no whole
archive is buffered): git produces the reproducible tar bytes, Go compresses them. The response is a
real gzip stream (`1f 8b`), and decompressing it yields bytes byte-identical to `git archive
--format=tar` run locally against the same tree (pinned by `internal/archive`'s test, which also
compares the tar entry listing). Naming either lane `.tar.gz` without the gzip stage is a bug —
every archive tool opens the body by extension.

`<ref>` is a branch, tag, or full 40/64-hex commit; slashes in refs arrive as extra URL path segments and
are re-joined before resolution; the ref is validated (no leading `-`, no refname metacharacters, no
`..`/`@{`/`//`) and `--end-of-options` closes the option-injection hole. The ETag is
`"<tree-id>-<format>"`, so two refs naming the same tree revalidate against each other. Tar output is
byte-identical to `git archive` run locally against the same tree (commit mtime is the entry mtime) —
before the gzip stage. Zip output is also byte-identical to `git archive --format=zip -0` locally on the
same git build; `-0` is the one flag difference from a hand-run `git archive --format=zip` (which
deflates), chosen so the bytes do not depend on the zlib compressor version and the server spends no CPU
compressing an archive that is already incompressible at the transfer layer. Streaming means no
`Content-Length` and no `Range`: archives are generated, not stored (law 4 — a derived stream is not
state).

### 11.4 Tag listing (`for-each-ref`, PLAN-V1 WP4.1)

The `/tags` page and `GET …/api/tags` read one spawn per request:

```
git for-each-ref --sort=-creatordate \
  --format=%(refname:strip=2)%09%(objectname)%09%(*objectname)%09%(creatordate:iso-strict)%09%(authorname)%09%(taggername) \
  refs/tags
```

Fields are tab-separated (refnames cannot contain tabs). `%(*objectname)` is empty for a lightweight tag
(the target is `%(objectname)`); for an annotated tag it is the peeled commit and `%(objectname)` is the
tag object. `creatordate` is the tagger date for annotated tags and the commit date for lightweight ones;
`authorname` follows the same rule (annotated tags carry an empty `authorname`; the `taggername` field is the fallback). Tag deletion is `refs/tags/<name>` old → zero through the ordinary WAL
REF_UPDATE publish (CAS old-oid; a moved tag is a 409, never a blind delete).


## 12. Import clone + scratch ingest (docs/features/10)

The import task (`internal/repoimport`) clones the source into task
scratch, then hands packs/refs to the existing publish path. Exact argv
(all spawns in the feature's own bounded pool with ctx timeouts —
`import.clone_timeout` 1800 s for the clone, `import.git_timeout` 300 s
for the rest; `GIT_TERMINAL_PROMPT=0` always; token via child env only):

```
git [-c credential.<scheme>://<host>.helper= -c credential.<scheme>://<host>.helper=!<helper>] clone --mirror --progress -- <url> <dir>
```

(the credential `-c` pairs are present only for token sources, built
dynamically per spawn with a per-task env name — static copy-paste is
forbidden; the helper is host-pinned so redirects can't harvest the
token; the empty-helper-first clear-then-set order from §11 applies).

```
git --git-dir=<dir> for-each-ref --format=%(objectname) %(*objectname) %(refname)
git --git-dir=<dir> rev-parse --show-object-format
git index-pack <pack>                                   # ONLY when the source pack lacks a sibling .idx
git show HEAD:.gitattributes                            # LFS probe (best-effort; unborn HEAD / no file = skip)
git --git-dir=<dir> cat-file -e <oid>                   # servability probe (issue #320): exit 0 = object present
```

Refnames cannot contain spaces (§4.3 validation), so the three
for-each-ref fields split unambiguously on the first two spaces
(`%(*objectname)` renders empty for non-tags). The `.idx` discipline is
load-bearing: `AddPack` installs/uploads the `.pack` only, so the
importer installs each sibling `.idx` into the serving copy BEFORE
`AddPack` (its internal `LevelServe` Sync needs it locally) and uploads
it to `wal/<checksum>.idx` create-if-absent after (a fresh instance
materializes from the store alone). A regenerated index's stray `.keep`
is swept (same rule as §9). The mirror sync/heal loop
(`docs/features/11_mirror.md` §3/§3.1) reuses this section's argv
(`clone --mirror`, `for-each-ref`, `rev-parse`, `index-pack`) plus
`merge-base --is-ancestor` (ff rule) and the `cat-file -e` probe above
(any non-zero exit = unservable — a verdict, never a retry).

### Concurrency

Hazard: a clone holding a pool slot forever; progress parsing blocking
on a pipe git never closes. Avoidance: `Pool.Run` + `CloneTimeout` ctx
(`exec.CommandContext` SIGKILLs git); stderr drained by a dedicated
goroutine to EOF; a 15 s heartbeat ticker fires while git is silent (no
silent spinners — law 7).

## 13. Config keys consumed by this package

| Key | Default | Used in |
|---|---|---|
| `git.binary` | `"git"` | every exec (§2) |
| `git.upload_pack_engine` | `"auto"` | §8.1 (`gix` → stock + WARN) |
| `git.max_wants` | `0` | §8.2 guard |
| `git.allow_filter` / `git.allow_any_sha1_in_want` | `true` / `false` | advertisement path (init always sets allowAnySHA1InWant=true — §6.1 note) |
| `git.object_format` | `"sha1"` | §1.2 init |
| `git.commit_graph` / `git.commit_graph_changed_paths` | `true` / `false` | §9 |
| `git.history_pack` | `true` | §9 |
| `git.ingest_timeout` / `git.connectivity_timeout` *(new)* | `600s` / `300s` | §2, §3, §7.1 |
| `git.max_git_procs` *(new)* | `4 × GOMAXPROCS` | §2 pool |
| `server.max_push_bytes` | `64GiB` | §3.2 |
| `wal.fsck_objects` / `wal.check_connectivity` | `true` / `true` | §3.2, §7.1 |
| `server.max_concurrent_per_repo` | `64` | taken by HTTP layer before the handler |
| `cache.dir`, `cache.ref_advert_entries` | `/tmp/walgit`, `256` | §1.2, §4.1 peel cache |
| `bundles.require` | `[]` | §8.2 D17 |
| `upstream.*` | — | §11 |

New keys (`git.ingest_timeout`, `git.connectivity_timeout`, `git.max_git_procs`) are Go-only additions —
listed for doc 11; Rust-compat keys keep their names verbatim.

## 14. Threads workflow reads (docs/features/15, feature-owned)

`internal/threads` reads workflow files at a commit from the repository's serving copy (no
worktree, no checkout, read-only). Exact argv (run in the synced git dir, `GIT_DIR` set, ctx
timeout, bounded stdout — 1 MiB + 1, stderr tail 8 KiB, own bounded semaphore of
`4 × GOMAXPROCS`; a non-zero exit is the not-found class, an exec/timeout failure is 503):

```
git ls-tree -z --name-only -r <commit> -- .spindle/workflows
git show <commit>:<path>
git rev-parse --verify --quiet <ref>^{commit}
```

`-r` is required: `ls-tree` on a directory prints the directory itself, not its children. Only
direct children of `.spindle/workflows` with `.yml`/`.yaml` extensions are parsed (nested files
and other forges' directories are ignored). The `<commit>:<path>` form rejects paths containing
`:` (ambiguous with the tree-ish separator). These calls are control-plane, event-rate reads —
never on a git hot path — and they never write a ref, object, or worktree.

## 15. Server-side commit plumbing (browser file edit, docs/features/17)

`internal/webedit` mints one commit from plumbing only — no worktree, no branch movement, no checkout —
in the repo's serving copy (`internal/webedit/git.go`). Every command runs under the bounded pool with
`GIT_DIR=<repo>`, `GIT_TERMINAL_PROMPT=0`, a per-command timeout and request-ctx cancellation. The
index file is a **request-scoped temporary file** (`GIT_INDEX_FILE` under a unique scratch dir removed
on every exit path); `GIT_WORK_TREE` points at that empty scratch dir because
`update-index --force-remove` refuses to run in a repo without a work tree at all.

```
git rev-parse --verify --quiet refs/heads/<branch>^{commit}      # resolve the base
git ls-tree -z <commit> -- <path>                                # entry mode/type/oid
git cat-file -s <oid>                                            # size (binary/LFS guard input)
git cat-file blob <oid>                                          # content (read side)
git hash-object -w --stdin                                       # the new blob
git read-tree <base>                                             # temp index = base tree
git update-index --add --cacheinfo <mode>,<blob>,<path>          # create/edit
git update-index --force-remove -- <path>                        # delete
git write-tree                                                   # the new tree
git diff-tree --quiet <base> <tree>                              # exit 1 = changed, 0 = no-op (422)
git commit-tree <tree> -p <base> -m <message>                    # author=principal, committer=server
git diff-tree -p --no-color --no-commit-id <base> <tree>         # preview only (writes nothing)
git for-each-ref --format=%(refname)                             # pack exclusion list
git pack-objects --revs --stdout                                 # stdin: <tip> + ^<ref> per existing ref
```

Notes pinned by tests:

- `commit-tree` runs with `GIT_AUTHOR_NAME/EMAIL/DATE` = the signed-in principal and
  `GIT_COMMITTER_NAME/EMAIL` = `spindle <spindle@localhost>`, both at the same wall-clock instant
  (`<unix> +0000`; git rejects a sub-8-digit stamp).
- `pack-objects` takes no `--not`/`--all` flags; caret negation rides stdin (the merge-task precedent,
  §9). The resulting pack is thick, self-contained and O(KiB–MiB): one blob, its rebuilt ancestor trees,
  and one commit. It is ingested with `Layer.Ingest` (complete, fsck on) and published in the same WAL
  entry as the ref txn.
- `diff-tree --quiet` (rather than the plan's `diff-index`) needs no index and no work tree: it compares
  two trees directly, which is exactly the no-op question.
- A delete of the last file in a directory drops the directory entry itself (write-tree does that
  naturally); deleting the repo's only file produces a valid empty tree — allowed, not refused.
- Paths are validated before any argv is built (no `..`/`.git` segment, no leading `-`), and revs are
  sha-resolved first, so no user string is ever parsed as an option.

## 16. Repository browsing reads (server-rendered pages, docs/go/12)

The Blame/Branches/Compare pages read the repository through **one optional seam**,
`api.RepoBrowse`, implemented by the same `walView` that backs `api.RepoView` and running in the same
materialized serving copy under the §2 discipline (`GIT_DIR`, `GIT_TERMINAL_PROMPT=0`, request ctx,
`git.binary`). These are event-rate page reads, never push/clone hot paths; they write nothing (no ref,
object, worktree or index), so law 4 holds — attribution and comparison are derived renders.

### 16.1 Per-line blame

```
git cat-file -s <commit>:<path>                              # size (same 2 MiB guard as §9.5)
git blame --line-porcelain -L 1,10001 <commit> -- <path>     # attribution + content
```

`--line-porcelain` (NOT `--porcelain`) is deliberate: every line record repeats the full commit header,
so attribution never depends on group state. A grouped stream puts commit metadata on a group's first
line; an unchanged trailing line in a later group, a CRLF file, or a merge commit then require
carry-over parsing, which is exactly where a per-line renderer drifts. The cost (a few hundred bytes
per line) is bounded by `-L` and the 2 MiB cap.

`<commit>` is the sha `Repo.Resolve` already returned — never a user string — and the path follows `--`.
The `-L 1,<cap+1>` bound asks for one extra line so a truncated render is observable without a second
read; the page states the cap. `git blame` runs under a 20 s ctx timeout. Binary detection reuses git's
NUL-in-first-8KiB heuristic over the attributed lines, so the page refuses with the blob page's exact
language.

### 16.2 Branch overview (list, create, delete)

The Branches page reads every branch in ONE spawn — the per-branch rev-list shape would be N
processes for N branches:

```
git for-each-ref --sort=-committerdate --count=501 \
  --format=%(refname:strip=2)%09%(objectname)%09%(committerdate:iso-strict)%09%(ahead-behind:<default-sha>)%09%(authorname)%09%(subject) \
  refs/heads
```

`%(ahead-behind:<committish>)` is evaluated by git inside the one enumeration, so ahead/behind costs
no extra spawn. It takes the default branch's **resolved sha**, never a ref name: refnames may contain
`%` and parentheses, and the value lands inside a format string. The subject is the LAST field because
a commit subject may itself contain tabs (refnames cannot). A git build without the atom is retried
once without it; the page then renders "—" (`ahead_known:false`) instead of a fabricated zero.

Writes go through the WAL publish funnel (never a direct `update-ref`), one `REF_UPDATE` txn each,
CAS'd by the manifest verify step:

```
git rev-parse --verify --quiet --end-of-options <from>^{commit}   # create's start commit
# create: refs/heads/<name> zero-oid -> <commit>
# delete: refs/heads/<name> <resolved-sha> -> zero-oid
```

No new pack rides either txn: the starting commit's objects are already published (a create adds no
object), and a delete adds none. The delete path refuses the default branch (the `HEAD` target from the
manifest snapshot) and any ref the stored policy protects against `delete`, evaluated with
`policy.EvaluateProtect` over the SAME `policy.json` object the Settings → Branch protection tab
renders — no second protection rule exists. Writer commits/deletes do not run the on-push fan-out
(they introduce no commits; workflows fire on pushes, not ref bookkeeping).

### 16.3 Ref-to-ref comparison

```
git rev-list --count --left-right <base>...<head>       # "<base-only> <head-only>"
git merge-base <base> <head>                            # common ancestor (diff base)
git log --format=<§9.11 gitFmtLog> --max-count=<n+1> <head> --not <base>
git diff --numstat -z --no-renames <diff-base> <head>   # change summary
git diff --no-color --no-renames <diff-base> <head>     # unified patch
```

Every argument is a resolved hex object id (the page resolves both refs through `Repo.Resolve`
first), so no user string can be option-shaped. `--no-renames` matches the commit renderer, which is
also what parses and draws the patch — the compare page never owns a second differ. The diff base is
the merge-base (what a merge of head would bring); when the histories share no ancestor, git exits
non-zero from `merge-base` and the diff falls back to `<base> <head>` with the page stating that the
difference is the full tree difference, never pretending a shared root.

The commit list is head-only (`--not <base>`, capped at 250 with `more`) and ahead/behind is the
same triple-dot count git prints (left = base-only, right = head-only). The patch is capped at 2 MiB
by bytes with an explicit truncation notice; the stats are `--numstat` (a binary file is `-1/-1`, the
§9.6 rule).

### 16.4 Repository Insights scan

The Insights page never runs git on a request path: it reads the cached
`repos/<o>/<r>/insights/pulse.json` projection (`docs/features/18_repo_insights.md`). The
`insights-refresh` task makes ONE history walk for every commit-derived fact, plus ONE tree listing
for the language mix (§16.5):

```
git log --no-renames --no-ext-diff --no-textconv --no-show-signature --no-decorate \
  --max-count=<cap+1> --since=<RFC3339 UTC> \
  --format=%x1e%H%x00%an%x00%ae%x00%cI --shortstat <resolved-head-sha>
```

- `<head>` is a resolved hex oid (the serving copy's default-branch tip), never a ref name.
- `--since` is the earliest bucket start of the widest (365-day) window; exact per-window filtering
  happens in Go.
- One extra commit is requested so truncation is observable without a second read (the cap defaults
  to 20,000; a truncated window is stated in the UI).
- `--no-ext-diff`/`--no-textconv` stop repository config from spawning helpers or changing the
  content diffed; `--no-show-signature`/`--no-decorate` keep the record stream clean.
- A record is `0x1e`, then `%H %an %ae %cI` NUL-separated, then the optional `--shortstat` block.
  The parser splits on NUL (names may contain newlines) and skips malformed records; a hard
  subprocess failure fails the refresh (no half-truth projection is written).
- Merge commits appear as commits with no shortstat block (zero line changes); binary-only changes
  also contribute zero. Cost: linear in commits, one process (E20).

### 16.5 The language-mix listing (the Code pulse card)

The same refresh makes ONE tree listing for the language mix, against the serving copy and resolved
head it already holds:

```
git ls-tree -r -l -z <resolved-head-sha>
```

- `<rev>` is the resolved hex oid, never a ref name.
- `-r` recurses (trees are not emitted as rows), `-l` adds the blob size, `-z` NUL-terminates so a
  path containing anything but NUL parses. A record is
  `<mode> SP <type> SP <oid> SP <size> TAB <path> NUL`; the size is `-` for a gitlink.
- Only regular blobs count (`100644` / `100755`). Symlinks (`120000`) and gitlinks (`160000`) are
  skipped, and an unparseable record is ignored rather than failing the refresh.
- Entries are grouped by a pinned extension→language table (`internal/git/languages.go`: Go,
  JavaScript, TypeScript, CSS, HTML, Shell, Python, Rust, Java, C, C++, Ruby, PHP, JSON, YAML,
  Markdown, SQL, Dockerfile, Makefile; everything else `Other`). This is a **committed-bytes
  estimate**, not a line counter: vendored trees, lockfiles and generated files count by size.
- The tally caps at 50,000 regular blobs (`InsightsLanguageScanMax`); a tree past the cap is
  reported capped and the UI says the mix is bounded. The listing streams through a counting writer,
  so the cap bounds the count without buffering the whole output.
- A hard subprocess failure fails the refresh (no half-truth projection is written).

## 17. Code search: the grep primitive (`docs/features/20_search.md`)

`Layer.Grep` / `Layer.GrepAt` run ONE `git grep` over the repository's already
materialized serving copy at a **resolved object id**. Search is a read: it
writes no ref, object, worktree or index, adds no bucket key, and runs no
LIST. It is the engine decision of `20_search.md` §2 (grep now; an index only
when a named trigger fires).

### 17.1 The exact argv

```
git grep --no-color -n --column -I -z --no-textconv --no-recurse-submodules \
  [-i] [-w] [-F|-E] -m <perFile+1> -e <pattern> <rev> -- [<pathspec>...]
```

- `<rev>` is a **resolved 40/64-hex oid** (never a ref name or other user
  string); `--` always precedes the pathspec list, even when it is empty.
- `-n --column` emit the 1-based line and the 1-based **byte** column; `-z`
  makes the record NUL-delimited so paths containing spaces, colons and
  newlines parse. Record shape (verified on git 2.53):
  `<rev>:<path> \0 <line> \0 <column> \0 <text> \n`. `<text>` excludes the
  line terminator: it runs to the `\n`, and the parser drops exactly one
  trailing `\r`, so a CRLF line's content is reported without it — the answer
  is deterministic regardless of the committed bytes or the git build (a
  fixture must still pin `core.autocrlf=false`; a developer's global config
  otherwise converts the blob and hides the case).
- `-I` skips binary blobs (git's NUL-in-the-first-8-KiB probe), matching the
  blob page. `--no-textconv` and explicit `-F`/`-E` stop repository config from
  spawning filters or changing the pattern dialect; `--no-recurse-submodules`
  is explicit even though it is the default.
- `-i` is the default (ASCII case-insensitive, `20_search.md` §5.1); a
  case-sensitive query omits it. `-E` selects POSIX ERE and is pre-validated
  with Go `regexp.CompilePOSIX`; `-F` is the fixed-string default.
- `-m <perFile+1>`: `git grep` caps per file silently, so asking for one extra
  match makes per-file truncation observable without a second run.
- Environment: `GIT_DIR=<serving copy>`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`
  (deterministic, ASCII-only folding; no inherited locale). `cmd.Dir` is the
  same directory. `exec.CommandContext` carries the caller's context plus the
  query's hard deadline (default 10 s), so a client disconnect or a timeout
  SIGKILLs the child.

**Exit codes:** `0` = matches; `1` = **no matches (success, empty result)**;
`≥ 2` = error. Treating `1` as an error is the classic bug this contract
forbids. A bad pattern is an error in practice — measured git 2.53 exits
`128` for `-e '('` — so the contract is the class (`≥ 2`), not the number. When
the pattern still fails at run time (a dialect corner the pre-flight RE2 check
accepted), the primitive returns `ErrGrepPattern` (a 400-class error), never an
empty result; any other non-0/1 exit is a subprocess failure (503-class).

### 17.2 A derived scan pool, never the control-plane pool

`Layer.ScanPool` (`git.GrepDefaultPoolSize` = 2 slots) is the **only** pool a
grep takes. `Layer.Pool` is for clone/upload-pack/ingest and is never used by
search: a broad grep is CPU-bound for up to the deadline, clone/upload-pack is
latency-sensitive I/O, and the 2026-08 incidents are what happens when bulk and
control-plane work share a semaphore (`13_concurrency.md` §7). A hand-built
`Layer` with a nil `ScanPool` falls back to a package default pool of the same
size rather than borrowing the control plane. The slot is held only while the
subprocess runs — never across a sync, a resolve or a store read.

### 17.3 Streaming parse and caps

The stdout stream is parsed **sequentially** (never `strings.Split` on NUL: a
text blob may contain NUL after git's binary probe), which also handles a record
straddling any write boundary:

1. path up to `\0`, strip the known `<rev>:` prefix (fall back to the first
   colon only if the prefix is absent);
2. line digits up to `\0`, column digits up to `\0`;
3. text up to `\n` — the raw line bytes with exactly one trailing `\r`
   dropped (the terminator, §17.1), then capped at 4 KiB per match and
   middle-ellipsized around the column on UTF-8 boundaries.

Caps are enforced **while parsing** (`20_search.md` §5.2): per-file (the extra
`-m` record), total matches, and stdout bytes. A cap cancels the subprocess
(the parser asks the runner to cancel) and sets `GrepStats.Truncated`; the
deadline is reported as `GrepStats.TimedOut` with the partial result and a nil
error (a deadline is an answer, not a failure). A caller cancellation is
returned as the context error. Output stays bounded: at most `Limit` matches,
each at most `MaxText` bytes, read from at most `MaxBytes` of stdout.

### 17.4 Tests and evidence (`internal/git/grep_test.go`)

Real bare fixtures (built by a worktree commit, then `git clone --bare`, so the
searched copy has no worktree): matches with exact line/column, a non-match
(exit 1), a run-time-rejected pattern (distinct error), unicode plus the pinned
ASCII-folding limitation, a binary blob skipped, a 60 KiB single line, CRLF, a
missing trailing newline, pathspec filtering, ref selection, a path containing
a colon and a newline, symlink content, a skipped submodule gitlink,
cancellation (mid-flight via a stub that blocks on a fixture marker, and while
queued for a pool slot), a deadline, the match/per-file/byte/text caps, the
control-plane-pool isolation test, and the argv pinned literally. The stub
`git` scripts stand in only for process-level behavior a real repository cannot
produce on demand. `grep.go` holds 100 % statement coverage. The cost harness is
`internal/devtools/searchbench`; its numbers live in `docs/EVIDENCE.md` E21.

## Decisions & deviations from the Rust design

- **Collaboration-layer fork→base bridge fetch (issue #456, owned by
  `docs/features/03_pull_requests.md` §7).** Cross-fork PR work runs git in
  the base serving copy, so fork-unique head objects are fetched from the
  fork serving copy on demand. Exact argv (run in the base git-dir):
  `git -c gc.auto=0 fetch --no-tags --no-write-fetch-head --quiet <forkDir> <sha>`.
  Full history (merge-base/trial-merge need whole ancestry), no tags, no
  FETCH_HEAD (ref-free, invisible to concurrent readers), gc disabled (a
  read-path bridge must never repack the serving copy). Fetch-by-sha is
  exact under a moved fork branch and fails loud under a force-pushed-away
  sha. Idempotent; base manifest untouched (serving-copy warmth only).

- **Connectivity via `git rev-list --objects --stdin --not --all | git cat-file --batch-check`** instead of
  the gix rev-walk: stock git gives exact `stop_at_existing_refs`/missing-object semantics with zero
  in-process walker code (dependency policy); per-ref attribution by bounded per-tip re-runs on failure.
- **No gix/upload engine at all in Go v1:** stock git for every fetch; a remote-served base (which Rust
  serves via the gix engine + remote-reader faulter) is refused with an explicit pkt ERR + 503/Retry-After
  naming the fix (`cache.store_mount` or fetch from the serving host); `gix` config value accepted and
  downgraded with a WARN. The Rust gix engine carried the 178 GB OOM and wrong-id thin-pack bug; deferring
  keeps v1 boring while the config seam (doc 14) preserves the slot.
- **v2 fetch requests passed through byte-for-byte** after parsing for guards (Rust sometimes rebuilds the
  request): no re-encoding means no encode bugs; guards only read.
- **v2 capability advertisement rendered by hand** (Rust spec implies engine-rendered): needed since
  ls-refs is ours while fetch is stock git's; advertised fetch features are the subset stock git accepts.
- **v2 report-status-v2 negotiated but never used for output; plain `ng` lines always** — carried over
  from §20 item 12 (behavioral truth), not a new deviation.
- **Ref cache pending-overlay uses copy-on-write slice swap under a RWMutex** instead of Rust's
  generation-keyed moka cache: stdlib-only, same O(k) view and O(n) patch, and the base snapshot's
  immutability is enforced by construction.
- **Peel cache via one persistent `git cat-file --batch` per repo** instead of gix object reads: avoids a
  subprocess per tag while staying stdlib; bounded by `cache.ref_advert_entries`.
- **Blocking pool is a semaphore-bounded goroutine pool (`git.max_git_procs`)** replacing tokio's 4-worker
  bulk runtime + spawn_blocking: one pool for all git execs; per-repo caps live in the HTTP layer.
- **Commit-date derivation is one light `log -1` (§4.5, Forgejo #247):** the
  bucket never carries commit dates, so the push-path hint and the sweep
  backfill both read them from the commit object via
  `git log -1 --format=%cI%x00%aI <sha>` (30 s, pool-accounted). Rationale:
  a single local object read — no history walk, no new config, no new pool —
  and every failure degrades to null activity (R1 B3: derivation never fails
  the push; the sweep heals).
- **Repository Insights is one `--shortstat` walk (§16.4, 2026-11-21):** the
  pulse projection needs commit counts, distinct authors and line totals over
  three nested windows, so the task runs one `git log --max-count=<cap+1>
  --since=<365d floor> --format=… --shortstat` and slices windows in Go.
  Rationale: the alternative (one walk per window) triples the spawn cost and
  can disagree about the head; one walk + the cached projection keeps a page
  read at zero git. Merge commits contribute zero lines and the cap/truncation
  is stated in the UI — no sampled tail is silently presented as complete.
- **New Go-only config keys** `git.ingest_timeout`, `git.connectivity_timeout`, `git.max_git_procs`:
  Rust hard-codes tokio timeouts/worker counts; Go makes them explicit for operators (documented in doc 11).
- **`git.binary` is plumbed everywhere** (unlike Rust, which hardcodes `"git"` — §20 item 5): one field on
  `Layer`, zero cost, closes the known discrepancy.
- **NEW (2026-09-08) — `git.PackChecksumFromIdx` is the single source of truth for idx-basename →
  bucket checksum (#205):** `PackDiff.New` basenames (`pack-<hex>.idx`) map to the bare trailing
  SHA (02_storage_protobuf.md §2.2) by stripping the `.idx` suffix plus one `pack-` infix layer.
  The three producers that each hand-rolled `TrimSuffix`-only (push, server import, CLI import)
  all call it now, so the on-disk infix can never reach the manifest again; single-strip (not
  strip-all) keeps doubled legacy names mapping to their stored legacy checksums instead of
  colliding with bare ones.
- **Pull-only mirror refusal at the push funnel (Forgejo #240, R1 (c)).**
  `pushPipeline` (the ONE function HTTP `receivePackLocal` and SSH both land
  in) checks the injected mirror predicate first: mirrors get per-ref `ng`
  lines with `this repository is a read-only mirror; pushes are rejected` and
  never reach ingest/connectivity/publish — the `IsManagedRef` shape, one
  funnel instead of three gates. The sync engine publishes via
  `Publish`/`PublishRefs` directly and never enters the pipeline, so it cannot
  refuse itself. Sync clones use the pinned `clone --mirror` argv (§12); the
  only fetch variant is the same argv with a host-pinned credential helper
  for token-bearing first/manual syncs (memory-only, never stored).
- **Annotated-tag construction argv (Forgejo #263, 2026-09-10, `internal/tags`
  + the `cmd/spindle` tags composition).** Two new pinned argv, both run in the
  repo git-dir (`GIT_DIR=<repo>`, `GIT_TERMINAL_PROMPT=0`, bounded pool + ctx
  timeout, 8 KiB stderr discipline per §2):
  `git mktag` (server-rendered tag content on stdin — `object/type/tag/tagger`
  headers, blank line, message — strict-fscks the tag body, writes the loose
  object, prints the tag oid) and `<oid>\n | git pack-objects --stdout`
  (plain oid list on stdin, no `--revs`; one object, no deltas, flags
  minimal). `mktag` is chosen over `git hash-object -t tag -w --stdin`
  precisely because `hash-object` performs no fsck: it would accept a
  malformed tagger line/date that later fails fsck on fetch or breaks
  `git show`. The pack feeds the existing `Layer.Ingest` path
  (`index-pack --stdin --keep --rev-index --threads=0 [--fsck-objects]`,
  `thin=false` — the pack is complete), and composition publishes it as a
  PUSH entry (txn create with `NewPeeled`) through the unchanged funnel.
- **Per-entry tree-date walk argv (Forgejo #301, 2026-09-10, recipe owned by
  `internal/api` — normative in 07_api.md §9.4, registered here per the
  exact-argv law):**
  `git log <sha> --format=SPINDLETREE\ %H\ %cI --name-status --no-renames --no-color --first-parent -z --max-count=<server.max_tree_log> [-- <dir>]`
  (run in the serving copy, request ctx, no extra pool — it rides the
  per-repo HTTP semaphore like every other §9 recipe). One invocation per
  tree listing, newest-first; `--first-parent` attributes a merge's whole
  merged diff; `--no-renames` lands renames as delete+add (no pair
  parsing); `-z` keeps path bytes exact (no C-quote misses). Output shape
  (verified byte-for-byte): `MARK <sha> <date>` NUL, `\n`, alternating
  1-char status / path NUL records per commit. Bound: `server.max_tree_log`
  (default 200, validated `>= 1`); failure degrades to undated entries,
  never a tree failure.
- **Servability probe argv (issue #320, 2026-09-11):** `git --git-dir=<dir> cat-file -e <oid>`
  (exit 0 = servable) joins §12's argv for the mirror sync/heal loop. Rationale: law 2 pins exact
  argv — a new spawn needs its doc line in the same change; `-e` (existence only, no output) is
  the cheapest object proof.
- **Push-mirror transfer argv (Forgejo #623, 2026-09-16,
  `internal/pushmirror` Runner + `ListRefs`).** One pinned transfer
  shape, run in the serving copy (`GIT_DIR` unset — `cmd.Dir=<repo>`,
  pool + ctx timeout, 8 KiB scrubbed stderr discipline per §2):
  `git [-c credential.<scheme>://<host>.helper= -c credential.<scheme>://<host>.helper=!<helper>] push --prune -- <url> +<ns>/*:<ns>/* [...]`
  (the credential `-c` pairs are present only for password/token pushes,
  built dynamically per fire with per-fire env names — the §11
  clear-then-set order and host pinning apply; the helper echoes
  `username=$USERENV` + `password=$SECRETENV` so neither argv nor logs
  carry secrets — the username rides env too because the `!` helper
  runs through a shell and the username is user-controlled text).
  The refspecs are rendered per fire from a local enumeration (the §12
  `for-each-ref` argv): every surviving namespace as a forced wildcard
  (`refs/heads` + `refs/tags` unconditionally, so an emptied namespace
  prunes upstream; other surviving namespaces only when populated;
  bare two-segment names as exact forced refspecs). Dropped before
  rendering (the S4 refmap, pull-direction FilterRefs discipline
  reversed): `refs/replace/*`, `refs/meta/*`, `refs/keep-around/*`
  always;   `refs/pull/*`, `refs/changes/*`, `refs/review/*` (spindle's
  own PR heads live in `refs/pull/` — shipping them would leak forge
  state, and hosts like GitHub refuse writes there), `refs/notes/*`
  by default. SSH pushes set `GIT_SSH_COMMAND="ssh -i <keyfile> -o
  IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=<kh> -o
  HashKnownHosts=no [-o
  StrictHostKeyChecking=yes | -o StrictHostKeyChecking=accept-new]"`
  with the private key materialized 0600 into per-fire scratch (swept on
  every exit path — no feature state on disk, law 1); the known_hosts
  file is ALWAYS a per-fire path (pinned content, or empty when
  unpinned — never the ambient `~/.ssh/known_hosts`); `HashKnownHosts=no`
  keeps the learned hostnames stable plaintext for the harvest merge
  (distro `HashKnownHosts=yes` would salt a fresh `|1|` token per fire);
  unpinned hosts
  trust on first use (`accept-new`, never the silent-insecure `no`).
  After a successful SSH push the runner returns the post-push
  known_hosts content for the trust harvest (features/13 §2 — merge
  into the secret sidecar, fingerprint surface).
  `x/crypto/ssh` is server-transport-only and is never used as a client
  (law 1 sub-point). Keypairs are generated dep-free (stdlib
  `crypto/ed25519` + hand-rolled OpenSSH wire format — no `ssh-keygen`
  subprocess, no new runtime dependency). Ref enumeration for the
  refspec render reuses the §12 `for-each-ref` argv (one cheap local
  spawn per fire, no store trip). Rationale: law 2 pins exact argv —
  a new spawn needs its doc line in the same change; forced wildcards
  with `--prune` carry the `--mirror` deletion semantics (spindle is
  the primary) over exactly the refs the manifest store publishes
  minus forge-internal namespaces (`Sync(LevelServe)` materializes
  them first, `refs/pull/**` included — a bare `--mirror` would ship
  them).
- **Merge-tip pack argv (issue #424, 2026-09-13, `internal/pulls` PackTip +
  the `RefPack` publish seam):** server-made merge/update-branch commits
  must reach the bucket atomically with their ref update (a merged ref
  without its objects bricks clones AND fork children — found live: the
  parent itself failed `upload-pack: not our ref`). Three pinned argv, all
  run in the serving copy (`GIT_DIR=<repo>`, pool + ctx timeout, 8 KiB
  stderr discipline per §2):
  `git for-each-ref --format=%(refname)` (the exclusion set),
  `<tip>\n^<ref>\n… | git pack-objects --revs --stdout` (caret negation
  rides stdin — pack-objects takes no `--not/--all`, verified live; the
  pack is thick and complete), and
  `<pack> | git index-pack --stdin --fsck-objects` in a hand-built
  scratch git-dir (the ingest shape: `objects/pack/`, `refs/`,
  `objects/info/`, `HEAD` seed, serving config copy — index-pack
  requires a recognizable layout). Empty (header-only) output means the
  tip is fully contained and the publish stays ref-only. The pair feeds
  the unchanged funnel as a PUSH entry (txn + `PreparedPack`, tier 0)
  through `RefPublisher.UpdateRefWithPack` — the #263 tag-pack precedent,
  one entry, never a dangling ref. Rationale: `--not --all` (the
  rev-list spelling) is a usage error in pack-objects (exit 129); caret
  lines are the stdin-native exclusion.
- **NEW (2026-11-21) — a refused pack terminates the exchange (audit V1
  open item: "rejected pack left the push client hanging").** Two defects
  met here; both are closed in `internal/server` and pinned by
  `internal/e2e/pushreject_test.go` + `internal/server/pushterm_test.go`:
  1. **`thin=true` is unconditional, exactly as §3.2/§7 step 2 already
     said.** The code had keyed `--fix-thin` on the client requesting a
     `thin-pack` capability, but receive-pack never advertises it (the v0
     cap list is the §6 receive-pack row) and `git send-pack` builds a
     thin pack by DEFAULT regardless — stock receive-pack therefore always
     ingests with `--fix-thin`. Keying on the cap rejected every
     incremental push carrying a cross-pack delta with `pack has N
     unresolved deltas`; a fresh repo (no external bases) hid it.
  2. **Every refusal writes the negotiated report** (§7.2): `git push`
     requests ONLY `report-status-v2` when it is advertised, and the
     refusal path tested `report-status` alone, so it wrote the band-2
     message and returned — the client blocked reading a status stream
     that never ended. `PushRequest.WantsReport` (either cap) is the
     gate, and the trailer carries one `ng <ref> unpacker error` line per
     command (stock receive-pack's shape; without it git prints "remote
     failed to report status" instead of naming the refused ref). The
     connectivity refusal uses the same trailer with the missing-object
     reason per ref.
  3. **The HTTP push exchange is bounded** by `server.request_timeout`
     (doc 11 §2 — until now an unused "documented cap"): one absolute
     read+write deadline covers the body read and the result write, so a
     client that trickles an upload or stops reading the report cannot
     pin the handler; a ResponseWriter that cannot carry deadlines
     ignores the call. SSH needs no equivalent: the session channel ends
     with the handler and the ingest is bounded by `git.ingest_timeout`.
     The git subprocess is reaped on the error path (`cmd.Run` =
     start+wait; `TestIngestReapsIndexPack` asserts no child survives).
- **NEW (2026-11-21) — a shallow push is refused legibly (closing-audit Observation 2).** `git push`
  from a shallow clone opens the request with a `shallow <oid>` section; the v1 advertisement has no
  `shallow` capability, so no server-side ingest can make the exchange valid. `parsePushCommands` now
  returns `git.ErrShallowPush` (whose message names the fix: `git fetch --unshallow`, and
  `with: fetch-depth: 0` for a Threads workflow checkout) instead of failing the first line as
  `bad push command line`, and the servers surface it as a refusal, not a generic protocol error:
  HTTP answers the git wire's ERR packet (status 200, so `git` prints `remote: …` in the job log) and
  SSH returns the same sentence; both log a Warn with repo and principal. A leading-section-only
  `git.ParseShallowSection` detects it without reading pack bytes. This is a refusal, not capability
  support — advertising `shallow` remains a follow-up. Gates: `TestParsePushRequestShallowRefused`,
  `TestParseShallowSection`, `TestPushFromShallowCloneNamesCauseAndRemedy`,
  `TestSSHReceivePackShallowCloneNamesRemedy`.

- **NEW (2026-11-21, Feature F) — the browser commit plumbing (§15) uses `diff-tree`, not
  `diff-index`, and gives the temporary index an empty `GIT_WORK_TREE`.** The plan's prose named
  `git diff-index --quiet <base>`; without `--cached` that compares the *work tree* (a bare repo has
  none) and with `--cached` it re-reads the index we just built. `diff-tree --quiet <base> <tree>`
  asks the no-op question directly over the two trees. `update-index --force-remove` also refuses to
  run at all in a repo with no work tree ("this operation must be run in a work tree"); the empty
  scratch dir satisfies the check and nothing is ever read or written there. Both argv are pinned by
  `TestSubprocessGitCommitLifecycle` / `TestSubprocessGitErrors`.

- **NEW (2026-11-25) — code search is one `git grep` on a DERIVED scan pool (§17).**
  grep at a resolved sha needs no worktree, index file, write or lock, and its result
  is exact-at-the-commit with no invalidation; that is why v1 has no index
  (`docs/features/20_search.md` §2). `Layer.ScanPool` (2 slots) is separate from the
  blocking `Layer.Pool` for the reason the bulk/control-plane split exists at all: a
  broad search may burn a core for the 10 s deadline, and clone/upload-pack must not
  queue behind it (nor search behind a clone). Nothing here writes, so law 4 and law 6
  are untouched. The deadline and every cap are result fields, never errors; exit 1 is
  empty success and `≥ 2` is a failure (git 2.53 exits 128 for a bad `-e` pattern —
  the plan's "exit 2" was an assumption, corrected here).

## 14b. Issue-template reads (docs/features/02 §15, feature-owned)

`internal/issues` reads issue templates at a commit from the same serving copy, with the §14 discipline (read-only, no
worktree, `GIT_DIR` set, `GIT_TERMINAL_PROMPT=0`, ctx timeout, bounded stdout/stderr, own bounded semaphore of
`4 × GOMAXPROCS`; a non-zero exit is the not-found class, an exec/timeout failure is 503). Exact argv:

```
git ls-tree -z --name-only -r <commit> -- .spindle/ISSUE_TEMPLATE
git show <commit>:<path>
git rev-parse --verify --quiet <ref>^{commit}
```

Only direct children of `.spindle/ISSUE_TEMPLATE` with a `.md` extension are read; nested paths and other extensions are
ignored, and `.github` is never consulted. These calls run **only** from the post-push hook (`OnPush`), never from a
composer or API request — that path reads the cached `meta/issue_templates.json` projection instead (law 6).
