# UPGRADE — moving a running spindle instance to a new build

> Status: normative procedure. It exists because on **2026-11-22** a stale
> `spindle.toml` put the live instance into the restricted boot: the old binary
> tolerated `server.auth.mode`, the new one refused it, and nothing checked the
> deployed config before the task was recreated. The preflight below is the fix.

The procedure is the same for the container image and a bare binary. It assumes
one writer instance (the normal deployment: `serve` + `maintain` roles). With
several instances, upgrade **writers first, then readers**, one at a time, with
the same preflight on each host.

## 0. What you need before you start

- the **new** binary or image, pulled but **not** started;
- the deployed config path and data dir (`--config`, `--data-dir`, or the
  defaults `<data-dir>/spindle.toml` / `~/.local/share/spindle`);
- the bucket credentials the instance uses (`store.backend = "s3"`:
  `store.s3.access_key_env` / `secret_key_env` must resolve in **your** shell
  for the doctor to read the bucket; `filesystem`: `store.root` must exist and
  be readable);
- a terminal on the host (or `docker run --rm` next to the volume) — `doctor`
  is a read-only client of the same bucket, it does not need the server socket.

## 1. Checkpoint and quiesce (stop writers)

1. **Checkpoint.** Let the maintainer write a checkpoint at the current head
   (`maintenance.checkpoints = true`, one pass) or run it explicitly:
   `spindle compact <REPO> --once` for a single repository, `spindle bundle run`
   for the bundle slot state. A checkpoint is the only thing that makes a fast
   rollback to the old binary cheap.
2. **Quiesce.** Stop accepting writes: drain the task that runs `serve`
   (SIGTERM; the server's two-phase drain honours `server.drain_timeout`), or
   scale the writer to zero. Do not start the new binary while the old one is
   still committing — the manifest CAS keeps the bucket consistent, but the
   point of the quiesce is that the preflight sees one settled state.
3. Note the old image tag/SHA and the old `spindle.toml` bytes. That is your
   rollback pair.

## 2. Config preflight — **before** recreating the task

Run the **new** binary against the **deployed** config. Not a default file, not
a copy: the exact path and environment the task will use.

```console
# 1. The shipped validator: unknown/removed keys, every §5 rule, ignored env
#    overrides. --strict exits 3 when an env override was ignored.
$ /path/to/new/spindle config check --config /etc/spindle/spindle.toml --strict

# 2. The readiness check: config findings + a bounded, read-only bucket sample.
$ /path/to/new/spindle doctor --config /etc/spindle/spindle.toml --limit 200 --budget 2000
```

Both must be clean (`doctor` exits 0). The 2026-11-22 failure was a config the
**old** binary accepted and the **new** one rejected:

```console
$ new/spindle config check --config /etc/spindle/spindle.toml
error: /etc/spindle/spindle.toml: unsupported key "server.auth.mode" (line 4): server.auth.mode was removed —
       OIDC (issuer + client + session secret) is the only authentication; delete the key
$ new/spindle doctor --config /etc/spindle/spindle.toml
config.keys        server.auth.mode    unsupported key "server.auth.mode" (line 4): …
doctor: 1 findings, not clean
```

Fix the file (delete the key, add the replacement the message names), re-run
both commands, and only then continue. `config check` is the rule set;
`doctor` adds the bucket sample — run them both.

If the preflight is clean but the task still boots restricted, see §5.

### What `doctor` reads (and does not do)

- **Config**: `config.Validate` plus the strict loader's own unknown/removed-key
  errors — the exact rules `serve` enforces, not a second copy.
- **Bucket sample**, one bounded LIST (or a singleton GET) per family: `manifest`, `refs`, `access`,
  `identity`, `collab` (issues/pulls/review), `packages`, `threads`
  (runs/queue), `moderation`. `--limit N` caps decoded objects per family and
  orphan rows per check, `--repos N` caps sampled repositories, `--budget N`
  caps total decoded objects. Every family line says **"sampled M of N"**, so
  a truncated scan is visible, never implied to be exhaustive.
- **Orphans, highest-risk only**: Threads artifacts whose run record is gone;
  global queue index rows naming a missing per-repo job; moderation report
  index pointers with no record; fork child/parent pointers that disagree; and
  a repository prefix with WAL history (`checkpoints/` or `log/`) but no
  `manifest.pb` (a repository that cannot open). It is not a general referential
  graph walk. Two deliberate exclusions: package blob/upload staging objects
  are not checked for orphans (the staging object is its own record, so there
  is no index pointer to dangle), and registry blobs are not decoded (they are
  bytes; only manifests and tags are JSON). Run the check during the quiesce
  for the strongest signal — the queue and report indexes are reconciler-owned,
  so a live writer can leave a transient pointer that the next pass repairs.
- **Never writes and never repairs.** The only store operations are GET, HEAD,
  LIST, LISTPREFIXES. It is safe against a live instance; it holds no locks.

Output is one line per finding — `check`, the full key path, a short reason —
plus the sampling lines and a summary. `--json` is the stable machine shape
(`version`, `findings[]{check,key,reason}`, `summary`, `families[]`).

Exit codes: **0** clean, **1** findings, **2** usage/IO failure (bad flags, a
missing explicitly-named config file, an unreadable store). `2` means the check
did not complete — never treat it as "clean".

## 3. Upgrade the image/binary

Start the new build with the same config, data dir, bucket and environment.
Watch the startup log for the version line and for warnings; `GET /readyz`
must answer 200 once cache prewarm (if configured) completes. The old and new
binaries share the bucket format — protobuf fields are append-only and the
manifest CAS is the only commit point — so a short overlap is safe, but keep
the quiesce unless you have a reason to overlap.

## 4. Verify the surfaces

Run the same `doctor` again against the **running** instance's config and
bucket (it is read-only, so this is the same command as §2), then walk the
surfaces a user would:

1. `GET /healthz` and `GET /readyz`.
2. sign-in and the Administration area (`/admin`, `/admin/config`,
   `/admin/access`, `/admin/maintenance`) — the Overview's config status must
   be present, not "restricted".
3. one repository page, one clone/fetch, one push (or a read-only mirror sync).
4. one collaboration flow the deployment uses (open an issue, view a PR, a
   Threads run if CI is enabled).
5. `/metrics` if you scrape it.

If `doctor` reported findings you accepted as known-benign, record them in the
change ticket; do not silence the check.

## 5. If the boot is restricted

A restricted boot serves only `/admin`, `/admin/config`, their POSTs, the
auth-test POST and the health endpoints; everything else answers 503 with a
pointer to `/admin`. That is the recovery loop, not an outage:

1. Open `http(s)://<host>/admin` — the Overview names the config errors and
   links each to its row on `/admin/config#row-<key>`.
2. Fix the named keys on `/admin/config` and **Save** (writes
   `<data-dir>/spindle.toml` atomically; a backup of the previous file is kept).
   The page tells you which changed keys are restart-required.
3. **Restart.** The save does **not** hot-swap the running config; a restart is
   the supported path, and the page says so. In a normal boot the restart
   control is on **`/admin/maintenance` → Restart** (typed confirmation, audited,
   one self-signal; the process then exits and the supervisor starts it again —
   with `restart: no` it stays down). The restricted boot serves only `/admin`
   and `/admin/config`, so the Maintenance page is not reachable there: restart
   the task/process from the platform supervisor instead.
4. Re-run `doctor` and confirm the boot is normal (the 503 surface is gone and
   `/readyz` is green).

If the file is invalid in a way that keeps `/admin` from rendering the editor,
the boot-error table still names the file and line; fix it on disk and restart.

## 6. Rollback

The bucket stays compatible in both directions for this release line, so
rollback is: stop the new build, start the old image/SHA with the **old**
`spindle.toml` bytes (keep the pair from §1.3), and confirm `/readyz`. If the
new build wrote anything the old one cannot read, the preflight in §2 is what
should have caught it — capture `doctor --json` from both binaries and attach
it to the incident.
