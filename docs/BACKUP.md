# Backup and restore

> Status: procedure for operators (2026-11-22), pinned by the `make backup-drill`
> e2e drill (`internal/e2e/backup_test.go`). Scope: a single spindle instance (or
> a fleet sharing one bucket). This is not a product feature — it is the
> operational contract around the fact that **the object store is the
> repository**.

## 1. What the unit is

Back up the **object store**, not a machine:

| Store | What to copy |
|---|---|
| filesystem | the directory at `store.root` — `<data-dir>/store` when unset (`store.backend = "filesystem"`) |
| S3 | the bucket at `store.bucket`, under `store.prefix` (empty prefix = the whole bucket) |

Everything the product owns lives there: `repos/<owner>/<repo>/` (manifests, WAL log
segments, packs, checkpoints, issues, pulls, releases, packages, Threads runs,
attachments), plus bucket-root `users/`, `meta/`, `maintain/`, `packages/` and
`events/`. A repository has no other home, so a store backup is a repository
backup, an identity backup and a CI backup at once.

**Not the unit, and expendable:**

- `<cache.dir>` — serving copies, materialized packs, render caches. The next
  fetch rebuilds it. Do not back it up; do not restore it.
- a running instance — instances are disposable. Restore into a fresh one.

**Back these up beside the store** (they are not objects):

- `<data-dir>/spindle.toml` — the config (issuer, client secret, session secret,
  store coordinates, admin group). Without its OIDC client secret the instance
  cannot authenticate.
- `<data-dir>/ssh/ed25519_host_key` — the SSH host key. Losing it does not lose
  data; it forces every client to re-pin the host key (TOFU).

Keep the config backup wherever you keep the store backup, with the same
retention. A config is small and hand-recoverable; the store is not.

## 2. Filesystem store

### 2.1 Quiesce, then copy

The manifest CAS (`repos/<o>/<r>/manifest.pb`) is the single commit point; a
push writes its log segment and pack **before** CASing the manifest. A copy
taken while a push is in flight can therefore hold a new log segment and pack
that no manifest names (harmless garbage), or — worse — a manifest whose
objects were not copied if the walk passed them first (a corrupt restore).
Quiescing removes the question.

```sh
# 1. stop the process and wait for it to exit (SIGTERM drains; it does not
#    accept new work). With compose: docker compose stop spindle
systemctl stop spindle        # or: kill -TERM <pid>

# 2. snapshot with the shipped script (writes ARCHIVE and ARCHIVE.sha256)
sh scripts/backup-store.sh \
    --from /var/lib/spindle/store \
    --to /backup/spindle-store-$(date -u +%Y%m%dT%H%M%SZ).tar.gz

# 3. start the process again
systemctl start spindle
```

Back up the two non-store items beside the archive while it is stopped:

```sh
cp -p /var/lib/spindle/spindle.toml /var/lib/spindle/ssh/ed25519_host_key /backup/
```

(The Configuration page's automatic `<file>.bak-<utc>` — `spindle.toml.bak-<utc>` for the default file — is a save-time
undo buffer, not a backup: it lives on the same disk.)

The script is `tar -C <store> -cpzf`, i.e. an ordinary archive: any `tar`/`rsync`
that copies the directory *contents* with modes and mtimes is equivalent. If
you prefer rsync for a big store:

```sh
rsync -aH --numeric-ids /var/lib/spindle/store/ /backup/store-2026-11-22/
```

Permissions: the store is private data. Keep the archive mode `0600`, owned by
the operator, and on restore `chown -R spindle:spindle <store>` (whatever user
the service runs as) with directories traversable and files readable by it.
Never restore over a live store; `restore-store.sh` refuses a non-empty target
for this reason.

### 2.2 Restore into a fresh data dir

```sh
# fresh data dir; do NOT reuse the broken one
install -d -m 0700 /var/lib/spindle-restored
sh scripts/restore-store.sh \
    --from /backup/spindle-store-2026-11-22T020000Z.tar.gz \
    --to /var/lib/spindle-restored/store

# same config, new paths: copy spindle.toml and the SSH key, then point the
# flag-derived paths at the new data dir (only store.root/cache.dir matter)
install -m 0600 /backup/spindle.toml /var/lib/spindle-restored/spindle.toml
install -d -m 0700 /var/lib/spindle-restored/ssh
install -m 0600 /backup/ed25519_host_key /var/lib/spindle-restored/ssh/
chown -R spindle:spindle /var/lib/spindle-restored

spindle --data-dir /var/lib/spindle-restored config check --strict
spindle --data-dir /var/lib/spindle-restored serve   # or point the unit at it
```

`restore-store.sh` verifies `ARCHIVE.sha256` when it sits beside the archive.
The config may be the old one verbatim. If its `store.root`/`cache.dir` are the
defaults (`<old-data-dir>/store`, `<old-data-dir>/cache`), `--data-dir` re-points
them to the new data dir; an explicitly configured path is preserved, so edit
those two keys when you want the restored instance to use the new directory.
Do not reuse the old cache dir.

## 3. S3 store

An object store gives **per-key** atomic reads and writes, not a snapshot of a
bucket. Copying a live bucket is exactly as racy as copying a live directory,
only the race is easier to hit:

- a `sync`/`LIST`-driven copy walks keys; while it runs, a push can land its log
  segment, its pack, and then its manifest CAS. A copy that captures the
  manifest but missed an object it references is **not restorable**.
- the reverse capture (objects copied, manifest not) is harmless: the objects
  are unreferenced and the old manifest is a consistent point.
- versioning turns overwrite/delete into recoverable versions; replication
  copies deletes too unless versioning is on, so replication alone is not a
  backup.

Preferred, in order:

1. **Quiesce + copy.** Stop every writer of the bucket — all `serve` instances,
   any `maintain`/`events` role, and `runner serve` — then copy. With AWS CLI:

   ```sh
   aws s3 sync "s3://$BUCKET/$PREFIX" /backup/spindle-store-$(date -u +%Y%m%d) \
       --only-show-errors
   ```

   (or `aws s3api`/`mc mirror`/provider-native tooling). Because no writer runs,
   the result is a point-in-time image.

2. **A provider point-in-time snapshot** of the bucket (bucket snapshot, backup
   vault, or a versioned copy at one version of every key). This is the only
   live-safe option for a large bucket.

3. **A live `aws s3 sync` at your own risk.** It is restorable only if every
   manifest captured is older than or equal to the objects captured. Copying
   objects first and manifests last narrows but does not close the window (a
   manifest CAS after you passed its objects names a pack you never copied).
   Treat it as a best-effort copy, never as the backup you rely on.

Restore: `aws s3 sync /backup/spindle-store-2026-11-22 "s3://$BUCKET/$PREFIX"`
into a **fresh bucket/prefix**, then point `store.bucket`/`store.prefix` at it
(or `--delete` into the old bucket only after the fresh instance verified).
Keep object metadata (Content-Type is load-bearing for a few surfaces); `sync`
preserves it.

### 3.1 Does a checkpoint make a live copy safe?

No. A checkpoint (`checkpoints/<seq>/`) folds the log and gives a stable
`seq` to replay from, but it is one more object written into the same bucket —
it does not freeze the rest of the bucket. It **does** shorten the replay on
restore (see RTO below) and is worth forcing before a planned quiesce:

```sh
spindle compact --once               # one maintenance pass; may write a checkpoint
```

The safe sequence is still: stop writers, then copy. If you cannot stop writers,
use option 2 (provider snapshot), not a cleverer copy loop.

## 4. RPO and RTO

These are properties of **your** cadence, not of spindle. The assumption this
document makes:

- **RPO ≈ backup cadence.** If you snapshot the store nightly, a failure at
  03:00 loses at most the writes since the last snapshot (~a day), regardless of
  the checkpoint interval. `wal.checkpoint_interval` defaults to `1h`, but a
  checkpoint is not a backup: the WAL segments above it are still only in the
  store, and the store's copy is what you restore. Measure RPO as the age of the
  newest *archive*, plus detection and response time.
- **RTO ≈ restore bandwidth + boot + replay.** Restoring writes the whole store
  (packs dominate); boot is seconds. Replay covers `checkpoint.seq+1 ..
  manifest.head_seq` from the copied log segments — small because checkpoints
  fire at 256 entries / 8 MiB of tail / 1 h. Cache warmth is not part of RTO:
  the first clone after restore refetches packs.
- A live S3 copy does not improve RPO beyond the newest *consistent* manifest
  captured, which can be older than the copy's end time. Another reason to
  quiesce.

State your own numbers next to the cadence in your runbook (e.g. "nightly
archive, RPO ≤ 24 h, restore of 40 GiB measured at 12 min"). Do not quote numbers
from this file as if they were measured for your store.

## 5. Restore into a fresh instance: verify the surfaces

Boot the restored instance and check the things a backup is supposed to carry.
The full checklist is short and mechanical:

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz        # 200; "restricted" means the config did not load
spindle config check --strict                 # zero ignored overrides, zero invalid keys
spindle repo list                             # repositories exist
spindle repo info owner/name                  # manifest stats: head_seq, packs
```

Then, in a browser or the JSON API:

- **repository** — clone one repository; `git fsck` and compare `HEAD` against
  the expected sha (`git ls-remote`). A clone is the real pack integrity check.
- **issues / pull requests** — open an issue and a PR: title, body, state,
  author and event timeline read back.
- **packages (OCI)** — `GET /{owner}/{repo}/api/packages` lists the tags; pull
  one tag or read `/api/packages/{digest}`.
- **Threads** — the run list and a run record read back; logs exist only for
  runs a runner executed (queued runs have records, not logs).
- **identity** — sign in; the account and admin group resolve from
  `users/<name>/user.json`.

`spindle doctor` is the single command that will aggregate these checks; it is
not in this build yet (it is landing separately). Until it lands, the checklist
above is the substitute — run it before pointing traffic at the restored
instance.

## 6. Drill instructions (the stressed version)

Know the drill before you need it. `make backup-drill` runs the automated one
(filesystem store, real git, no docker): seed a repo, issue, package tag and
Threads run, snapshot, restore into an empty instance, and compare the surfaces
semantically. Run it after any change to `internal/store` or the store key
layout.

When it is real:

1. **Do not restore over the broken instance.** Start the restored store beside
   it, on another port; keep the old data dir (rename, never delete) until the
   new one is verified.
2. **Pick the newest archive and verify its checksum** before extracting
   (`restore-store.sh` does this when `.sha256` is present).
3. **Use the same config** or a copy with `store.root`/`cache.dir` adjusted.
   Do not hand-edit secrets under stress; reuse the backup config.
4. **Boot and check `/readyz`.** `restricted` means the config is invalid, not
   that the data is gone — fix the config, not the store.
5. **Clone one repository and read one issue/package/run** (section 5). Only
   then switch the service/ingress to the restored instance.
6. **Keep the archive** until the next successful backup, and note in your
   incident log which archive (`sha256`, timestamp) you restored.

The procedure is the same for a fleet: stop every writer of the bucket
(`serve`, `maintain`, `events`, `runner serve`), snapshot once, restore one
instance, verify, then start the rest against the restored bucket.
