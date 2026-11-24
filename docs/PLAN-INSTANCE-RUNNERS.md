# PLAN — instance-scoped runners

Status: implementing (2026-11-22). Replaces the repository-scoped runner registry and its per-repo
Settings tab. Pre-1.0: the old shape is deleted in the same change — no aliases, no compatibility flag.

## 1. Trust model

A runner is **instance-scoped infrastructure**, not a repository member. It is registered with a token
minted by a **host administrator** in the browser at `/admin/runners`; that mint is the
authorization. Consequences, stated plainly:

- An instance runner may be offered **any** repository's jobs, including private ones — unless the
  admin's mint narrowed it: the token carries a **repository policy** (`allow_all`, or up to 50
  `owner/name` / `owner/*` patterns) that the claim filter enforces. A repository admin cannot
  register a runner, and cannot see the fleet: `GET /api/v1/admin/runners` is host-admin only (a
  repo admin gets 403, an anonymous caller 401).
- The **runner host is trusted** with the code it executes. Fork pull-request runs stay exactly what
  they were: the base branch's workflow, a read-only checkout, and no secrets. Registration grants no
  access to repository contents beyond the attempt tokens the server issues per job, which remain
  scoped to one repository and one attempt.
- **A runner with the docker socket is root-equivalent infrastructure.** Job containers are siblings
  on the node daemon (the runner must hold the socket to spawn them), so whoever controls a job's
  `docker create` argv controls the host. Workflow container options and mounts are therefore
  allow-listed (resource/confinement flags only; no workflow supplies a volume), enforced at run
  creation and re-checked by the runner before any container is created (`docs/threads.md` §8.7).
- **Fork code needs an opted-in runner.** `allow_fork_runs` (flag `--allow-fork-runs`, default off)
  is recorded at register/declare; the global hint and the per-repository claim both withhold a fork
  job from a runner that did not opt in, and such a job stays queued when no runner qualifies. A
  fork runner belongs on a disposable host; trusted and fork jobs must not share one. A container
  escape on a shared host remains residual risk — the isolation card (daemonless/per-job daemons) is
  the durable fix and the socket mount is unchanged here.
- **Labels are exact and matched as a subset.** `--labels` is a comma-separated list stored
  verbatim; a job runs only on a runner that declares every label in its `runs-on`. The default is
  `self-hosted,linux,docker`; Forgejo-hosted labels (`docker, ubuntu-latest, linux_amd64`) are not a
  substitute.
- Revoking the runner is the off-switch: the next protocol call is a real 401, and no job is offered
  to it again. Registration tokens are single-use, hash-only at rest, and expire in 15 minutes.
- **The repository policy is admin-set and enforced at claim time.** The mint records `allow_all` +
  `repos` on the token and the registry record; the global hint skips a row outside the policy, the
  per-repository claim refuses the repository, and `claimEntry` re-checks under the CAS. A fork run
  needs both the repository allowed and `allow_fork_runs`. The runner cannot declare a policy
  (register/declare reject the field), and a record with no policy — written before tenancy — is
  offered **nothing**. An administrator changes a registered runner's policy in place (audited, no
  revoke/re-mint, no interruption of its current job); the next fetch reads the updated record.
- **The policy is tenancy, not isolation.** It bounds which repositories reach a host; it does not
  contain the job. The deployment recipe (dedicated runner host, daemon topology, rootless/gVisor/
  Kata options) is `docs/threads.md` §8.5.
- Runner groups/labels-as-tenancy (a named pool that is not offered every repository) are **not**
  part of this change; labels remain an execution match, not an authorization boundary. Tenancy is
  the admin-set per-runner repository policy below (a flat allowlist, not a group layer).

## 2. Storage shapes (instance scope)

```
threads/runners/<id>.json                 registry: name, labels, capacity, state, version,
                                          allow_all + repos (admin-set tenancy), registered_by,
                                          created/registered/last_seen/revoked,
                                          current_job{repo,run,job} + lease_until while leased
threads/runners/tokens/<id>.json          single-use registration token: SHA-256 hash only,
                                          allow_all + repos (what the mint authorized),
                                          created_by, expires_at, consumed_at, revoked_at
threads/runners/credentials/<id>.json     long-lived runner credential: hash only, revocable,
                                          names the runner id (never a repository)
threads/queue/index.json                  CAS'd global dispatch hint (below)
```

The per-repository families `repos/<o>/<r>/threads/runners/**` and
`users/<principal>/tokens/threads/<id>.json` are **deleted**. They are not read, not migrated, and not
swept as a compatibility path: an old repository-scoped registration is simply an unknown credential.

Registry list is one bounded prefix LIST over `threads/runners/` filtered to direct `.json` children
(never on a request hot path; the admin page and CLI are human-rate). Login credential and liveness
stay exact-key GET/CAS.

`current_job` is the most recent lease the runner took (a capacity-N runner reports the latest claim);
it is cleared on the terminal transition of that job, on requeue, and on revocation it remains until
the job terminalises or its lease lapses. It is display state, never authority.

## 3. Queue index — a hint, never the authority

`threads/queue/index.json`:

```json
{"version": 4, "queued": [{"repo":"o/r","run":"r_…","job":"j-build","labels":["linux"],"created_at":"…"}], "updated_at":"…"}
```

- Written at **enqueue** (one CAS: read + conditional PUT), cleaned on **claim** and on **terminal**
  transitions, and bounded at **10,000 rows**. A full index refuses the append with `ErrUnavailable`;
  the caller logs it and the job stays queued (run creation never fails because the hint is full).
- It is **only a hint**: the per-repository queue entry CAS remains the sole claim authority. A stale
  row is skipped (its repo probe finds nothing, or finds another claimable job) and swept by the
  reconciler. The index never grants a claim; it only tells a fetch which repositories are worth
  probing.
- A global fetch walks rows oldest-first, filters by the runner's label superset, and probes up to
  `queueGlobalProbeMax` (16) distinct repositories per poll. Within a repository it runs the existing
  per-repo claim loop (head probe, label match, `needs:` gate, concurrency admission, entry CAS), so
  fairness and correctness are exactly the proven path. Blocked entries are remembered for the poll
  and released; the long-poll wakes on any enqueue.
- **Cost.** The index append adds two store round trips to run creation (a GET and a conditional PUT),
  off the client's push path (run creation is already detached). The claim path still costs one index
  GET plus one head GET per probed repository and the entry CAS — no LIST. The index is one object,
  so concurrent enqueues contend on its CAS; retries are bounded at five, a loss is logged, and the
  job remains queued for the reconciler.
- **Reconciler.** `threads-queue-reconcile` (maintainer task, per repository, every five minutes)
  LISTs that repository's queue prefix (LIST only inside the task), rebuilds its rows from `queued`
  entries, and CAS-merges them into the index. It repairs a lost append, not a lost job.

### Concurrency (queue index)

Hazard: two enqueues overwrite each other's row. Avoidance: every mutation is a CAS on the single
index object with bounded retries; the loser re-reads. Hazard: a lost append hides queued work.
Avoidance: the index is a hint, never authority — the reconciler rebuilds it from per-repo entries,
and a fetch that misses the row still leaves the job in its repository queue. Hazard: a corrupt index
breaks every claim. Avoidance: read errors surface as 503/500 on fetch (never a silent 204), and the
reconciler rewrites the document from authoritative entries. No lock is held across a store call;
the protocol has no in-process shared mutable state beyond the wake hub (channels, sender-closed).

## 4. Admin and runner protocol

Host-admin JSON (both are ordinary authenticated API calls; the host-admin gate is in the service):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/admin/runners/tokens` | mint `{name?, allow_all?, repos?}` → `{id, token, expires_at, policy}` (shown once). At least one of `allow_all`/`repos` is required |
| GET | `/api/v1/admin/runners` | fleet: id, name, labels, capacity, state, allow_all/repos (tenancy), allow_fork_runs, last seen, current job |
| POST | `/api/v1/admin/runners/{id}/policy` | change one registered runner's tenancy: `{allow_all?, repos?}` → `{id, policy}` (same rule as the mint; revoked → 409) |
| DELETE | `/api/v1/admin/runners/{id}` | revoke (audited) |

Runner protocol (unchanged job endpoints; global registration/claim):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/threads/runners/register` | `{token, name?, labels, capacity, allow_fork_runs}` → instance credential (no repo, no policy field) |
| POST | `/api/v1/threads/runners/declare` | `{version, labels, capacity, allow_fork_runs}` — liveness + what the runner is (declare is idempotent: a restart without the flag turns the opt-in off) |
| POST | `/api/v1/threads/runners/fetch` | global long-poll (≤30 s); oldest matching job; 204 on timeout |
| … | `/api/v1/threads/jobs/{ref}/attempt/{n}/…` | unchanged |

Deleted: `/{o}/{r}/api/threads/runners`, `…/runners/tokens`, `…/runners/{id}`, and the per-repo
`MintRegistrationToken`/list/revoke service paths, the Settings → Runners tab and its partial.

Admin mint, policy edit and revoke write one record each in the host audit family (actor,
`runner_token_mint` / `runner_policy` / `runner_revoke`, `runner:<id>`, detail). The mint's and the
edit's detail carry the policy (`policy=all repositories` or `policy=owner/name,owner/*`), so the
tenancy an admin granted is recoverable from the audit trail alone. Audit failure is logged and never
reverts the committed change (the audit-after-commit rule).

## 5. Admin UI

`/admin/runners` is a normal Administration page (nav + page, the area's one host-admin gate).
It renders: the mint form (optional name, plus the repository policy — an "all repositories" checkbox
and a pattern list, with the tenancy rule stated beside it), the one-time token with its expiry and a
"shown once" warning, the fleet table (name, id, labels, **repository policy**, **fork runs**,
capacity, state, last seen, current job, policy edit + revoke), and an empty state. A policy and trust
class are text, never colour alone: `all repositories` / the pattern list / `none — no repository
allowed` (a pre-tenancy record), and `off — trusted code only` / `on — untrusted fork code`.

Each live row carries a no-JS **change-policy** control — a `<details>` form naming the runner, with
the allow-all checkbox and the patterns field prefilled from the recorded policy. Submitting posts
`/admin/runners/policy` and redirects back to the fleet, which then shows the new policy (or the
service's validation message with the form reopened and the typed values kept). A revoked row carries
neither the edit nor the revoke control. It composes `docs/style-guideline.md` classes, has one `h1`,
scrolls tables internally at ~390 px, works in both themes, and revoke asks for confirmation like the
other destructive actions.
Settings → Runners is removed from `settingsRepoTabs`, its dispatch line and partial are deleted, and
`TestSettingsTabsArePartials` continues to pin the layout rule.

The page is verified in a real browser at desktop and ~390 px (layout, no page-level horizontal pan,
the checkbox/textarea pair, the policy column readable at the narrow width).

Administration is **browser-only by design**. The CLI's API client speaks static machine credentials
(`resolveToken`), and a static token cannot administer (AGENTS law 9), so there are deliberately no
`spindle runner token|list|revoke` verbs: they could only 403. The page and the JSON API share the
OIDC-session host-admin gate; `runner serve --register` is the operator step after a browser mint.

## 6. CLI

```
spindle runner serve [--register TOKEN] [--labels a,b] [--capacity N] [--allow-fork-runs] …
```

Only `serve` exists. Mint/list/revoke are browser-only (see §5): the CLI has no admin credential
path, and inventing a static-admin exception would violate the OIDC-only rule. `runner serve` loses
any repository assumption: the registration/credential it holds is instance-scoped, and every fetch
is a global claim. `--labels`, `--capacity` and `--allow-fork-runs` are declared to the server
(declare), not stored on a repository. Labels are exact, comma-separated and subset-matched against
`runs-on`.

## 7. Rollout — CI stops between merge and re-registration

There is no automatic migration and no shim. The sequence is:

1. Merge (the repo-scoped families stop being read; old runner credentials are unknown).
2. A host admin mints an instance token in the browser at `/admin/runners`.
3. The CI runner is upgraded to the new binary and restarted with
   `spindle runner serve --register <token> --labels … --capacity …`.
4. CI resumes.

**CI is down between step 1 and step 3.** The running CI runner must be updated with the new binary
and token; an old process keeps polling a route that is gone (or presents a credential the server no
longer recognises) and exits/fails. Plan the restart in the same maintenance window as the deploy.

## 7b. Fork-run gating and workflow argv (security hotfix, 2026-11-22)

- `RunnerDoc.allow_fork_runs` (default false) is recorded by register/declare and shown in the fleet.
- `QueueEntry.fork` and the queue-index row's `fork` are copied from the job at enqueue. The global
  fetch skips a fork row for a runner that did not opt in, and `claimEntry` re-checks the loaded run
  under the CAS (the authority); the runner also refuses a fork job it is handed anyway (reported as
  a failed attempt with the reason). A fork job with no eligible runner stays queued.
- Workflow `container.options`/`volumes` and `services.*.options`/`volumes` pass one secure-by-default
  allowlist (resource/confinement flags; no workflow volumes) at run creation/rerun and again in the
  runner's plan, before any container exists. See `docs/threads.md` §8.7 for the list and the trust
  boundary.
- Label examples were corrected: Spindle's default labels are `self-hosted,linux,docker`; a Forgejo
  runner declares `docker, ubuntu-latest, linux_amd64`, so only `docker` overlaps and Forgejo-shaped
  examples never matched.

## 8. Tenancy: the repository policy, and what stays deferred

A runner's **repository policy** is the admin-set allowlist the mint carries (`allow_all` or up to 50
`owner/name` / `owner/*` patterns). It is the answer to "which repositories may this host be offered"
without inventing a group layer: the policy travels on the token, is copied to the registry record at
registration, is shown in the fleet, and is enforced before labels in the claim filter (hint skip +
per-repository refusal + a re-check under the claim CAS). It is **tenancy, not isolation**: a
compromised runner still sees the code of every repository its policy allows, and a runner with a
daemon socket is root-equivalent on its host — `docs/threads.md` §8.5 is the deployment recipe for
that boundary.

The policy is not frozen at the mint. `SetRunnerPolicy` rewrites only `allow_all`/`repos` on the
registry record (one CAS, a no-op when unchanged), refuses a revoked runner, audits the actor and the
resulting policy, and leaves the current job and its lease alone; both admin lanes reach it
(`POST /admin/runners/policy` and `POST /api/v1/admin/runners/{id}/policy`). Tenancy is therefore a
live knob an administrator turns, not a decision that costs a runner its registration.

Runner **groups** — named pools with their own registry and membership, so several runners share one
grant — remain deliberately out of scope. The per-runner policy is the smaller shape that makes
instance-scoped runners safe to hand out; a group layer would add a per-group registry, a
queue-index group key and a second claim filter on top of it, and that shape is still undecided.

## 9. Tests (definition of done)

Host-admin-only mint (repo admin 403, anonymous 401); single-use + TTL tokens; registration yields an
instance credential and declares labels/capacity; global claim across two repositories; two runners
racing one job with exactly one winner via the per-repo CAS; non-matching label 204; revoke stops
claims; last-seen/current-job heartbeat; index append/cleanup/reconcile; the 10k bound; audit records
for mint/revoke; the repo Settings tab is gone and the structural settings test still passes; both
lanes for the admin routes; the admin page render/gating/token-once/empty state/revoke confirmation.
The hotfix adds: a fork run is not claimed by a default runner (global hint and per-repo CAS) and is
claimed after `allow_fork_runs`; a same-repo run is unaffected; register/declare carry the field; each
container-option attack string is refused at creation (failed job + red check + no enqueue) and at
plan time (no docker call), and the benign resource set passes.
The tenancy change adds: the policy carried from the mint to the registry record (normalised, and
surviving declare); claim allows exact / `owner/*` / `allow_all` and refuses everything else, including
a record with no policy; fork composes with the policy (allowed repo *and* opt-in); an invalid, empty
or over-long pattern list is refused; the runner cannot self-declare a policy (register/declare are a
400 and change nothing); the mint audit carries the policy; the fleet page renders both policy states
and both trust classes; both admin JSON lanes mint with a policy and both lanes refuse a policy-less
mint.
The in-place edit adds: an active and a pending runner are editable and a revoked one is refused
(unchanged record); the edit rewrites only the policy, preserves every other field including the
current job and its lease, and is a no-op when the policy is unchanged; empty / malformed / over-long
lists are the mint's own errors; concurrent edits leave a valid record (one of the submitted policies);
the audit entry names the actor, the runner and the resulting policy; both lanes answer 401 / 403 /
200 and refuse unknown fields, a policy-less body and a revoked runner; the page renders one form per
live row (prefilled, naming the runner), redirects on success and reopens the row with the service's
message on a refusal; a claim the policy refused is offered after the edit, and a job already running
keeps its lease and its record.
`-race` on `threads`, `api` and `webui`; the ≥95 % per-package coverage gate holds.

## 10. Decisions (one line each)

- **Instance over repository scope.** A runner is infrastructure registered once by a host admin,
  not a repository member; labels route execution, never tenancy.
- **Host-admin mint is the authorization.** Registration tokens are hash-only, single-use and
  15-minute-lived; a repo admin cannot mint or see the fleet.
- **Administration is browser-only.** The CLI's machine credentials cannot administer, so no
  `runner token|list|revoke` verbs exist; `runner serve --register` follows a browser mint.
- **The queue index is a hint.** A single CAS'd candidate document (10k rows) saves a global fetch
  from LISTing; the per-repository queue-entry CAS remains the only claim authority.
- **One extra CAS per enqueue.** Two store round trips (read + conditional write) off the client's
  push path; a lost write is logged and repaired by `threads-queue-reconcile`, never a lost job.
- **Bounded global probing.** A fetch probes at most 16 distinct repositories from the hint,
  oldest-first, and walks the existing per-repo claim loop inside each.
- **Delete, do not migrate.** The repo-scoped registry and `users/…/tokens/threads/…` credential
  families are unread; old credentials are unknown and CI stops until re-registration.
- **Capacity is declared.** `declare` records labels and capacity (1–64); it stays an operator flag,
  never a repository setting.
- **Revoke is the off-switch.** The registry, credential and unused token are stamped revoked; the
  next protocol call is a real 401.
- **Audit after commit.** Mint/revoke write `runner_token_mint`/`runner_revoke` in the host audit
  family; an audit failure is logged and never reverts the change.
- **Workflow argv is allow-listed; fork runs are opt-in.** Container options/mounts are
  resource/confinement flags only, enforced at creation and re-checked at plan time; a runner records
  `allow_fork_runs` and is never offered fork code without it (a job with no eligible runner stays
  queued). A runner with the docker socket is root-equivalent infrastructure; a shared-host container
  escape is residual risk left to the isolation card.
- **Labels are exact and subset-matched.** `self-hosted,linux,docker` is the declared default; a job
  needs every label in its `runs-on`, and Forgejo-hosted label sets do not match.
- **Runner groups are deferred; the repository policy is not.** A per-runner admin allowlist
  (`allow_all` or `owner/name`/`owner/*`, at most 50) is enforced in the claim filter and shown in the
  fleet; a named group layer with shared grants stays undecided.
- **Tenancy is not isolation.** The policy decides which repositories reach a host; a runner with a
  daemon socket stays root-equivalent on that host, and the dedicated-worker recipe (`docs/threads.md`
  §8.5) — separate host, local or TLS daemon, rootless/gVisor/Kata options — is the isolation answer.
- **Policy change is an audited admin edit, not a re-mint.** `SetRunnerPolicy` CASes only
  `allow_all`/`repos` on the registry record (refusing a revoked runner, preserving the current job
  and its lease), so tenancy can be widened or narrowed on a working fleet; both admin lanes reach
  the one service call and every accepted edit records `runner_policy` with the resulting policy. The
  mint remains the authorization for a NEW runner; revoke remains the off-switch. (Supersedes the
  earlier revoke-and-re-mint-only decision — the hostile path for a fleet runner, and impossible
  without killing its in-flight job.)
- **The policy-aware mint has its own seam.** `AdminDeps.MintRunner` (admin.go) is a name-only mint and
  cannot carry tenancy, so the page reads `webui.AdminRunnerTenancyDeps.Mint` (admin_runners.go,
  bound by composition) instead; the name-only field is not rendered and not bound.
