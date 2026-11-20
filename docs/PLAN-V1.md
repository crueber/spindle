# V1 wrap-up plan

> **Status:** proposed (2026-11-20). Audits the README's v1 requirements against the shipped code and
> against GitHub's behaviour, then specifies the work that closes v1. This is a *release* plan; the
> feature contracts stay in `docs/features/` and the build order stays in `docs/go/README.md`.
> Relationship to existing docs: `docs/features/09_rollout.md` is the **as-built record** of the
> collaboration layer (waves A–E, landed 2026-09-04). It is not a v1 release plan and does not
> describe GitHub parity; where this document and 09 disagree, 09 is history and this is the plan.

## 0. Definition of done for v1

v1 ships when **every checkbox under "v1 release requirements" in `README.md` is true and each item
meets the GitHub-parity bar in §4**, with the verification in §6. "Parity" means: a user who knows
GitHub can do the same task in spindle through the web UI (or the documented API/SDK), and the
behaviour is specified and tested — not that every GitHub feature is copied.

**Scope decisions taken 2026-11-20** (they bound everything below): OCI/container is the only
package ecosystem in v1; release source archives and asset download counts are v1; **every
user-facing setting and policy must be editable in the UI** — raw JSON is not a v1 surface; the GCS
backend is removed rather than implemented; "sync fork" is v1; the registry reuses the existing
token/OIDC identity with repository-role scopes and introduces no second credential type.

Deliberately **not** v1 (README `v1.1` / `Backlog`, and P9): Actions runners, code search, projects,
wiki, insights, moderation, sponsorships, forum, ownership transfer, SAML/SCIM, merge queue,
auto-merge, stacked PRs, Discussions.

## 1. Audit — README v1 list vs the code (2026-11-20)

| # | Item | README | Reality | Evidence |
|---|---|---|---|---|
| 1 | git storage | ✅ | **shipped** | `internal/store` (fs/s3/memory), `internal/wal`, `internal/git`, smart v0/v2, LFS, bundles, SSH |
| 2 | issues | ✅ | **shipped** | `internal/issues` + API + pages + SDK; assignees are API-only in the UI |
| 3 | pull requests | ⬜ **stale** | **shipped** | `internal/pulls` + `internal/review`; open/get/diff/commits/comments/merge/merge-task/update-branch/head/forks/review-requests/threads; pages, SDK, e2e |
| 4 | tags | ✅ | **shipped (thin)** | `internal/tags` (light/annotated); no browse page, no delete |
| 5 | releases | ⬜ **stale** | **shipped** | `internal/releases`: draft/publish, prerelease, latest, assets (Range/ETag), pages, SDK, e2e |
| 6 | packages | ⬜ | **absent** | no registry of any kind (see §5.1 and `docs/features/14_packages.md`) |
| 7 | fork | ✅ | **shipped** | `internal/pulls/forks.go`, `forkexec.go` (manifest-sharing landed, #424); no "sync fork" |
| 8 | webhooks | ✅ | **shipped** | `internal/notify/webhooks.go` (HMAC, SSRF guard, retry, delivery ring); org hooks API-only |
| 9 | oidc | ✅ | **shipped** | discovery, PKCE, nonce, signed state, ID-token verify; no claim→team mapping |

Two README corrections follow from this table: **pull requests and releases are done** (ticked in this
change), and `packages` is the only unchecked v1 item with no implementation.

## 2. GitHub-parity matrix

Each row states GitHub's behaviour as the reference, what spindle does today, and the delta this plan
must close. Rows marked **—** are at parity already; they stay in the table as the acceptance record.

### 2.1 Pull requests

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Open from a branch, same repo | yes | yes | — |
| Open from a fork (cross-owner) | yes | API/SDK only; UI select limited to the base owner | **WP2.1** UI fork selection |
| Draft / ready-for-review | yes | yes | — |
| Conversation, comments, timeline, reactions | yes | yes | — |
| Commits / files changed, unified + split, line comments, suggestions | yes | yes | — |
| Reviews: approve / request changes / comment; threads resolve | yes | yes | — |
| Required checks gate | yes | yes (policy) | — |
| Merge: merge commit / squash / rebase; delete branch after | yes | yes | — |
| Update branch (merge base into head) | yes | yes | — |
| Labels / milestone / assignees on the PR | yes | thread model has `Labels`; no editing endpoint | **WP2.2** PR metadata UI + API |
| Branch protection rules UI (required reviews, status checks, who may push) | yes | raw policy JSON only | **WP2.3** (v1 — everything is editable in the UI) |
| Auto-merge, merge queue, stacked PRs | yes | no | **non-goal** |

### 2.2 Releases

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Release on an existing tag, title, markdown notes | yes | yes | — |
| Draft / prerelease / latest | yes | yes | — |
| Assets: upload, download (Range/ETag), delete | yes | yes | — |
| Auto-generated release notes from merged PRs | yes | yes (autodraft) | — |
| Auto-built source archives (zip, tar.gz) | yes | no | **WP3.1** |
| Download counts per asset | yes | no | **WP3.2** |
| Draft that creates its tag on publish | yes | drafts require an existing tag | **WP3.3** |

### 2.3 Tags

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Lightweight + annotated tags | yes | yes | — |
| Tag list page with commit, date, author | yes | count only in the repo header | **WP4.1** |
| Delete a tag (with policy check) | yes | no | **WP4.2** |
| Tag protection rules | yes | backlog (`protect` policy covers it) | **non-goal for v1** |

### 2.4 Fork

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Fork a repository (manifest-sharing, no copy) | yes | yes | — |
| Fork network listing | yes | yes | — |
| PR from a fork | yes | API yes / UI gap in WP2.1 | **WP2.1** |
| Sync fork from upstream | yes | no (pull + push manually) | **WP5** |

### 2.5 Issues

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Create/edit/close/reopen, comments, timeline, reactions | yes | yes | — |
| Labels, milestones | yes | yes | — |
| Assignees | yes | API-writable, UI read-only | **WP6.1** |
| Cross-references and closing keywords | yes | yes (linkify + refs) | — |
| Search/filter | yes | list filters (state/assignee/label/milestone) | **non-goal** (code search is P9) |

### 2.6 Webhooks

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Repo hooks: create, ping, list deliveries, delete | yes | yes | — |
| Secret + HMAC signature | yes | yes (`X-Walgit-Signature`) | — |
| Enable/disable, rotate secret in the UI | yes | API PATCH only | **WP6.2** |
| Redeliver a delivery | yes | no | **WP6.2** |
| Org-level hooks in the UI | yes | API only | **WP6.2** |

### 2.7 OIDC

| Capability | GitHub (as org SSO) | spindle today | Delta |
|---|---|---|---|
| Login via OIDC discovery + PKCE | — | yes | — |
| Allowlist enforcement | — | yes | — |
| Claim → user attribute mapping | yes | username/email only | **acceptable for v1** |
| Claim → org/team membership | yes | manual membership | **non-goal** (SAML/SCIM is P9) |

### 2.8 Git storage

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Smart HTTP v0/v2, SSH, LFS, bundle-uri | yes | yes | — |
| Filesystem + S3 backends | — | yes | — |
| GCS backend | — | **advertised, not built** (`store.backend="gcs"` errors) | **WP7** — remove it (decision 2026-11-20) |
| Archive download (`/archive/<ref>.tar.gz`) | yes | no | **WP3.1** (shares the archive writer) |

## 3. Work packages

Each WP is one implementable unit with its own acceptance criteria (§6). Ordering is §4.

### WP1 — Packages: an OCI registry (the only new feature)

Full design: **`docs/features/14_packages.md`**. Summary: implement the OCI Distribution Spec over the
existing object store, namespaced per repository (`host/<owner>/<repo>`), authenticated with the
existing tokens/OIDC through a Docker token endpoint, with a repository Packages page, API + SDK, and
a maintainer GC pass. No npm/Maven/NuGet in v1 (Q1).

### WP2 — Pull-request parity gaps

- **WP2.1 Fork PRs from the UI.** The new-PR form's head selector currently offers only the base
  owner's branches (`forms.go:formsPullNewDataFor`). Add the fork list (existing `forks.json`) to the
  picker and pass `head_repo` through to the existing API path.
- **WP2.2 PR metadata.** Labels, milestone and assignees on a PR: extend the thread update endpoint
  the issues layer already uses, add the sidebar controls to `pull.html`, and cover them in the SDK.
- **WP2.3 Branch protection rules, in the UI.** A settings tab that edits the policy document for the
  rules a team actually uses — required approvals, required status checks, who may push (roles), and
  force-push/deletion restrictions — writing the same policy the API validates. Raw JSON is not
  offered as the only path (scope decision 2026-11-20).

### WP3 — Release and archive parity

- **WP3.1 Source archives.** `GET /{o}/{r}/archive/{ref}.{tar.gz,zip}` — server-rendered from git
  (`git archive` argv added to `docs/go/04_git.md`), streamed, ETag from the tree id, and linked from
  the release page and the repo page's "Download" affordance. Reuses one writer for releases.
- **WP3.2 Asset download counts.** A counter per asset updated on the byte route, shown on the
  release page and in the API. Cheap write: one CAS bump, best-effort, never on the ACK path.
- **WP3.3 Draft with a new tag.** Allow a draft release to declare `target` and create the tag on
  publish, matching GitHub's "choose a tag → create new tag on publish".

### WP4 — Tags

- **WP4.1 `/tags` page** listing tag, target commit, date, author, and the release it backs.
- **WP4.2 Delete tag** (`DELETE /api/.../refs/tags/<tag>`) gated by the `protect` policy effect, with
  the page action and SDK method.

### WP5 — Sync fork

A "Sync fork" action that fetches upstream and fast-forwards the fork's default branch when it is a
strict ancestor; otherwise it reports divergence and links to a PR. Uses the existing mirror/egress
machinery (`internal/egress`, `internal/mirror` presets) so SSRF rules are shared, and runs as a
`(repo, kind=sync-fork)` task with progress packets (law 7).

### WP6 — Issues and webhooks UI parity

- **WP6.1 Issue assignees in the UI** (the API and model already support them).
- **WP6.2 Org webhooks tab** (create/ping/deliveries/delete), enable/disable + secret rotation, and
  redeliver — the repo-level surface already does all of this, so this is a UI + one endpoint.

### WP7 — Remove GCS

GCS is not wanted (decision 2026-11-20); remove it everywhere it is advertised rather than implementing
it: `store.backend` enum + validation (`internal/config`), the `store.gcs` config struct, the setup
schema/UI entries, the `case "gcs"` branch in `cmd/spindle/serve.go`, the README and
`docs/go/03_store_backends.md`, and any test that names it. `DEVIATIONS.md` D-DEP-3's claim of a
hand-rolled GCS client is corrected in the same change (no `store/gcs.go` exists).

### WP9 — UI editability sweep (the "everything does" rule)

The scope decision makes UI editability a v1 property, so one pass audits every user-facing setting
and policy for an API-only path and closes what it finds. The audit already named four (PR
labels/milestone/assignees in WP2.2, protection rules in WP2.3, issue assignees in WP6.1, org
webhooks in WP6.2); this WP verifies there are no others (mirror settings, push-mirror auth, token
scopes, check defaults, per-repo features) and records the result as a table in this document.

### WP8 — Documentation and checklist reconciliation (landed with this plan)

README checkboxes corrected; `03_pull_requests.md` status fixed; `docs/features/README.md` P9 amended
(packages is now in scope for v1, pointing at 14_packages.md); `09_rollout.md`'s deferred items
corrected (fork manifest-sharing landed); this plan linked from the README.

## 4. Sequencing

| Milestone | Contents | Why this order |
|---|---|---|
| **V1-alpha** | WP8 (docs truth), WP7 (decide GCS), WP1 skeleton: `/v2/` ping, blob/manifest PUT/GET, token auth, `docker push/pull` of a single-image repo | Packages is the largest risk; prove the protocol first |
| **V1-beta** | WP1 complete (tags/list, delete, GC, UI page, API/SDK, feature toggle), WP2.1, WP3.1 | A user can push an image, open a fork PR, and download a release tarball |
| **V1-rc** | WP2.2, WP2.3, WP3.2/3.3, WP4, WP5, WP6, WP9 | Parity gaps that do not gate the headline features; WP9 proves the editability rule |
| **V1** | Full ladder green, `docs/EVIDENCE.md` entry for the registry hot paths, README all-ticked, release button | Ship |

WP1 and WP3.1 are independent and can run in parallel; WP2 is independent of both; WP5 depends on
nothing in WP1–WP4 but should follow V1-beta so the egress machinery is settled.

## 5. Design notes that constrain every WP

- **The bucket is the repository (law 4).** Every WP above stores its state in the object store under a
  documented prefix; nothing on local disk or in the WAL except what the existing docs allow.
- **No LIST on a hot path.** The registry must probe by digest/tag, not enumerate. Tag listing is the
  one paged LIST and it is a user action, not a push path (`docs/features/14_packages.md` §5).
- **Round-trip budgets (law 6).** A `docker push` of a small image must fit the same discipline as a
  git push: parallel blob PUTs, one manifest CAS, no read-after-write verification on the happy path.
- **Tasks, not spinners (law 7).** WP5 and registry GC are task kinds with progress and SSE attach.
- **Seams only (law 8).** The registry attaches as a `RouteProvider` (`internal/server/bind_api.go`)
  plus a feature package; it must not import upward into `internal/server`.
- **Docs move with code (law 12).** Each WP updates its feature doc and appends decisions there.

## 6. Acceptance criteria

Common to every WP: `make fmt vet test cover test-web` green; `-race` on touched packages; rendered
Chromium proof at desktop and ~390 px for UI changes; the feature's API covered by httptest and its
user flow by e2e where a real client exists.

Per WP:

- **WP1** — the OCI conformance subset in `14_packages.md` §8 passes (push/pull by tag and digest,
  auth allow/deny per role, delete + GC); `docker push`/`pull`/`manifest inspect` work against a live
  instance; blob PUTs are parallel and the push adds no sequential read; the Packages page lists tags
  with pull commands; feature toggle off hides the page and 404s the routes.
- **WP2.1** — a cross-owner fork PR can be opened entirely from the UI.
- **WP2.2** — labels/milestone/assignees editable on a PR from the UI and the SDK; list filters use them.
- **WP2.3** — the three common protection rules are editable without hand-editing JSON, and each is
  enforced by a test (the policy effect already is).
- **WP3.1** — `git archive` output for a tag and a commit matches `git archive` run locally
  (byte-identical for tar with fixed metadata; documented flag differences for zip).
- **WP3.2** — counts increment on download and survive a restart (bucket, not memory).
- **WP3.3** — a draft with a new tag creates the tag at publish and never before.
- **WP4** — the tags page and deletion work with the policy check; deletion of a tag backing a release
  is refused with a clear message.
- **WP5** — fast-forward sync is idempotent and single-flighted; divergence is reported without
  rewriting history; SSRF rules match the mirror path.
- **WP6** — assignee editing and the org webhook tab pass the same tests as their API twins.

## 7. Risks

1. **Registry scope creep.** "Packages" could mean five ecosystems. v1 is OCI only (Q1); every other
   ecosystem is a separate plan.
2. **Registry GC correctness.** Blobs are shared by manifests; deleting a tag must not delete a blob a
   second manifest references. GC runs as a sweep (no refcount LIST on delete) — specify and test it.
3. **GCS decision drift.** Config, README, setup UI and the store doc must agree (WP7) or the next
   operator hits a boot error.
4. **The editability rule expands scope.** "Everything is editable in the UI" can surface settings with
   no form today (WP9); each one costs an endpoint + a page section. The sweep bounds the list before
   the rc milestone, and anything it uncovers that is genuinely rare lands as a named follow-up rather
   than silently shipping as JSON-only.
5. **Doc truth.** The README checklist had drifted from the code. WP8 makes it the single status source
   and the plan links every claim to evidence.

## 8. Resolved scope decisions (2026-11-20)

| # | Question | Decision | Effect on this plan |
|---|---|---|---|
| 1 | Package ecosystems | **OCI/container only** | WP1 as written; npm/Maven/NuGet/RubyGems stay out |
| 2 | Release archives + download counts | **In v1** | WP3.1 and WP3.2 are required, not optional |
| 3 | Branch-protection UI | **In v1 — everything is editable in the UI** | WP2.3 required; new WP9 sweep enforces the rule |
| 4 | GCS | **Remove it** | WP7 becomes a removal, not an implementation |
| 5 | Fork sync / code search | **Sync fork in v1; search stays deferred (P9)** | WP5 required; search unchanged |
| 6 | Registry auth | **Reuse existing token/OIDC, repo-role scopes** | WP1 as designed; no second credential type |

Consequences worth stating plainly: v1 grows by WP2.3 (protection UI), WP9 (editability sweep) and the
release-archive work, and shrinks by the GCS backend. The README's "protection rules are backlog" line
is superseded by decision 3 and is updated when WP2.3 lands.
