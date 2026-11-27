# 15 — Testing strategy
> Source: MASTER_RUST_SPEC.md §17 (testing strategy, FaultStore, sim), §4.8 (round-trip budgets as test assertions), §20 (discrepancies as test-worthy behaviors) · Status: normative for the spindle Go implementation.

spindle keeps the Rust system's testing philosophy unchanged: correctness claims are **proven by simulation**, not by unit tests alone; the object-store trait has **one contract suite run against every backend**; and protocol work is judged on **counted round trips**, asserted at the transport layer. The Go port replaces the tooling (cargo → go test, just → make) but copies every tier, every fault mode, and every budget — and adds three gates the Rust repo did not have: a per-package **coverage floor** (§1, §7), a **web/JS test tier** (§6.5), and a **rendered accessibility smoke** over a fixed page set (§6.6).

Test framework: stdlib `testing` only. No testify, no gomock (dependency policy, see 01_overview.md). Assertions are `if got != want { t.Fatalf(...) }`; subtests are `t.Run`; parallel cases use `t.Parallel()`. Directory layout follows the canonical tree (02_concurrency.md is the canonical concurrency playbook referenced throughout).

---

## 1. Tiers

| Tier | Command | What runs | Bar |
|---|---|---|---|
| fast | `make test` → `go test -short -count=1 ./...` + `node --test web/test/unit/*.test.js` | every unit/integration test honoring `testing.Short()`; hermetic: in-memory store, `t.TempDir()` caches, the real `git` binary; plus the headless JS tests (§6.5) | < 1 min; a watchdog (`timeout 300`) wraps the Go recipe |
| store contract | `make contract` (memory + filesystem, always); `make contract-s3` for the credentialed backend | ONE suite `contract.Run(store, prefix)` (§2) | every case green on every backend: memory + filesystem on every run, S3 whenever its env is set |
| sim | `make sim` → `go test -count=1 -timeout 15m ./internal/sim/...` | fault-injection over one truth store, one `FaultStore` **per instance link** (§3, §4) | the consistency proof; deterministic under a seed |
| e2e | `make e2e` → `go test -count=1 ./internal/e2e/...` | real git clients against the running spindle server (smart HTTP, receive/upload-pack, bundle-uri, WAL) **plus the first-run/bootstrap lifecycle scenarios (§5.3)** plus the collaboration full chain (§5.4) | ~60 s; run whenever git-path, boot-path, or collab-path code changes |
| a11y | `make a11y` → `go test -count=1 -run TestA11y ./internal/e2e/` | the rendered accessibility floor over a fixed page set: real instance, admin sign-in through the in-test IdP, headless Chromium over CDP at 390 px **and** desktop (§6.6) | needs a browser: skips without one locally, `SPINDLE_A11Y=1` makes a missing browser a **failure** (CI); never in the fast tier |
| parity | `make parity` → `go run ./internal/devtools/paritygate -check` + `go test -run TestParity ./internal/e2e/` | the Threads conformance gate and corpus (§6.8): matrix integrity + the four Forgejo corpus files through the parser and the engine's run-creation path | **no docker, no network**; the execution tier is opt-in (`make parity-exec`, `SPINDLE_PARITY_EXEC=1`) and executes the committed golden of a real Forgejo workflow (byte-unmodified) through a server + runner in containers — it needs a host docker daemon, which the standard gate's container jobs do not have; `make parity-closed` adds the plan's closure rule |
| lint/vet | `make vet` → `go vet ./...` && `gofmt -l` && `go build ./...` | Go equivalent of Rust's `-D warnings`: `go vet` output, any `gofmt -l` line, or any build warning fails the gate | zero findings |
| race | PR: `make race-pr` → `-race` over the concurrency-owning packages; push to main: `make race` → `-race ./...` (both `-timeout 10m`, watchdog `timeout 1800`) | full fast tier under the race detector (§6) | zero races; the PR tier races where shared state lives because `internal/webui` is a large render suite that owns no shared state (§6.7); main is the complete set |
| cover | `make cover` | per-package statement coverage of every `internal/...` package, ≥ 95% fail-under (§7) | CI-enforced; new code lands with tests (D7) |
| web | `make test-web` → `node --test web/test/unit/*.test.js` | headless JS logic tests + opt-in fetch-based server smoke (§6.5; runs only with `SPINDLE_TEST_WEB_BASE_URL`) | zero npm dependencies; wired into `make test` and CI |
| slow | no recipe yet (no `TestSlow*` soak tests exist; see D8) | `TestSlow*` soaks when they land: 20 k-ref push, 466 k-ref refs render, long bundle chains | nightly-ish; never in fast |

Rules carried over verbatim from §17:
- **Never run the whole test tree unbounded.** Every recipe is wrapped in a watchdog. The Makefile resolves `timeout` (GNU coreutils) into the `T5`/`T15` variables (§7); when it is absent the recipe runs unwrapped — there is no `gtimeout` fallback, so macOS contributors need coreutils on `PATH`. Never hang a contributor's session.
- **Known-flaky tests are named with their cause, never assertion-loosened.** Port both known flakes with their causes as comments: a base published without `has_commit_graph` ~1 in 3 under the full e2e suite (passes alone); a base-rebuild kill-resume race ~1 in 7 on a shared test-abort constant. If a flake appears, name it, quarantine it behind `-run`, and fix the cause.
- Tests are hermetic unless noted: e2e is the only tier that talks to a live server; slow soaks use the local rustfs dev store.

---

## 2. The store contract suite

**One suite, every backend.** `internal/store/contract/contract.go` exposes:

```go
// Run exercises every observable guarantee of store.ObjectStore against `store`,
// using keys under `prefix` ("" for memory). All state is cleaned up before return.
func Run(t *testing.T, store store.ObjectStore, prefix string)
```

Wiring (`internal/store/contract/backend_test.go`):

| Test | Condition | Backend |
|---|---|---|
| `TestContract_Memory` | always | `store.NewMemory()` |
| `TestContract_Filesystem` | always | filesystem backend (D4) rooted at `t.TempDir()` |
| `TestContract_S3` | `SPINDLE_TEST_S3_ENDPOINT` set (local rustfs from `make dev-store`) | S3 backend, `force_path_style=true`, dev keys |

The filesystem backend (Divergence D4) joins memory as **always-run**: `make contract` exercises both on every invocation, so the conditional-write machinery (sidecar `.lock` + flock + stat-compare + atomic rename; `renameat2(RENAME_NOREPLACE)` create-if-absent with the portable fallback; `"<size>:<mtime_ns>"` version tokens doubling as ETags) is contract-proven exactly like S3 conditional PUT is. The suite runs the same case list against it with no special-casing — if the filesystem backend needs a suite tweak that S3 does not, that is a backend bug, not a suite bug. Platform note: CI is Linux, so the `renameat2` path is what CI proves; the portable fallback is additionally unit-tested in `internal/store` (forced by a test-only hook), not by this suite.

Envs unset → `t.Skip` with the exact env name in the message. **S3 is first class: the contract must pass on it when its env is set; treating it as optional is a bug** (§17). CI runs memory **and filesystem** always (both env-free) and S3 via the rustfs container (`SPINDLE_TEST_S3_ENDPOINT`). These harness envs keep the plain `SPINDLE_` prefix (single underscore, like `SPINDLE_SIM_SEED`); the `SPINDLE__SECTION__KEY` double-underscore scheme of 11_config_cli.md §4.2 is for config overrides only. A prefix (default `test/<runid>/`) isolates concurrent runs on shared backends; cleanup deletes every listed key under the prefix afterward.

Case list — table-driven subtests, these exact names, copied semantics from the Rust `run_contract`:

| Test function | Asserts |
|---|---|
| `TestContract_PutCreateWinsOnce` | `Create` on an absent key succeeds; a second `Create` on the same key returns `PreconditionFailed` (412-class); content of the winner is intact |
| `TestContract_PutUpdateCAS` | `Update(wrongVersion)` → `PreconditionFailed` with `current` filled (S3 does the follow-up HEAD); `Update(rightVersion)` → ok; version token after update differs; callers never parse tokens, only compare |
| `TestContract_GetIfNoneMatch` | GET with `if_none_match` = current version → `NotModified{version}`; with a stale version → full object |
| `TestContract_GetIfMatch_Mismatch` | GET with `if_match` ≠ current version → `PreconditionFailed`; with the current version → object |
| `TestContract_RangeReads` | half-open `[start,end)` ranges; last byte; out-of-range end clamps (no error); `start ≥ size` → empty body; `size` in the returned meta is the **whole** object, not the range |
| `TestContract_HeadAndAbsent` | `head` on a present key returns size + version; on an absent key returns "absent" (Go: `(*ObjectMeta)(nil), nil` — see 03_store.md for the exact signature); `get` on absent → `NotFound` |
| `TestContract_Delete` | unconditional delete of an absent key is **Ok** (idempotence); conditional delete with the right version deletes; with a wrong version → `PreconditionFailed`; on S3 the conditional delete is the documented emulated HEAD-then-delete (§20.9) — the suite asserts the *result*, not the wire shape |
| `TestContract_ListOrderingAndPrefixIsolation` | `list(prefix)` returns keys lexicographic, strictly after `start_after`; keys of sibling prefixes never leak; a lazy stream (Go: an iterator func `func(yield func(ObjectMeta) bool)`) pages correctly |
| `TestContract_ListPrefixes` | delimiter listing returns distinct `"<prefix><segment>/"`, sorted, files of a directory not returned |
| `TestContract_LargeStreamedRoundtrip` | a multi-MiB `Stream` body (KNOWN length required) round-trips with a SHA-256 checksum match; content-type survives |
| `TestContract_MultipartPath` | a body above `store.multipart_threshold` (64 MiB default; the suite uses a smaller threshold injected for test speed) lands as one object with identical bytes; a mid-multipart failure aborts and leaves no part objects listed |
| `TestContract_Compose` | compose of 1..=32 sources in order = byte-concat; honors `Create` (second compose of the same dest → 412) / `Update` / `Overwrite`; sources remain in place; > 32 sources → `InvalidArgument`; on S3 this is the emulated multipart-of-ranges path (`compose_is_native() == false`) — the assertion is on the resulting bytes, never on the mechanism |
| `TestContract_LeaseSteal` | protocol-level case on top of CAS: absent lease → `Create` (epoch 0); present and `now ≥ expires_at + 2s` → rewrite `epoch+1` via `Update`; early steal attempt → 412 (lost race); release deletes; deleting an already-stolen/absent lease is Ok (§4.9) |

Memory and filesystem backends are always exercised, no env needed — `TestContract_Memory` and `TestContract_Filesystem` are part of the fast tier (`make contract`).

**Concurrency (store contract).** Hazard: the S3 cases hit real HTTP against a container that may be slow or absent. Avoidance: each case is sequential inside one `Run` (the suite is deterministic; parallelism is exercised by the sim, not here), every case keys under a run-unique prefix, and cleanup uses one goroutine draining a bounded channel of keys with a `context.WithTimeout(30s)` so a hung backend cannot wedge the test binary. See 13_concurrency.md for channel-ownership rules.

---

## 3. FaultStore — the sim's heart, ported

Package `internal/store/fault`. One FaultStore wraps the inner store **per instance link**; N simulated instances share ONE truth store (memory), each seeing the truth only through its own fault store.

### 3.1 The fault plan

```go
// Probabilities are 0..=1 floats; Default = no faults at all.
type Plan struct {
    // Uniform latency added to every op, before it is applied. nil = none.
    Delay [2]time.Duration // low, high
    // Latency added to reads (get/head) AFTER the inner op: the answer was taken
    // at an earlier instant and arrives late — a conditional GET racing a local
    // publish. Honors OnlyKeys.
    DelayAfter *time.Duration
    // Retryable before the op is applied (get/head/put/delete/list/compose).
    PErrBefore float64
    // Mutation applied, then Retryable returned (put/delete/compose only) —
    // the class that breaks PUT-then-CAS protocols.
    PErrAfter float64
    // Conditional PUT/DELETE answers PreconditionFailed without applying.
    PCASFail float64
    // get with if-none-match answers NotModified regardless of the real
    // version (a replica that never sees anyone else's writes).
    PStale304 float64
    // get body streams end early with Retryable after some bytes.
    PTruncate float64
    // The op's context never completes.
    PHang float64
    // Every op hangs forever (hard partition). Pending ops keep hanging.
    BlackHole bool
    // Keys containing any of these substrings answer NotFound on get/head
    // (object lost / not yet visible). Mutations still go through.
    DenyKeys []string
    // Keys containing any of these substrings panic on first touch, once per
    // pattern (a crash mid-protocol). A pattern may be scoped to one op as
    // "put:manifest.pb" (ops: get/head/put/delete/compose).
    PanicOnceKeys []string
    // Restrict every probabilistic fault to keys containing one of these
    // substrings (nil = all keys). BlackHole/Deny/Panic are unaffected.
    OnlyKeys []string
}
```

Presets (same semantics as §17):

| Preset | Fields | Meaning |
|---|---|---|
| `Chaos(rate)` | `Delay [0,5ms]`, `PErrBefore=rate`, `PErrAfter=rate/2`, `PCASFail=rate/2`, `PStale304=rate/2`, `PTruncate=rate/2`, `PHang=0` | moderate uniform chaos, the "safety mode" dice |
| `BlackHole()` | `BlackHole: true` | hard partition: nothing ever returns |
| `StaleForever()` | `PStale304: 1.0` | asymmetric partition of the replica kind: writes go through, the instance never learns anything new |

### 3.2 Decide order (per op, normative — port exactly)

The wrapper rolls the dice **in this order**; first match wins:

1. `ops` counter increments (every op, even ones that will fault).
2. **panic-once**: first matching pattern in `PanicOnceKeys` (op-scoped patterns checked as `op:key`, else substring) not yet in the fired set → record it, bump `panics`, `panic(...)` in the request goroutine. In Go a panic must be converted at the instance boundary: the sim's `Instance` wraps every call with a recover that turns it into a synthetic process death — restart the instance (§4). This is the "process crash mid-protocol" injection.
3. **black_hole** → `Hang` (pending ops keep hanging; healing the plan does NOT unhang them — that is the point of a crash).
4. **deny_keys** on non-mutations (get/head) → `Denied` → `NotFound`.
5. **delay**: uniform in `[Delay[0], Delay[1]]` via the wrapper's own seeded RNG.
6. Scope check: `OnlyKeys` set and key matches none → `Proceed` with no faults.
7. Dice, in order: `PHang` → `PErrBefore` → `PCASFail` (only mutation ∧ conditional) → `PErrAfter` (only mutation) → `PStale304` (only read ∧ conditional) → `PTruncate` (only reads with a body) → `Proceed`.

Consequences to preserve: `casFail` answers 412 **without applying**; `errAfter` applies the mutation **then** returns `Retryable` (the ambiguous-write class); `stale304` answers NotModified regardless of the true version.

RNG: xorshift64* seeded per link from `SPINDLE_SIM_SEED` (deterministic; the Rust wrapper uses the same generator shape — port it byte-for-byte so seeds are reproducible across ports). Guarded by its own mutex; never shares the plan mutex.

### 3.3 API surface

```go
type FaultStore struct { /* inner store.ObjectStore; name; mu-guarded plan; stats; rng; fired map */ }
func New(inner store.ObjectStore, name string, seed uint64) *FaultStore
func (f *FaultStore) Set(plan Plan)          // takes effect for every op issued from now on
func (f *FaultStore) Plan() Plan             // current plan (copy)
func (f *FaultStore) Heal()                  // liveness mode for a core link: zero faults;
                                             // ops already hanging stay hung
func (f *FaultStore) Stats() *Stats          // exact per-op counters
func (f *FaultStore) SetTrace(on bool)       // ring of "name op key: decision" lines
func (f *FaultStore) TakeTrace() []string
```

Stats — atomic counters, one per fault class plus total ops: `Ops, ErrBefore, ErrAfter, CASFail, Stale304, Truncate, Hang, Denied, Panics` (atomic.Uint64). `Stats.Faults()` sums all fault classes; `Stats.Summary()` renders the `k=v` line the sim dumps on failure. Budget assertions read `Stats.Ops` (§4.8: "Simulation asserts exact budgets (`FaultStore::stats().ops`)").

FaultStore implements `store.ObjectStore` fully (get/head/put/delete/list/list_prefixes/compose/signed_get_url/accel_target + the get_bytes/put_bytes helpers of 03_store.md), delegating to `inner` after `decide` says `Proceed`.

### 3.4 The truth oracle

The cluster holds the truth store itself (unwrapped). `Cluster.TruthManifest(repo)` reads `repos/<o>/<r>/manifest.pb` **via the truth store, bypassing every link** — no instance's stale view can contaminate it. `checkTruth` post-run: decode the truth manifest, verify committed log segments form a contiguous replay, verify every pusher's refs are (eventually) reflected, verify no pack referenced by the manifest is missing. `dumpTraces` prints each link's name, `stats.Summary()`, and its last 40 trace lines (reversed, newest first) — the standard failure artifact.

### 3.5 Concurrency

Hazard: `Hang` means a goroutine that never returns; a black-holed link leaks a goroutine per op, and `panic()` in a wrapper goroutine kills the whole test process unless recovered. Avoidance: every FaultStore op takes a `ctx context.Context`; `Hang` is implemented as `<-ctx.Done()` with **no timeout of the wrapper's own** (the caller's timeout is the test's instrument); instance calls use `context.WithTimeout(10s)` so a hung op surfaces as a deadline error the sim treats as a crash. The `fired` panic-once set and the plan are mutex-guarded with a **fixed lock order**: plan mutex → fired mutex → rng mutex (13_concurrency.md); no op ever holds two while calling into `inner`. Trace slices use the same plan mutex; `TakeTrace` swaps under it.

---

## 4. Sim scenarios (`internal/sim`)

Landed (Forgejo #338): the sim tier is real — `internal/sim` implements the
harness (Cluster of N instances over one memory truth store, one FaultStore
link per instance, crash-boundary wrappers, retrying pusher, truth oracle)
and eleven of the twelve scenarios below as `TestSim_` tests grouped by theme
(`safety_test.go`, `budgets_test.go`, `liveness_test.go`, `seeds_test.go`;
harness unit tests in `harness_test.go` run in the fast tier). All `TestSim_`
tests skip in `-short` mode (`if testing.Short() { t.Skip }`) so `make test`
stays under a minute; `make sim` runs them with a generous `-timeout`. The
twelfth scenario and two nuances are documented gaps (G1/G2/G3 in the
`internal/sim` package doc and D8 below) — stated here, not silently dropped.

| Go test | Ported from | What it proves | Status |
|---|---|---|---|
| `TestSim_SafetyThenLiveness` | `sim_safety_then_liveness` (seeded) | N pushers × M pushes under `Chaos(rate)` faults: **exactly one winner per competing transaction**, no lost commits, every instance converges to the truth refs; `checkTruth` + liveness checks after faults are healed | landed |
| `TestSim_OrphanedLogSegmentDoesNotBlockWriters` | `liveness_orphaned_log_segment_does_not_block_writers` | crash between the log-segment PUT and the manifest CAS → the segment is an orphan; later writers **burn past its seq** (§6.4), a later commit sweeps it; writers never block on it | landed |
| `TestSim_AfterALostCASResponse` | `liveness_after_a_lost_cas_response` | `errAfter` on the manifest CAS → the write may have landed; the writer re-reads fresh ("cas_landed") and treats "my segment is listed" as committed; no duplicate seq, no lost ref | landed |
| `TestSim_StaleInstanceCannotStarveTheCore` | `liveness_stale_instance_cannot_starve_the_core` | a replica under `staleForever` keeps answering 304; the monotonic revision guard makes a stale manifest read **after a local publish ignored**; the core (other instances) still commits | landed |
| `TestSim_ConcurrentPushersExactlyOneWinner` | `Pusher::push_once` races in `sim_safety_then_liveness` | K instances push conflicting ref updates simultaneously; the truth manifest ends with exactly one manifest revision per CAS and every loser observes the winner's version (412 → re-sync) | landed |
| `TestSim_ReaderWriterReadGuardDuringCompaction` | `liveness_leaked_read_guard_pins_cache_until_drop` | a clone holds the pack-cache **ReadGuard** while compaction removes packs → compaction proceeds (try-write rule: writers never block on a reader lock, they retry); a leaked guard pins the cache only until drop, never deadlocks | landed (try-write rule pinned against the real `rw.TryRWMutex` + the dual-direction liveness proof; pack removal itself stays in `internal/maintain`, outside the sim seam) |
| `TestSim_BaseRebuildResumesAfterKillBetweenAnyTwoPhases` | `base_rebuild_resumes_after_a_kill_between_any_two_phases` | kill the rebuild between any two phases (`copied → repacked → history_pack → commit_graph`); resume continues from the marker iff `manifest.head_seq == started_head_seq`, else restarts; across all attempts **exactly one `git repack` runs** | **gap G1**: not landed — killing between phases needs an injection hook in the maintain phase machine that does not exist; resume is unit-covered in `internal/maintain` (`rebuild_test.go`) |
| `TestSim_DrainInterruptsRunningUnit` | drain hooks (§6.8) + `sim_task_ownership_under_concurrency_and_owner_crash` | SIGTERM phase 1 interrupts the running maintenance unit; the dropped task records failure 503 "interrupted: instance shut down; will be retried by the next pass"; the next pass retries it; serving stays up through phase 1 | landed at the hook level (drain cancels the running unit with the exact 503, refused restarts); **gap G3**: the SIGTERM delivery path itself belongs to e2e, not the sim |
| `TestSim_CheckpointWriterCrashIsInvisibleAndRepaired` | `sim_checkpoint_writer_crash_is_invisible_and_repaired` | panic-once during checkpoint writes leaves garbage keyed by seq that is never a hazard; the next writer checkpoints idempotently | landed with **gap G2**: the crash is invisible and repaired by progress, but a retry at the SAME head is not idempotent (round-1 re-Create 412s — hard-pinned in the test) |
| `TestSim_BlackHoledInstanceIsInvisibleToTheCore` | `liveness_black_holed_instance_is_invisible_to_the_core` | a black-holed link never wedges the core or leases: lease steal after `expires_at + 2s` works, compaction proceeds, the partition heals or the instance is restarted | landed (lease steal over a healthy link + hung-into-deadline on the partitioned link + heal/restart convergence) |
| `TestSim_HealthyRequestRoundTripBudgets` | `healthy_request_round_trip_budgets` | the §4.8 budgets, counted at the transport layer via the FaultStore link stats (below) | landed |
| `TestSim_LivenessUnderRandomSeeds` | `WALGIT_SIM_SEED(S)` plural | loops seeds (default set: 22 and neighbors; `SPINDLE_SIM_SEED` / `SPINDLE_SIM_SEEDS` override) through `SafetyThenLiveness` — the randomized consistency proof | landed |

### 4.1 Budget assertions (counted at the transport layer)

`TestSim_HealthyRequestRoundTripBudgets` snapshots `link.Stats().Ops` around each operation and asserts:

| Operation | Assertion | Budget source (§4.8) |
|---|---|---|
| push (per batch) | `ops ≤ 6` (5 if already synced; settings-only batches skip the sidecar) | freshness GET → (pack PUTs ∥ log PUT ∥ size-sidecar PUT) → manifest CAS; the sidecar PUT adds total ops only, zero sequential trips (R1 B1, Forgejo #248) |
| warm refs sync | `ops ≤ 1` | 1 conditional GET (0 within freshness TTL) |
| cold refs sync (one tail) | `ops ≤ 2` | manifest GET → (checkpoint refs ∥ tail) |
| checkpoint | `ops ≤ 4` | freshness GET → (refs PUT ∥ checkpoint PUT) → manifest CAS; provenance times come from what the writer already applied, **never a log GET** (the 2026-08-22 regression was 6 requests — this assertion is the regression fence) |

Reconciliation with AGENTS.md law 6 ("push ≤ 5 requests") and the Rust §4.8
model ("push ≤ 5, 4 if already synced"): both count freshness GET → (pack
PUTs ∥ log PUT) → manifest CAS. The Go publish path adds exactly one op on
top — the `meta/stats.json` sidecar PUT, which runs IN PARALLEL with the
manifest CAS (+1 total op, +0 sequential trips). So the asserted bound is
Rust 5/4 + 1 sidecar = **6/5 with sequential depth unchanged** — law 6's
intent (defend the depth) is intact, and the with-pack budget case pins the
bound exactly (freshness + pack + idx + log + sidecar + CAS = 6; 5 synced).
Note the correction: ref-only (pack-less) batches DO write the sidecar —
only settings-only batches skip it (see `batchWritesSidecar` in
`internal/wal/publish.go`); the sidecar write is best-effort (a lost sidecar
PUT never fails the push — the maintainer sweep backfills).

Counting rule: read `Stats.Ops` immediately before and after the awaited op on the **acting instance's link only**; background maintenance on other instances does not count against this instance. Failure messages include the measured count, the budget, and `dumpTraces` output.

### 4.2 Concurrency

Hazards and rules (13_concurrency.md is the playbook; these are the sim-specific applications):

- **Hung goroutines** → every FaultStore op is `ctx`-cancellable; the cluster restarts an instance by abandoning its goroutines, never by waiting on them. The test binary's `-timeout 15m` is the outer watchdog.
- **Panic-injected crashes** → recovered at the instance boundary, never at the wrapper: one request's injected panic must not kill the test process (mirrors the prod rule that one request's panic must not kill the instance).
- **Restart races** → `Restart` closes the instance's channels before spawning the replacement; **who owns and who closes** is fixed: the cluster owns and closes all instance lifecycle channels; instances close only their own task channels (13_concurrency.md §shutdown).
- **Shared truth store** → the memory store serializes under one mutex; sim load is deliberately modest (≤ 8 instances) so the lock never becomes the bottleneck under `-race`.

---

## 5. Wire-compat fixtures and git e2e

### 5.1 Golden wire fixtures

`testdata/wire/` holds reference vectors produced by the Rust implementation, checked in and never regenerated by Go code:

| Fixture | Content | Test |
|---|---|---|
| `manifest.pb.golden`, `log_segment.pb.golden`, `lease.pb.golden`, `checkpoint.pb.golden`, `bundle_list.pb.golden` | protobuf bytes exactly as the Rust writer emitted them (package `walgit.v1`, field numbers per 05_protobuf.md) | `TestWireDecode_Golden`: hand-written wire decode (02_protobuf.md's decoder) parses each to the expected struct field-by-field; re-encode → **byte-identical** to the golden |
| `log_frame_stream.golden` | a multi-entry WAL segment with uvarint framing (§5.3) | `TestWireFrames_Golden`: framing decode → entries; re-frame → byte-identical |
| `ref_vectors.json` | CAS version/ETag fixtures per backend from recorded exchanges | fed to the contract suite's token-equality cases |

Byte-identical re-encoding is the compat bar (11_compat.md): a Go-written manifest must be readable by the Rust reader and vice versa; `cmp` of decoded-and-re-encoded bytes is the test. A Go struct field order or omission that changes field numbers fails here, not in production.

### 5.2 Git e2e with real clients

`tests/e2e.sh` (plus `tests/lib-auth.sh` ported) drives a live spindle server with the real `git` binary — the same pattern as the Rust repo:

- The server is started once per run (`make dev` or an explicit URL); e2e talks to `SPINDLE_E2E_BASE_URL` (default `https://walgit.localhost:8080`) with `SPINDLE_TOKEN`. The bootstrap scenarios (§5.3) instead boot their own throwaway servers with a temp `--data-dir` each.
- **Never touch the user's git config.** Every git invocation runs with a private `GIT_CONFIG_GLOBAL` pointing into the test sandbox; credentials are staged there (credential helper or URL-in-token), `GIT_TERMINAL_PROMPT=0` is always exported, and the sandbox is cleaned by the trap. lib-auth.sh is the ONLY place that composes these env vars; test bodies just call its helpers.
- Flows covered (~20 s): clone over bundle-uri (advertisement, chain tokens, remainder fetch), push + ref update + WAL visibility, second clone reusing the bundle, fetch after push from another client, LFS round-trip, 503 edge-fallback shape, drain behavior (SIGTERM mid-clone completes or fails cleanly with the documented status).
- Go harness for unit-level git tests: `internal/git` tests run the real binary with exact argv (04_git.md), same stdin/stdout copy discipline; never go-git.

### 5.3 First-run / bootstrap e2e scenarios (Divergences D5, D6)

Zero-config first run and the Administration area/API are first-class behaviors (06_server_http.md §setup, 11_config_cli.md), so they are pinned by e2e scenarios that boot real server processes. Each scenario gets a fresh temp `--data-dir` (`SPINDLE_DATA_DIR` or `--data-dir`), a fresh port from the ephemeral range, lib-auth's env isolation, and a trap that kills the server and removes the dir; they run as part of `make e2e` (~30 s added) and in CI. Fixtures (config fragments + expected-error lists) live in `tests/bootstrap/`, the scenarios are shell functions in `tests/e2e.sh`, and unit coverage of the setup handlers stays in `internal/server` (table-driven httptest per D7) — the e2e layer proves only what unit tests cannot: a real restart, a real file write, and real 503 routing:

| Scenario | Steps | Asserts |
|---|---|---|
| `bootstrap_no_config` | start `spindle serve` with a clean `--data-dir` and no `spindle.toml` anywhere | boots with built-in defaults (D5): `server.listen = "0.0.0.0:8080"`, filesystem store rooted at `<data-dir>/store`, `auth.mode = "none"`, `auto_create_on_push = true`; `/healthz` + `/readyz` are 200; `/admin/config` serves the UI with the setup banner; the log contains the loud auth warnings; a git push against the anonymous server succeeds |
| `bootstrap_invalid_config` | write a deliberately invalid `spindle.toml` into the data dir (unknown key and a type error, one at a time), then boot | **restricted mode**: `/admin` + `/admin/config`, `/healthz`, `/readyz` answer; every other route (a git endpoint, `/api/v1/...`) returns 503 with a pointer to `/admin/config`; `GET /api/v1/setup` reports file state invalid and the exact validation errors; the UI renders those errors verbatim |
| `bootstrap_setup_save_restart` | from the invalid (or absent) config: `PUT /api/v1/setup` with a corrected config, then restart the process | save returns 200 and lists which keys need a restart; `<data-dir>/spindle.toml` exists afterwards and is byte-valid TOML; after restart the server boots normally (no banner, no 503s, saved values effective — verified via `GET /api/v1/setup` effective values and a git round-trip) |
| `bootstrap_setup_api_auth` | matrix over the access rule (D6/WP13): (a) no config file; (b) config present + `auth.mode = "none"`; (c) config present + token auth; (d) case (c) with `SPINDLE_SETUP_TOKEN` set; (e) oidc + ID token whose `groups` lists `spindle-admins`; (f) oidc + `groups: ["other"]` / claim absent; (g) oidc + an `admin_emails`/`admin_domains` match | (a) and (b): `GET/PUT /api/v1/setup` open, no credentials; (c): unauthenticated → 401, with the admin token → 200, a non-admin token → 403; (d): `SPINDLE_SETUP_TOKEN` has **no effect** (still 401); (e): 200; (f): 403; (g): 200 (the migration path). `POST /api/v1/setup/test` follows the same rule and never writes the file; `groups_claim = "roles"` reads `roles` instead, case differences match, and `admin_groups = []` never grants admin |

The page layer repeats the access matrix in `internal/webui/admin_test.go` (anonymous / signed-in
non-admin / host admin / open mode × all four Administration pages, asserting status **and** the
designed denied view), pins that `/setup` answers 404 for GET and POST (deleted, never aliased), and
walks the area's a11y floor (one `<h1>`, skip link, landmarks, exactly one `aria-current`, one
`.btn.primary` where the page owns one). The rendered proof (desktop + ~390 px, both themes) is part
of the milestone's definition of done, not a test.

### 5.4 Collaboration full chain (Feature 09)

`TestE2E_CollabFullChain` (`internal/e2e/collab_test.go`) proves the
collaboration layer works as one system on a real stack: an OIDC
server subprocess (the in-test IdP mints real RS256 ID tokens; alice admin +
bob writer — two principals, because fan-out never notifies the actor), the
real git binary (preemptive `http.extraHeader` Bearer auth — git never answers
a Bearer 401 challenge), and a real webhook sink. One scenario walks org → team →
access bindings → watch + repo webhook → issue #1 → branch push → PR #2
(`fixes #1`) → cross-user approval review → `require_checks` policy
(blocked merge task goes `error`, then reported-success turns the
combined view green) → merge → `fixes #N` close-on-merge → tag + release
+ latest → author tray + webhook delivery → fork → cross-fork PR (#3,
proving P2 shared numbering across forks). Phase timings log per run
(the §7 rollout evidence in `docs/EVIDENCE.md` E10).

Tier placement: the chain skips in `-short` mode (it is the e2e tier,
not the fast tier). As-built scope notes the test pins: fork
manifest-sharing is deferred (the pull-fork task records fork objects
only — `ForkExecutor` nil), so the chain seeds the fork head by direct
push and proves the cross-fork *open* path; the base-side reachability
probe can only answer for objects the base materialization holds.

The push fast-path fence lives next to the composition it measures:
`TestPushFastPathZeroCollabRoundTrips` (`cmd/spindle/push_budget_test.go`,
excluded from the coverage gate with the rest of `cmd/`) boots the
shipped composition (`buildCollab` — the same function `serveHTTP`
calls) over a prefix-counting store and pushes twice through real
receive-pack: cold 8 ops, warm 9 ops, **0** collab-family keys on
either. Any push-path read of `access.json`, a team roster, or an index
object fails it.

---

## 6. Concurrency test kit (mandates from 13_concurrency.md)

Mandatory for every PR that touches shared state:

1. **`-race` in CI** — every PR runs `make race-pr` (the packages that own shared state: wal, store, notify, api, events, threads, packages, maintain, identity, server, git, sshd, bundle, sim), and every push to main runs `make race` (the full fast tier). A data race is a failing build, no exceptions. The split exists because `internal/webui` is a large render suite that owns no locks, queues or goroutines worth racing — the detector is not finding anything there, and racing it on every PR spent ten minutes of the gate; the complete run still happens on main and on demand. §6.7 records what the suite now costs and the rule for parallelising it.
2. **Stress tests** — every lock/actor with a plausible contention story gets a stress test: N goroutines × M ops against the shared structure, asserting the invariant (not the interleaving). The sim IS the store/WAL stress test; package-level stress tests cover caches, the SSE fan-out, and the events bridge.
3. **Deadlock canary** — the test binary runs under `timeout 300` (fast tier); a suite that deadlocks fails the recipe, it does not hang CI silently. Additionally the watchdog pattern from §3.4 of the Rust spec is unit-tested: a stalled-tick detector test asserts the "async runtime stalled" warning fires when a tick is > 2.5 s late (simulated with a fake clock).
4. **Every goroutine has a shutdown path** — linters cannot check this; review does. The sim's restart machinery is the executable proof that instances shut down on context cancel.

### 6.5 Web/JS test tier (Divergence D2; frontend stack per D-WEB-14/D-WEB-15)

The frontend is server-rendered pages in Go (`internal/webui`), heavily enhanced by the vanilla-ES
islands (`web/src/islands/`) and the dependency-free SDK — and its JS still needs no test framework:
runner is Node's built-in `node --test` (Node 22 or newer; Node 24 also works — pass files or globs, never a directory), invoked by `make test-web`, wired into `make test`
and `ci`. `web/package.json` carries only build devDependencies (`vite`, `@tailwindcss/vite`,
`tailwindcss`, `esbuild`); there are no runtime npm dependencies. The test files import
`node:test`/`node:assert` and the ESM sources directly.

- **Logic/DOM split, normative.** Every testable module — the SDK client groups in `web/sdk/src/*.js`
  (plain ESM submodules; the shipped `dist/repos.js` is the esbuild bundle of them, §1.0 of
  12_web_ui.md) and the islands' pure helpers (SSE frame parsing/backoff/coalescing) — is a pure ES
  module with zero DOM access; DOM wiring lives behind exported `init(root)` functions that the tests
  do not call. This is what makes the tier headless: tests import by relative path (vite aliases are a
  build-time mechanism and never appear in test imports). Page rendering, gating, markdown/diff/graph
  rendering and form handling are Go and covered by `httptest` in `internal/webui` (law 11), not here.
- **`web/test/unit/*.test.js`** — unit cases with `node:assert` in the Go tier's style: repos.js request
  shaping and error mapping, SSE frame parsing/backoff/coalescing, the SDK lane/401 rules. Same
  assertion discipline: `if (got !== want) throw`.
- **`web/test/unit/smoke.test.js`** — fetch-based server smoke, **opt-in**: it only runs when the caller names a server in `SPINDLE_TEST_WEB_BASE_URL` (there is no ambient/default `:8080`, so an unrelated local server cannot break the tier). It asserts the server-rendered contract of D-WEB-14: `/` and `/admin/config` answer 200 `text/html; charset=utf-8` with the `base.html` shell (doctype, `<html lang="en" class="dark">`, `/_ui/app.css` stylesheet, vendored `/_ui/htmx.min.js` classic script, `/_ui/ui.js` module island, `<main id="app">`, server-rendered header/footer, and no Solid `id="root"`), the landing/Configuration page markers, the `/_ui/` assets' MIME + `no-cache` (`app.css` must carry the Tailwind utilities the templates use, `/_ui/index.html` is a 404), and `/repos.js` as `text/javascript` with `no-cache`, a strong `sdk-*` ETag, a 304 on revalidation, and the SDK's `ReposClient`/ESM exports. Unset → every case skips with the reason; set but unreachable or restricted mode (503 at `/`) → skip with the reason; a reachable server is asserted strictly. Unit tests import the SOURCE modules directly, so testing needs no build; `make web` produces only the shipped artifact (and the smoke needs the server to have been built from one).

### 6.6 Rendered accessibility smoke (the browser tier)

AGENTS.md §2 makes the accessibility floor a requirement, and a floor that depends on whoever remembers to drive a browser is not a gate. `internal/e2e/a11y_test.go` is the machine check: it boots the real binary, seeds a repository with an issue, a pull request and a release, signs in as an admin **through the in-test OIDC provider** (the production verification path: discovery, JWKS, nonce, PKCE, group claim), and drives headless Chromium over the DevTools Protocol across a **fixed page set** — landing `/`, repo code `/{owner}/{repo}`, an issue, a pull request, releases, `/admin`, `/admin/config` — at **390 px** and desktop.

What each page must satisfy, per viewport:

- the skip link exists, is the **first** focusable element, targets `#app`, and `#app` is the `<main>` landmark;
- **exactly one `h1`**, and it renders;
- every **visible** interactive control has an accessible name (`aria-label`/`aria-labelledby`, a wrapping or `for` `<label>`, its text, an image `alt`, `value`, or `title`) — and at least one control is found, so a probe that matches nothing fails;
- no **positive `tabindex`**;
- no control that **keyboard focus can land on but nothing renders** (`focus()` is a no-op for a `display:none` subtree, so the probe asks the browser rather than pattern-matching CSS);
- at 390 px, `document.documentElement.scrollWidth === clientWidth` (no page-level horizontal pan);
- **no page-code console errors** — `console.error`, uncaught exceptions, and browser log entries at level error. The allow-list holds exactly ONE entry, by text and URL: the engine's own 404 for `/favicon.ico`, which the browser requests on every page and the app does not ship.
- the first real **Tab** focuses the skip link, matches `:focus-visible`, and paints a non-zero outline; **Enter** reaches `#app`.

The CDP client (`internal/e2e/cdp_test.go`) is hand-rolled on `golang.org/x/net/websocket`, already in the dependency budget (law 1): launch `--headless=new --remote-debugging-port=0` into a throwaway profile, read the `DevTools listening on …` line, connect to the page target, then `Page.navigate` / `Runtime.evaluate` / `Emulation.setDeviceMetricsOverride` / `Input.dispatchKeyEvent`. Chromium is a **CI tool, not a Go dependency**. `--no-sandbox` is opt-in via `SPINDLE_CHROMIUM_NO_SANDBOX`, set only by the CI job (a job container has no user namespaces for Chromium's sandbox); a developer machine runs with the sandbox on.

**Skip/fail rule.** Locally the smoke is best-effort: no browser (and no `SPINDLE_CHROMIUM`) → every case skips with the reason, so a contributor without Chromium is not blocked. In CI the browser is required: the gate sets `SPINDLE_A11Y=1`, and then `TestA11yPreflight` **fails** (never skips) when the browser is missing or will not start. A skipped accessibility gate is not a gate.

**The checker has teeth, and that is tested.** A deliberate break — a second `<h1>`, a button whose only content is `aria-hidden` SVG, a missing skip link, a 1400 px child at 390 px, an inline script CSP violation — fails the run and names the page, the viewport and the offending selector; the break is never committed. `make e2e` runs the same package with `-skip TestA11y`, so the git tier stays browser-free.

### 6.7 Render-suite speed: parse once, parallelise only the independent tests (`internal/webui`)

`internal/webui` was the slowest package in the tree: **624 s** for `go test -race -count=1 ./internal/webui`
on a 10-core host (543 s on a loaded runner; it hit Go's 10-minute per-package timeout twice on 2026-11-22,
which is why the race/cover tiers carried a 20-minute band-aid). The measured cause was not the tests:
each fixture helper (`newBare`, `newRepoFixture`, `newCollabFixture`, `newPublicServer`, `parityServer`,
`newSettingsServer`, `newFormsHarness`) called `webui.New`, which parses **57 page sets × (base + every
partial + the page)** — ~0.6 s per call under `-race`, once per test (1.3× CPU: the parse is
single-threaded). Test bodies summed to 604 s; the parse was >90 % of it.

Two test-only changes took it to **11 s** (`-race -count=1`), 811 tests, `-count=5 -race -shuffle=on` green
in 41 s:

1. **Parse once per test binary** (`internal/webui/testsupport_test.go`). The embedded template FS and the
   `FuncMap` (`helpers.go funcs`) are server-independent, a parsed `*template.Template` is safe for
   concurrent `Execute`, and production parses once per process anyway (one provider per process) — so
   the tests share one parsed page set instead of rebuilding it per fixture (`newTestServer`), and the two
   group harnesses that parse a subset (settings/admin, forms) memoize theirs (`onceTemplates`). The first
   caller still runs the real `New()`, so the parse path — and a boot-time template error — stays covered.
   Each server gets its own map over the shared sets, so a test that swaps a page cannot alter a peer's.
2. **`t.Parallel()` on the 265 independent top-level tests.** Not on the rest.

**The rule (binding tests stay sequential).** The page seams are process globals by design —
`BindAdmin`, `BindSettings`, `BindCollab`, `BindForms`, `BindPackages`, `BindPublic`, `BindRepo`,
`BindThreads`, `BindWebedit`, `BindDirectory`, `bindInsights` — and handlers read the bound value at
request time. Two product knobs are also process-wide and saved/restored by tests
(`adminRestartDelay`, `assetUploadBodyLimit`). A test that touches any of those is **not independent**,
so it must not call `t.Parallel()`: Go's testing package runs every parallel top-level test only after
the last sequential test returns, so keeping the binders sequential is what keeps them out of the
parallel batch's window — no mutex, no sleep, no ordering assumption. What *is* parallel: the pure
renderer tests (markdown/diff/graph/highlight), the helper/format tables, and the full-server tests whose
harness binds nothing (most of `repo_test.go`, `blame`, `compare`, `coverage_core`, `settings_protection`,
16 of the forms tests, …).

The rule is executable: **`TestParallelTestsBindNoGlobals`** (`internal/webui/parallelism_test.go`) parses
the package's `*_test.go` files, builds the call graph, and fails with the call path when a `t.Parallel()`
test (transitively) binds a seam or writes a knob — so a harness that starts binding later cannot quietly
turn a parallel test into a schedule-dependent race. `go test -race -count=5 -shuffle=on ./internal/webui`
is the proof that the two sets do not interfere.

Measured on a 10-core host, 2026-11-23: `-race -count=1` **624 s → 11 s**; `-race -count=5 -shuffle=on`
**41 s**; the non-race fast tier for the package **145 s → 2.8 s**; `make cover` for the package
**2.8 s, 95.1 %** (unchanged). The full race tier (`go test -race -short ./...`) is 158 s wall, and the
slowest package in it is `internal/e2e` at 132 s — 4.5x under the 10-minute per-package ceiling the
Makefile now states instead of the 20-minute band-aid it carried for `internal/webui`. The outer
watchdog stays.

---

### 6.8 Conformance corpus and the parity gate (`internal/e2e`, `internal/devtools/paritygate`)

Threads accepts the Actions syntax, so "accepted" has to mean *tested against real files*. Two
artefacts make that mechanical (`docs/features/21_parity_master.md` §1/§9):

- **`docs/features/parity/parity.json`** — one row per `PAR-` id (`status`, `wave`, `track`,
  `security`, `decision`, `tests[]`). **`matrix.md`** beside it is generated; the gate fails a
  hand-edited or stale copy.
- **`internal/e2e/testdata/parity/`** — byte copies of real workflows with a `SOURCE.json` stamp
  (repository, path, commit, fetch date, sha256), the converted golden each importer run must
  reproduce, and the harness.

`make parity` runs two things and is in `make ci`:

1. **The gate** (`internal/devtools/paritygate`, stdlib only). It fails when a `have` row names no
   test, or names a `Test…` that does not exist under `internal/**`; when a `missing`/`partial`
   row is unplanned (no wave and no decision); when a `non-goal` has no `approved_by`; when the
   master plan's `PAR-` table and the JSON disagree on the id set; or when `matrix.md` differs
   from a fresh render. `WEAVE_CARD_URL`+`WEAVE_TOKEN` additionally compare the card body's ids
   (skipped with a printed notice otherwise — the board's address is not in this tree).
   **The rule that makes green mean something:** a row turns `have` only with a committed,
   passing test named in `tests[]`; a green row without one is a red gate, not a review comment.
2. **The corpus parse/plan tier** (`TestParityCorpusParsesAndPlans`, `-short`). Each converted
   file must parse, expose its documented jobs/step counts, and pass through the engine's real
   run-creation path (`threads.CreateRun` over a memory store).
   `TestParityCorpusTriggerSemantics` drives the corpus's trigger shapes too (a tags-only file
   fires on a matching tag, not on a branch push, and a tag run records no branch). After the
   Wave-1 merges all four files convert with **zero** `UNTRANSLATED` constructs. A file whose
   dependency rows are not `have` yet must instead fail with the refusal the harness documents for
   it (an unused mechanism today), and the entry flips to the strict assertion in the change that
   closes the row — a gap is named, never inert.

Deliberate split, and why it is not a loophole: **the PR gate never needs docker**. The
execution tier (`TestE2E_ParityExecution`, `make parity-exec`) boots a server and a runner and
executes the committed golden of a real Forgejo workflow (byte-unmodified) to success; it needs
`SPINDLE_PARITY_EXEC=1` and a daemon, so it runs where a daemon is available (a developer machine,
an operator host-mode runner), never on the merge gate — every gate job is a container job with no
daemon inside. `make parity-closed` is not a tier: it is the plan's §9 closure check (no `missing` row),
run when a wave closes.

## 7. Makefile — every tier is a target

All dev/CI entry points are Make targets (Divergence D3); there is no justfile, no separate task runner, nothing to remember beyond the tier map below. The watchdog semantics port unchanged: `T5`/`T15` resolve GNU `timeout` (coreutils) and run unwrapped when it is absent — there is no `gtimeout` fallback, so macOS needs coreutils on `PATH`; no watchdog is worse than no tests, but inventing a fallback that does not exist is worse still.

| tier | target | what makes it a tier |
|---|---|---|
| fast | `make test` | `test-go` (`-short`, watchdog) + `test-web` (`node --test`); hermetic, < 1 min |
| race | `make race-pr` (PRs) / `make race` (main) | the fast tier under `-race` (CGO on for the detector only); PRs race the concurrency-owning packages, main races everything |
| coverage | `make cover` | every `internal/...` package ≥ 95 % (D7); per-package profiles, never merged |
| contract | `make contract` | memory + filesystem, always; `contract-s3` is env-gated |
| proof | `make sim` | fault-injection consistency proof + round-trip budgets (`internal/sim`, seeded) |
| e2e | `make e2e` | smart-HTTP against the real git binary (git ≥ 2.47) |
| a11y | `make a11y` | the rendered accessibility floor over a fixed page set, headless Chromium over CDP at 390 px and desktop (§6.6); skips without a browser, `SPINDLE_A11Y=1` requires one |
| parity | `make parity` | the conformance gate + corpus parse/plan tier (§6.8); no docker, no network; `parity-exec` is the opt-in docker tier |
| merge gate | `make ci` | `vet test race cover contract sim e2e parity a11y` (a11y needs a browser; parity does not) |

The command lines live in the `Makefile` (16_packaging.md §7.2 owns the target list); this table is
the tier contract — a tier that is not here is not a tier.

Normative notes: the old `lint` target is `vet` (same three commands). `test-web` runs `node --test web/test/unit/*.test.js` (§6.5) and is part of `make test` — a Go-only green build is not green. S3 contract targets stay env-gated; memory + filesystem (`contract`) never skip. `cover` discovers packages via `go list ./internal/...`, so every collaboration package (identity, issues, pulls, review, checks, notify, releases, social) is gated at ≥ 95% with no target change — the 09 integration added no new tier, only new suites inside the existing ones (§5.4) plus the `cmd/`-side push fence (outside the gate by §7.1). The `sim` tier landed in `internal/sim` (D8) and runs in `ci` between `contract` and `e2e` — fast first, proof last.

### 7.1 The coverage gate (Divergence D7)

CI enforces **≥ 95% statement coverage, per `internal/...` package**; `cmd/` (main glue only) is excluded from the gate. The mechanism is deliberately boring, stdlib-only:

1. `cover` runs one `go test -count=1 -coverprofile=<pkg>.out` per package discovered by `go list ./internal/...` — profiles are never merged; each package is judged alone. The checker (`internal/devtools/covergate`, ~40 lines of Go invoked with `go run`; an awk equivalent is acceptable) reads the raw profile (`mode: set`), **skips every block in a `// Code generated` file** (the generated protobuf sources of 02_storage_protobuf.md would otherwise dominate the denominator), computes `Σ numStmts(hit > 0) / Σ numStmts` over the remaining blocks, and exits non-zero below 95.0, naming the package, its percentage, and its five lowest-covered files so the failure is actionable without opening the profile.

**The rule behind the gate: new code lands with tests.** 95% is the CI floor, not the review bar — the expectation is **near 100%**: every new function lands with table-driven tests (table-driven `httptest` for every handler), and a diff that lowers a package below the floor fails CI before review even starts. An uncovered line needs a named reason in the PR (unreachable defensive branch, `exec`-of-real-git shim); "hard to test" is not a reason, it is a sim scenario waiting to be written.

---

## 8. §20 discrepancies that are test-worthy behaviors

The rewrite copies CODE behavior (§20); each of these gets an explicit assertion so it is not "fixed" silently in the wrong direction:

- §20.2/§20.3: `GET /HEAD` and `/objects/info/packs` are 404; no `/services/git-*` routes — e2e asserts the 404s.
- §20.9: S3 conditional delete is HEAD-then-delete (check-then-act race, accepted) — contract asserts the result only.
- §20.7: bundle leases lack the 2 s skew tolerance and heartbeat sets `epoch = 1` — `TestContract_LeaseSteal` pins the **coord** semantics; the bundle-lease quirk is pinned in the bundle tests (10_bundles.md), not "fixed" by accident.
- §20.12: `report-status-v2` negotiated and parsed, `option` lines never emitted (rejected atomic → plain `ng`) — git e2e pushes an atomic transaction with a rejection and asserts the response shape.
- §20.5: the git layer always execs `"git"` regardless of `git.binary` — decision below changes this; the e2e suite must pass with a wrapper-script `git` to prove the config is honored.

---

## Decisions & deviations from the Rust design

- **Test framework is stdlib `testing` with `-short` as the tier switch, not build tags or `#[ignore]`.** One flag (`-short`) cleanly excludes sim/slow from `go test ./...`, matching Go convention and keeping `make test` hermetic; rationale: zero new dependencies, and `-run` composes naturally.
- **Env prefix `SPINDLE_`** replaces `WALGIT_` (e.g. `SPINDLE_SIM_SEED`, `SPINDLE_E2E_BASE_URL`, `SPINDLE_TEST_S3_ENDPOINT`); rationale: the binary and product are spindle; TOML keys (the compat surface, 11_config_cli.md) are unchanged. *(Refined by the 2026-08-31 divergence below: config **overrides** use `SPINDLE__SECTION__KEY` with the `WALGIT__` alias per 11_config_cli.md §4.2; these single-underscore names remain the harness/process envs.)*
- **`go vet` + `gofmt -l` + `go build` replace cargo's `-D warnings`** as the lint gate; rationale: Go has no clippy; vet + format is the community-accepted equivalent, enforced via `make vet`.
- **FaultStore lives in `internal/store/fault` and the truth oracle is simply the unwrapped memory store** rather than a separate oracle object; rationale: in the Go design every link is a decorator over one shared `store.ObjectStore`, so "read bytes bypassing every link" needs no extra abstraction.
- **Panic-once faults are recovered at the instance boundary and modeled as process death (restart), keeping the wrapper's `panic()`** like the Rust original; rationale: preserves the exact injection semantics (state mid-protocol) while the test harness decides what "the process died" means.
- **The contract suite adds `TestContract_LeaseSteal`** (not present in the Rust contract, which tested leases elsewhere); rationale: in spindle the lease is pure store protocol (§4.9), so pinning it in the one suite that runs on every backend is cheaper and stronger.
- **`git.binary` config is honored by the Go git layer** (fixing §20.5's hardcoded `"git"`); rationale: it costs one flag and makes the §20.5 e2e proof (wrapper-script git) possible; behavior change is marked in 04_git.md.
- **Deterministic RNG (xorshift64*) ported byte-for-byte** so seeds are reproducible across the Rust and Go implementations; rationale: golden failure repro across ports beats a nicer RNG.
- **Budget counters are read from the FaultStore link stats** (exactly as §4.8 prescribes) rather than from an HTTP middleware counter; rationale: the sim exercises the store path directly, and the FaultStore counts precisely the requests the budget model is about.

**Divergence (2026-08-31):**

- **D3 — Make replaces just.** Every dev/CI entry point is a Make target (§7: `build`, `fmt`, `vet`, `test`, `test-go`, `test-web`, `race`, `cover`, `sim`, `contract`, `contract-fs`, `contract-s3`, `e2e`, `image`, `dev-store`, `dev-store-stop`, `clean`, `ci`, `help`, `web`, `landing-gifs` — exactly the `.PHONY` list in the Makefile); the justfile is deleted. The old `lint` target is renamed `vet` (supersedes the `just lint` wording above; commands unchanged). Rationale: make is ubiquitous — no extra tool to install for a first contribution — and the watchdog/watch-what-you-run semantics port one-to-one.
- **D7 — Coverage gate.** `make cover` enforces **≥ 95% statement coverage on every `internal/...` package** (`cmd/` excluded), via per-package `-coverprofile` + the `covergate` checker (§7.1), CI-gated. Review bar is near 100%: new code lands with tests — table-driven `httptest` for every handler. This is new (the Rust spec had no coverage gate); it does not supersede any prior decision.
- **D4 — Filesystem store joins the contract suite as an always-run backend.** `TestContract_Filesystem` runs wherever `TestContract_Memory` runs, no env needed; S3 stays env-gated (`SPINDLE_TEST_S3_ENDPOINT`) and CI exercises S3 via the rustfs container. Supplements (does not supersede) the `TestContract_LeaseSteal` decision — the suite's "one suite, every backend" rule now has three backends, two of them unconditional.
- **D2 — Web/JS tests without a test framework (amended; frontend stack per D-WEB-14/D-WEB-15).** The JS tier is `node --test` over headless pure-ESM logic modules plus fetch-based smoke tests (§6.5), wired into `make test` and `ci`; tests import source (`sdk/src/*.js`, the islands' pure helpers) and need no build — the builds (`make web`: stylesheet + islands + SDK) produce only shipped artifacts. Amended when the user directed a build step for the modular SDK (2026-08-31, second pass); D-WEB-6's SolidJS + Tailwind port (2026-09-02) kept this tier unchanged, and D-WEB-14/D-WEB-15 (2026-11-19/20) moved page rendering/tests into Go `internal/webui` while keeping this JS tier — no test framework, no TypeScript.
- **D5/D6 — Bootstrap and setup lifecycle is e2e-tested.** Four scenarios (§5.3: no-config defaults boot; invalid-config restricted mode 503s; setup save → restart → normal; setup API auth matrix incl. the WP13 OIDC admin-group rows, and pinning that `SPINDLE_SETUP_TOKEN` has no effect) run in `make e2e` and CI. New; no prior decision affected.
- **D8 — Sim tier lands; D3 target list corrected (Forgejo #338, 2026-09-11).**
  `internal/sim` implements the §4 harness and eleven of the twelve scenarios
  (gaps G1/G2/G3 stated in the package doc: no kill hook exists in the
  maintain phase machine; same-head checkpoint retry deterministically
  reports the round-1 re-Create 412; SIGTERM delivery belongs to e2e). `make
  sim` runs it (`go test -count=1 -timeout 15m ./internal/sim/...`) and `ci`
  chains it between `contract` and `e2e`, closing the AGENTS.md law-6 loop
  (budgets now actually asserted). D3's list is corrected to the actual
  `.PHONY` set: `sim` and `contract-fs` gain real recipes;   `test-slow` and
  `dev` are dropped (no `TestSlow*` tests exist; a one-box `dev` run needs
  the docker-backed store, out of scope for the Make surface) — a no-op
  recipe that matches nothing is worse than an honest list. §4.1's push bound
  (`ops ≤ 6`, 5 synced) is reconciled with law 6 / Rust §4.8 (5/4): the +1 is
  exactly the parallel #248 sidecar PUT, sequential depth unchanged; the
  ref-only parenthetical is corrected (ref-only batches DO write the
  sidecar — only settings-only batches skip it). Landing the proof also
  fixed three wal liveness bugs the sim exposed (all in `internal/wal`,
  each regression-pinned, doc 05 amended in the same change): the orphan
  sweep deleting concurrently-committed segments, unbounded orphan-backlog
  growth on failed batches (failure-path sweep), and the wiped version token
  never healing on guard-reject (token adopt). No prior decision affected.
- **CI-flake treatment, Forgejo #397 round (2026-09-12).** Six docker-run
  coverage-gate failures in twenty runs, five distinct flakes, all
  test-contract bugs (no production change): (a) `TestBeginOnMirrorIs409`
  (repoimport) and `TestCreateFromURLOwnerAdmission` (mirror) returned
  while a spawned clone/sync was still writing into a `t.TempDir`, so
  cleanup `RemoveAll` intermittently failed with "directory not empty" —
  fixed by joining the background work first (`awaitDone` /
  `waitAsync`, the `TestCreateFromURL` precedent), never by deleting the
  pin; (b) `TestSSEWriterKeepaliveExitsOnClose` asserted an immediate
  `NumGoroutine` count after spawning keepalives, pinning scheduling, not
  behavior — fixed by polling up to the expected count (the #178
  poll-don't-sleep treatment, both keepalive tests); (c)
  `TestVerifyTokenWireNegatives/tampered_mac` (~1/16 flake, reproduced
  locally 2/30) and `TestWgtTokenLifecycle` (~1/256) tampered with the
  token by substituting trailing base64url characters, which can decode
  to the identical 32-byte HMAC (trailing sextet bits are padding) and
  verify clean — fixed with a deterministic `tamperWireMAC` helper
  (decode, flip a bit, re-encode), keeping the "invalid token" pin;
  (d) the `RegisterKind` process-global maps (repoimport, mirror) leaked
  across `-count=N` re-runs, panicking outside the panic tests' recover —
  fixed with a test-only `ResetKindsForTest` beside each `RegisterKind`
  (production keeps the panic-on-duplicate composition contract; both
  maps are write-only so a reset cannot disturb other tests). The
  resume/claim tests needed no change (already `awaitDone`-polled; the
  #200 log's "resuming import" line was interleaved output, not the
  failing assertion), and the process-global sweep found no other
  test-written shared state. **Retry-guardrail ruling:** no retry stanza
  on the coverage step and no `-shuffle=off` pinning — retries mask
  flakes and shuffle-pinning surrenders real signal; flakes are fixed at
  the test contract instead. Rationale: every one of these pinned
  scheduling or randomness instead of behavior, and each fix is
  deterministic without weakening the assertion.
- **Self-deadlocking SSH limiter test, Forgejo #409 (2026-09-12).**
  `TestMaxSessionsRefuses` (internal/sshd) launched the slot-holder
  session in a bare goroutine and immediately ran the session meant to
  be refused, so the scheduler decided which exec won the single slot;
  when the wrong one won, the test's own fake transport parked on a
  channel only the test's continuation could close — a self-deadlock
  that consumed the full 10-minute package timeout in CI. Test-only
  fix, no production change (the select/default limiter was already
  correct): the transport is built with `entered` and the test waits
  on it before offering the second exec (deterministic slot-holder);
  teardown closes the block AND the client session (each wait joined
  against a 5 s budget with a named failure); the sibling
  poll-with-deadline idiom is reused, never fixed sleeps. Rule
  restated: a test must never block on a channel only its own
  continuation can close — every such wait gets its own short
  deadline with a message naming the stuck wait.

**Divergence (2026-11-23):**

- **The rendered accessibility floor is a tier (`make a11y`), not a habit.** AGENTS.md §2 already
  required a real-browser pass for UI work; nothing ran it on a change that did not touch the UI, and
  nothing proved the floor on a page a writer never opened. `internal/e2e/a11y_test.go` now boots the
  real binary, signs in as an admin through the in-test IdP, and asserts the floor over a **fixed page
  set** (landing, repo code, an issue, a PR, releases, `/admin`, `/admin/config`) at 390 px and desktop
  (§6.6). Rationale: a fixed set is a contract — a page that joins it gets the floor for free, and a
  page outside it has no rendered floor at all. The CDP client is hand-rolled on
  `golang.org/x/net/websocket` (already budgeted, law 1) and Chromium is an apt package in the gate
  container, **not** a Go module; `--no-sandbox` is opt-in (`SPINDLE_CHROMIUM_NO_SANDBOX`) and set only
  by CI, because a job container has no user namespaces for Chromium's sandbox. The tier is separate
  from `make e2e` (`-skip TestA11y`) so the git tier stays browser-free, and the skip/fail rule is
  explicit: no browser → skip locally, `SPINDLE_A11Y=1` → fail in CI (a skipped accessibility gate is
  not a gate).

## Auth test seams (OIDC-only, PLAN-OIDC-ONLY §6)

There is **no configuration flag, environment variable or build mode that authenticates without a
verified credential in the shipped binary**. What the release binary accepts is exactly: OIDC ID
tokens/sessions (signature over the issuer's JWKS), `wgt_` HMAC tokens, operator-authored static
tokens (machine, never admin), and the compiled-in `ExtraCredential` shapes (CI `wct_`, runner
`wrt_`, job `wjt_`) that each feature package authorizes itself.

Tests use these seams:

1. **In-process unit/handler tests** construct `auth.Principal` values directly, mint sessions/tokens
   with `AuthService.MintSession`/`MintToken` (the real HMAC wire), mint RS256 ID tokens against a
   stub JWKS (`internal/server/x_auth_test.go`'s `stubIssuer`), and install the **test-only**
   `ExtraCredential` hook in `_test.go` (`installTestAdminHook`) where a handler needs a host admin.
   That hook is the same production seam composition uses for CI/runner credentials; it exists only
   in test files.
2. **E2E exercises the real verification path**: `internal/e2e/oidp_test.go` is a stdlib-only OIDC
   provider on a real listener (discovery, JWKS, auto-approving authorize, token endpoint). It mints
   real RS256 ID tokens carrying configurable `groups`, and the browser suite
   (`TestE2E_OIDCFirstRunAndAdminByGroup`) drives `/_auth/login` → authorize → callback with
   nonce + PKCE. There is no injected principal and no shortcut: the spindle subprocess verifies the
   signature, issuer, audience, expiry, nonce and the group claim itself.
3. **The seam cannot ship**: `oidp_test.go` is `_test.go` (never compiled into a release binary), and
   the e2e package is not in `cmd/spindle`'s import graph.
4. **Dev IdP for humans**: `go run ./internal/devtools/oidpstub` (stdlib only; also
   `compose.dev-idp.yml` under the `dev-idp` profile). `internal/devtools` is not imported by
   `cmd/spindle`, so it is not in the release binary or image; its `-help` says so.
5. **The gate that keeps it that way**: `internal/config/seam_gate_test.go` scans the config
   package's non-test sources for bypass-shaped keys (`testauth`, `auth_bypass`, `dev_auth`,
   `insecure_auth`, `no_auth`) and fails if one ever appears.
