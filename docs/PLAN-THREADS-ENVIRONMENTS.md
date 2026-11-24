# PLAN — Threads deployment environments

Status: implemented (2026-11-23). Scope: a repository-scoped **environment** record that a job can
target, the **required reviewers** who must approve a deployment to it, and **secrets scoped to the
environment** that reach only an approved job. It is the last Threads ecosystem slice: the CI surface
gains the one thing a build could not previously express — "this job deploys, and a named reviewer
has to say yes before it does".

Read with: [`features/15_threads.md`](features/15_threads.md) §2.7/§8, [`threads.md`](threads.md)
§3.3/§5.6 (the user-facing contract), `docs/go/06_server_http.md` (routes),
`docs/go/02_storage_protobuf.md` (the key family), `docs/go/12_web_ui.md` (the settings tab).

## 1. What the workflow declares

`jobs.<key>.environment:` accepts the two GitHub spellings and nothing else:

```yaml
jobs:
  ship:
    runs-on: [self-hosted, linux, docker]
    environment: production                  # bare name
  ship-canary:
    environment:                             # name + the URL the run page links
      name: staging
      url: https://staging.example.test/
```

- The **name** matches `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. It is a bucket key segment and a URL path
  segment, so the parser pins the charset; anything else is that file's parse error
  (`workflow.ErrBadEnvironment`), never a silently ignored key.
- The **URL** must be absolute `http`/`https` (an empty URL is fine). A run page renders it as a
  link, so a scheme the page could execute is a parse error rather than a stored footgun.
- A job with **no** `environment:` is byte-for-byte unchanged: no record read, no deployment record,
  no gate. The cost model is "one exact-key GET per environment-naming job", and zero otherwise.
- Validation is the parser's, so the run fails at dispatch/creation like any other malformed field;
  the environment's *existence* and its *reviewers* are server state and are decided at run creation
  (§5), because they can change between the push and the run.

## 2. The record

One CAS'd JSON object per environment, in the repository's Threads subtree:

```
repos/<o>/<r>/threads/environments/<name>.json
{
  "name": "production",
  "required_reviewers": ["ada", "grace"],        // principals, lowercased, sorted
  "secrets": ["DEPLOY_TOKEN"],                   // the allowlist of secret NAMES
  "created_by": "maint", "created_at": "<RFC3339>",
  "updated_at": "<RFC3339>", "version": 3
}
```

- **It lives in the bucket**, like every other repository fact (law 4): the record is the commit
  point, `version` is the CAS token, and a restart loses nothing. There is no in-memory registry and
  no local file.
- **`required_reviewers` is validated at save time**: every name must resolve to at least **write**
  in the repository (a repository role binding, or an instance admin). A name nobody can resolve to
  write is `ErrInvalid` with the name in the message — never a stored reviewer a deployment would
  then wait for forever. The list is deduped, lowercased and sorted; at most 10 entries.
- **`secrets` is a name list, not values.** Values live sealed under the environment's own prefix
  (§4). Keeping the allowlist on the record is what makes the job-payload path one exact-key GET per
  listed secret instead of a LIST (law 4/6: no LIST on a request path).
- **CRUD is a settings surface** (Settings → Environments, docs/go/12_web_ui.md): create/replace is
  one `PUT` of `{required_reviewers: [...]}` (an existing name is updated in place, `version++`),
  and delete removes the record *and* its sealed secrets (secrets first, record last, so a crash
  leaves a record that still names them rather than orphaned ciphertext). The floor is **maintain +
  repository admin (instance admin included)**; every read (which names secret names) is the same
  floor.
- **The API is the page's twin**: `GET/PUT/DELETE {lane}/threads/environments[/{name}]` and the
  secret endpoints below, on both `api` and `api-browser` lanes, same service calls, same floor.

## 3. Why the name must be declared

A job naming an environment the repository does not define **fails** with a named reason
(`environment "production" is not defined in this repository (create it in Settings → Environments)`)
instead of running. The alternative — treat an unknown name as "no deployment" — makes a typo look
like a successful deploy of nothing. The failure is a *failed job* (the run's check goes red), not a
silently disabled workflow, and the rest of the workflow still runs.

## 4. Secrets

Environment secrets are stored exactly like repository secrets — AES-256-GCM under
`server.secrets_key` (falling back to `server.auth.session_secret`) — with an **AAD that binds the
ciphertext to (repository, environment, name)**
(`spindle/threads/environment-secret/v1\0<repo>\0<env>\0<name>`). A sealed value therefore cannot be
replayed as a repository secret, under another environment, or under another name.

```
repos/<o>/<r>/threads/environments/<name>/secrets/<SECRET>.json
{ "name": "DEPLOY_TOKEN", "sealed": "<base64 nonce||ciphertext>",
  "created_by": "…", "created_at": "…", "updated_at": "…", "version": 2 }
```

- **Write-only**, like the repository secrets: the API accepts a value and never returns it; reads
  report names and timestamps; the settings page renders `•••••• (set)`.
- **Names** match `[A-Za-z_][A-Za-z0-9_]{0,63}`; values are 1 byte – 64 KiB. A write adds the name to
  the record's allowlist (the record is updated first) and a delete removes both.
- **Resolution** is one exact-key GET of the record plus one per listed name. A name whose ciphertext
  is absent is **skipped** (never injected as an empty value); an unreadable envelope fails the fetch
  with a clear error (fail closed — a job that silently lost the credential it deploys with is worse
  than an operator-visible failure).
- **Injection rule**: environment secrets are resolved only for a job that names the environment,
  only for a **trusted** run, and only when its deployment is **approved** (`ApprovalKind` empty and
  no reviewers → `not_required` counts as approved). `fetchPayload` re-checks this: a claim that
  reaches it without an approved deployment record is refused (`deployment to environment "x" is
  not approved`), so no queue race can run a deployment nobody approved.
- **Precedence** is GitHub's: the merged map starts from the repository secrets and the environment's
  secrets **override by name**; the rest of the repository set is still present. Both sets are in the
  log masker (best effort, the documented repository-secret limitation).
- Without a sealing key the store fails closed: the secret endpoints answer `503`, the settings panel
  explains which key to set, and the *environment record* (name + reviewers) still works.

## 5. The reviewer gate

A run containing at least one job that names an environment with required reviewers is created in
**`pending_approval`** — jobs written, checks mirrored `pending`, **nothing enqueued**, so no runner
can see it — reusing the first-time-contributor gate's state machine (one `pending_approval` state,
one approve/deny pair, one CAS claim of the decision). The run records which gate held it:

```json
{ "state": "pending_approval", "approval_kind": "environment",
  "environments": ["production"] }
```

- **Distinct kind**: the run page, the API (`approval.kind`, `approval.environments`,
  `approval.reviewers`, `approval.can_decide`) and the checks description (`… · environment
  production …`) name the environment, so a held deployment never reads as a generic fork hold.
- **Who may decide**: a **repo admin** (or instance admin) always; otherwise a principal listed as a
  required reviewer of **every** gated environment named by the run, holding at least **write**.
  Approval re-reads the live records, so a reviewer removed after creation (or an environment
  deleted) cannot be bypassed by a stale page. A read/anonymous principal can never decide.
- **Approve** CASes the header to `queued` (stamping `approved_by`/`approved_at`), stamps each gated
  job's deployment record `approved` with the same principal and time, then enqueues every still-
  queued job — one enqueue path, no second scheduler. An environment deleted between creation and
  approval **fails that job closed** (`… the environment no longer exists`) while the rest of the run
  proceeds.
- **Deny** stamps the header (`denied_by`/`denied_at`, never a release), stamps each gated deployment
  `denied`, **fails those jobs** with `deployment to environment "x" denied by <who>`, and lets the
  non-deployment jobs continue, so a refused deploy reads `failure` beside the build rather than
  erasing it. A denied run can never be approved afterwards; a **re-run** is a new decision.
- **A second decision is `409`.** A run held by an environment hold outranks the fork hold (one hold,
  the stricter reviewer rule): a fork run naming a reviewed environment waits for that environment's
  reviewers, not the fork gate's generic maintainer.
- **Re-runs re-apply the gate**: the deployment record is re-decided against the current record
  (`pending` again), the header's approval stamps are cleared and `pending_approval` returns. A
  record created or tightened after the first run cannot be bypassed by the re-run button.
- **No reviewer available → stays pending, forever.** There is no timeout that auto-approves and no
  "admin of last resort" path beyond the repo/instance admin rule: an unreviewed deployment simply
  never runs (fail closed). The page names the reviewers it is waiting for, so the stuck state is
  visible rather than mysterious.

## 6. Fork and trust rules

- A fork run is never **trusted** (15 §5.2), and environment secrets are resolved only for a trusted
  run — so even an approved fork deployment receives `secrets: {}` and `secrets_denied: true`.
- A fork run's actor holds no write role in the base repository, so it cannot approve its own
  deployment: the decision needs a listed reviewer or a repo admin, neither of which a fork actor is.
- A fork run naming an environment **with** required reviewers is created `pending_approval`
  (the gate is on the deployment, not the trust class) and stays held until a reviewer decides, or
  fails closed if nobody does. A fork run naming an environment **without** reviewers runs like any
  fork job — with no secrets.
- An environment record is repository state: a fork cannot create one, change its reviewers, or read
  its secret names (the surface is maintain+ in the base repository).

## 7. The deployment record

A job that names an environment carries a **deployment record on the job** (no separate bucket
family, so a read page and the API see the same fact and retention deletes it with the job):

```json
"deployment": { "environment": "production", "url": "https://prod.example.test/",
                "state": "approved", "approved_by": "ada", "approved_at": "<RFC3339>",
                "denied_by": "", "denied_at": null }
```

`state` is `not_required` (no reviewers), `pending`, `approved`, `denied` or `failed` (an environment
that vanished). The run page shows the environment name, the state, the approver (or denier) and time,
and the declared URL; together they answer "which environment, by whom, when" without reading the
workflow file again. **No secret value or name beyond the record's own allowlist renders here.**

## 8. Audit

Every mutation is recorded in the host audit family (the `Audit` seam composition binds to
`moderation.RecordAdmin`), after the commit and never reverting it:

| action | target | detail |
|---|---|---|
| `environment_create` / `environment_update` | `environment:<name>` | reviewer count |
| `environment_delete` | `environment:<name>` | number of sealed secrets removed |
| `environment_secret_set` / `environment_secret_delete` | `environment:<name>` | `secret=<NAME>` |
| `environment_approve` / `environment_deny` | `environment:<name>` | `run=<id>` + the decider |

The detail never contains a secret value, and a read is never audited (no invented events).

## 9. Routes

Both JSON lanes (`/{o}/{r}/api[-browser]/threads/…`), all maintain+:

| Method | Path | Answer |
|---|---|---|
| `GET` | `…/environments` | `{environments:[{name, required_reviewers, secrets, created_at, updated_at, version}], enabled}` |
| `GET` | `…/environments/{name}` | `{environment: {…}}` (`404` unknown) |
| `PUT` | `…/environments/{name}` | `{required_reviewers:[…]}` → `{environment: {…}}` |
| `DELETE` | `…/environments/{name}` | `204` |
| `GET` | `…/environments/{name}/secrets` | `{secrets:[{name, created_at, updated_at}], enabled}` |
| `PUT` | `…/environments/{name}/secrets/{secret}` | `{"value":"…"}` → `204` |
| `DELETE` | `…/environments/{name}/secrets/{secret}` | `204` |

The settings page calls the same service methods, so the page and its API twin cannot drift.

## 10. Concurrency

The record's CAS is the only lock: two maintainers saving the same environment serialize on
`version`, and a reviewer's decision claims the run header with a CAS **before** any enqueue, so two
approvers enqueue once and a second decision is `409`. Secret writes CAS the record's allowlist and
the sealed doc separately (record first): the failure window leaves a listed-but-absent name, which
resolution skips, never a ciphertext nobody can find. No lock is held across a store call, and
nothing is cached: a reviewer removed a second ago is removed for the next decision (law 4).

## 11. Tests

`internal/threads` (`environments_test.go`, `http_environments_test.go`): parser shapes and the
parse errors (in `internal/threads/workflow`); record CRUD, the role floor, reviewer validation,
dedupe/caps; the pending → approved → job-starts flow with the payload's environment secret; the
non-reviewer refusal and `409`; deny (job fails with the named reason, no queue entry, run continues);
undefined-environment failure; deletion while pending; re-run re-holds; secret precedence over
repository secrets; the fork rule (held, cannot self-approve, no secrets even when approved); audit;
the API on both lanes including the write-only value; and the record's CAS/audit failure paths.
`internal/webui` (`environments_webui_test.go`): the tab renders names/reviewers/secret names and no
value, the maintain floor on GET and POST, the honest "not wired" card, and the run page's
environment surface (banner, reviewers, URL, approve control only for a principal the service allows).
The structural settings-tab test (`TestSettingsTabsArePartials`) stays green because the body is one
`settings_repo_environments.html` partial plus one dispatch line.

Verification ladder for this change: `make fmt vet test race-pr cover`; the page was verified in a
real browser at desktop and ~390 px in both themes. What could **not** be exercised: an actual
runner executing an approved deployment (the runner is not registered in this environment), so the
"job starts" half is asserted at the queue/payload seam (`claimLoop` + `fetchPayload`), which is the
same seam the runner uses.

## 12. Deviations and limits

- **Run-level hold, not per-job approval.** The existing `pending_approval` state machine is
  run-scoped, so one run with two environment-naming jobs holds for the intersection of their
  reviewers (a repo admin always may). A per-job deployment queue (GitHub's model) would need a
  second state machine; this slice deliberately reuses the one that exists, and documents the
  intersection rule.
- **`required_reviewers` is validated at save, re-read at decision** — a reviewer who loses write
  access *after* the save can still decide while listed (the verb re-checks only the write floor).
  Removing them is one settings save.
- **Self-approval is allowed for a listed reviewer** who is also the actor (a maintainer pushing the
  release and named as its reviewer declared exactly that). Fork actors can never be listed.
- **No environment-level protection rules** (wait timers, branch filters, deployment branches): out
  of scope; the record carries reviewers and secrets only.
- **No new configuration key**: availability follows `server.secrets_key` /
  `server.auth.session_secret`, exactly like repository secrets.
- **The environment is not exposed to the runner** beyond the URL on the record; the runner executes
  a job, and the server owns the gate, the secrets and the record.
