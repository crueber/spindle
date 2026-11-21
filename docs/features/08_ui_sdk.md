# 08 — UI & SDK: the collaboration surfaces (issues, PRs, review, checks, releases, notifications)

> Depends on 01–07 for object shapes and endpoint semantics; **normative for the page layer
> (`internal/webui/**`) and `web/sdk/src/**`**.

> **As-built (2026-11-20, D-WEB-14/D-WEB-15):** the browser surfaces are **server-rendered Go
> pages** (`internal/webui` `html/template`, vendored htmx, vanilla-ES islands), not a client SPA.
> The route table's pages are templates under `internal/webui/templates/pages/`, shared blocks are
> `internal/webui/templates/partials/`, and behavior is the islands under `web/src/islands/`. Client
> mechanisms named in the contract text below (cache keys, signals, lazy `import()`, optimistic
> updates) describe the removed D-WEB-6 implementation; the *behavior* contract is unchanged and is
> enforced server-side. The SDK is per `docs/go/12_web_ui.md` §1.
> This is the UNIFIED frontend plan: route table, shared surfaces, SDK submodules,
> SSE wiring, permissions-driven gating. Feature docs sketch their pages; THIS doc owns the routes, the
> shared-surface inventory, and the wire-facing SDK surface. Where a feature doc and this doc disagree on an
> endpoint path, the feature doc owns the wire and this table is corrected in the same change.

Everything reuses the frozen patterns of `docs/go/12_web_ui.md` — server-rendered pages (§2), htmx for
partial updates (§2.6), islands (§2.7), Go markdown/diff/graph/highlight renderers (§2.8), URL-state
instead of client memory (§2.9) — and the wire conventions of `docs/go/07_api.md` §2: plain-text errors
shown verbatim, arrays `[]` never null, RFC 3339, SSE envelope (§6), SWR/ETag vs no-store cache classes
(§4 — plus the mutable-collab `private, no-cache` class for version-keyed collab GETs, issue #280). No
runtime npm dependencies (D-WEB-15; the SDK stays dependency-free). No TypeScript.

> **Historical adaptation (2026-09-04, D-WEB-6; superseded 2026-11-19/20 by D-WEB-14/D-WEB-15):** an
> earlier revision of the paragraph above described the vanilla-ESM SPA; D-WEB-6 replaced it with the
> SolidJS SPA (`solid-js` + `@solidjs/router` runtime; `vite` + `vite-plugin-solid` +
> `@tailwindcss/vite` dev-only). That implementation and its module names are historical; the
> contract below is realized in the server-rendered page layer.

## 1. Route table additions (server-rendered)

Routes are additions to the 12_web_ui.md §2.3 inventory; every route is answered by the page provider
(`pageOr404` through the `/*` dispatch) with its own template. Repo tabs extend to: **Code, Issues,
Pulls, Releases, Commits, WAL, Settings**.

| Route | Template (`internal/webui/templates/pages/`) | Gating (P6 role) | Notes |
|---|---|---|---|
| `/:owner` | `public/owner.html` | read | Renders an **org profile** when `owner` is an org: repos, members, description; else the user page |
| `/:owner/:repo/issues` | `collab/issues.html` | read | Filterable list (state/assignee/labels/milestone, #415), index-backed, "more" by `after` cursor |
| `/:owner/:repo/issues/new` | `forms/issue_new.html` | authenticated | Title + markdown composer |
| `/:owner/:repo/issues/:num` | `collab/issue.html` | read | Timeline + sidebar (labels, assignees, milestone, state) |
| `/:owner/:repo/labels` | `forms/labels.html` | triage+ | Label CRUD |
| `/:owner/:repo/milestones` | `forms/milestones.html` | triage+ | Milestone CRUD + progress |
| `/:owner/:repo/pulls` | `collab/pulls.html` | read | List with open/closed/merged tabs, same card shape as issues (`kind` distinguishes) |
| `/:owner/:repo/pulls/new` | `forms/pull_new.html` | write+ | Head/base pickers via the ref list |
| `/:owner/:repo/pull/:num` | `collab/pull.html` | read | Conversation + merge box (per 03; singular `pull` is 03's spelling) |
| `/:owner/:repo/pull/:num/commits` | `collab/pull_commits.html` | read | PR commit list |
| `/:owner/:repo/pull/:num/files` | `collab/pull_files.html` | read | Go diff renderer; line-thread anchoring lives on the main PR page |
| `/:owner/:repo/checks` | `collab/checks.html` | read | Paged index of checked shas (state pills, filter, live rows via `check` frames) |
| `/:owner/:repo/checks/:sha` | `collab/check_detail.html` | read | Statuses for one commit; linked from commit detail, the check pill, and the merge box |
| `/:owner/:repo/releases` | `collab/releases.html` | read | Per 07: list, latest |
| `/:owner/:repo/releases/{tag}` | `collab/release.html` | read | Detail: rendered markdown body, assets table |
| `/:owner/:repo/releases/new` | `forms/release_new.html` | maintain+ | Tag picker (ref list), "create a new tag on publish" (name + target commit, WP3.3), autodraft fill, asset upload with client-side `crypto.subtle` sha256 |
| `/:owner/:repo/tags` | `repo/tags.html` | read | Tag, target commit, created date, author, backing release; per-row archive links and a delete action (write; hidden for release-backed tags) |
| `/:owner/:repo/archive/{ref}.{tar.gz,zip}` | — (streamed) | read | Source archive from `git archive`, tree-id ETag; linked from the Code toolbar and the release page |
| `/:owner/:repo/sync-fork` (POST) | — (303) | write | Starts the WP5 sync-fork task; the header shows **Sync fork** on a fork for writers |
| `/notifications` | `public/notifications.html` | authenticated | Full tray page |
| `/:owner/:repo/settings/{tab}` | `settings/repo.html` + `partials/settings_repo.html` | varies | Sub-tabs: `access`, `collaborators`, `webhooks`, `ci-tokens` (admin); `labels`, `milestones` (triage+); existing scheduled/policy/config tabs unchanged; the Danger Zone section (red accents, both themes) renders below the tab content on every tab — first entry Delete Repository through the typed confirm, server is the admin gate, success redirects to `/` |
| `/:owner/settings/{tab}` | `settings/org.html` + `partials/settings_org.html` | org owner/admin | Org sub-tabs: profile, members, teams, invites, webhooks |
| `/:owner/teams/:slug` | `forms/team.html` | read | Team page: members, team repos (per 01) |

Global chrome additions: the notifications link/tray page, star/watch toggles in
the repo header (07; POST forms, the 303 back re-renders), "New issue"/"New pull request" buttons in repo tabs
(server-shaped per §5).

## 2. Shared surfaces (`internal/webui/templates/partials/`, `pages/`, islands)

`Component` below is the CONTRACT name from the plan; the shipped realization is a Go template
partial/page plus, where behavior needs it, an island. Client-mechanism words in the contracts
(`useData` cache key, signals, `innerHTML`, optimistic state) describe the removed D-WEB-6
implementation — the behavioral contract is what matters and it is enforced server-side.
Realization map: `ThreadTimeline` → `partials/collab_timeline.html`; `CommentComposer` →
`partials/collab_composer.html` (+ the `autogrow`/`submitkeys` islands); `LabelPicker`/`AssigneePicker`
→ `partials/collab_common.html` + `partials/collab_threads.html`; `DiffPage` →
`partials/repo_diff.html` + `partials/collab_diff.html` (Go `diff.go`); `MergeBox` →
`partials/collab_threads.html` + `pages/collab/pull.html`; `CheckPill` →
`partials/repo_check_pill.html`; `ReviewSummaryBar`/`ReviewModal`/`ReviewerPicker` →
`pages/collab/pull.html`; `NotificationTray` → `pages/public/notifications.html`;
`DangerConfirm` → `partials/settings_repo.html`/`settings_org.html` + `web/src/islands/danger.js`.

| Component | Purpose | Contract |
|---|---|---|
| `ThreadTimeline` | Renders a P3 event log (issue thread, PR conversation, review thread) oldest → newest (chronological, issue #225 — the handler supplies the stable by-seq order). Comment kinds (opened/commented) render as divider-separated entries — author/date header, rendered markdown body, reaction rows — with NO per-comment boxes; every other kind renders as a single-line centered muted system row ("{actor} {text}") | Input: header + seq window of events (`{after_seq, n}`); compensating events render as normal rows (never rewrite history); comment bodies through the Go markdown renderer (GFM + `#N`/`PRN` ref autolinks + `@mention` profile autolinks per 06 §7, escaped); `aria-live="polite"` region so appended rows announce; rows carry `event-{seq}` DOM ids |
| `CommentComposer` | New-comment editor | markdown preview through the Go renderer; **mentions autocomplete**: `@` opens a popup fed by the repo collaborators ∪ org members (endpoint per 01); submits via the feature's comment endpoint → `{event_seq}`; optional close controls share one right-aligned action row — the issue thread uses them for Close/Reopen + Comment-and-Close, the PR page omits them. On an open issue the close controls are SPLIT buttons (issue #311): primary segment closes immediately as completed (one click, `CLOSE_COMPLETED` sent explicitly), ▾ segment opens an upward menu with the not-planned alternate only; the reason is a form field (`onClose(reason)` / `onCommentAndClose(body, reason)` become the submitted value); any outside click dismisses the menu (the `menus.js`/`data-popover` contract); reopen stays a plain button |
| `LabelPicker` | Apply/remove labels on an issue/PR | Source: `labels:{o}/{r}` cache; each toggle = one PATCH (one event per 02); triage+ only |
| `AssigneePicker` | Same, for assignees | Source: `assignables:{o}/{r}`; triage+ only |
| `DiffPage` | Renders a PR/commit diff | The server's typed diff renderer (`internal/webui/diff.go`, 12 §2.8) over the `text/plain` unified patch; sign-first rows, one gutter column (unified only), file anchors by `stats[].path`; **line-thread anchoring**: anchor key `(path, side: old\|new, line)`; per-line comment forms create threads via the review surface; threads whose anchor no longer exists on the current head's diff render collapsed with an "outdated" flag |
| `MergeBox` | PR merge control | State machine below; renders the required-checks gate (05: `protect.require_checks`, evaluated by 03's `merge` task) and review-requirement state (04); the disabled button's tooltip lists missing/failing contexts |
| `CheckPill` | Aggregate check state for a sha | Input: `GET …/api/checks/{sha}` combined state (worst-of: error > failure > pending > success, with `total_counts`); pill tooltip lists contexts; links to `/:o/:r/checks/:sha` |
| `ReviewSummaryBar` | Review rollup on the PR page (approvals / request-changes / states) | Source: `reviews:{o}/{r}:{num}`; updates on `review` SSE frames (04) |
| `ReviewModal` | "Finish review" — comment / approve / request-changes + pending line comments collected on the diff | Submits `reviews.create(num, {event, body?, commit_sha, threads?})`; dismiss button (maintain+) |
| `ReviewerPicker` | Request reviewers | Fed by `reviews.suggest(num, q?)` (access.json roles + commit authors) |
| `NotificationTray` | Notifications page + header entry | Source: per-user stream; the unread count is rendered by the handler and refreshed on frames; "mark read"/"mark all read" are form posts |
| `DangerConfirm` | Typed-confirm row for every Danger Zone entry (`partials/settings_repo.html`, `settings_org.html`; arming island `web/src/islands/danger.js`) | Input + confirm button; the button arms on the exact `owner/name` match (case-sensitive; the island may trim to arm, the SERVER requires the untrimmed exact value); a submit guard prevents double-post; failures render a plain-text `.err-line`, never a client tray |

**MergeBox state machine** (per 03/04/05): `draft → ready → blocked{checks, reviews, conflicts} → mergeable
→ merging(task) → merged | failed`. Transitions recompute on: header fetch, `check` SSE frame, `review` SSE
frame, task packets. The merge button enables only in `mergeable` AND role ≥ maintain. Merge runs the
`pull-merge` task kind (P7): `POST …/pulls/{num}/merge` attaches to the task SSE stream; progress pills
render from `progress` packets, terminal `result`/`error` per the 07 §6 envelope. An "Update branch"
action (write+) runs the `pull-update-branch` task the same way.

### Concurrency — MergeBox task attach
Hazard: double-clicking merge starts two `merge` tasks on the same PR; unmount mid-merge leaks the attach
reader. Avoidance: the `(repo, kind)` single-flight of the task table (13_concurrency.md §3) is the
authoritative dedup — the server joins a running merge; the page disables the button while
`state === "merging"`. Attach is the server-rendered merge-task fragment: while running it carries
`hx-get="…/merge/task" hx-trigger="every 2s"` (`partials/collab_threads.html`) and the swap stops once
the state is terminal; alternatively the page renders a `data-sse` element and the island dispatches the
task frames. Terminal packet flips the state machine exactly once.

## 3. SDK additions (`web/sdk/src/`)

New submodules, same style as `repo.js` (plain ES modules, `ReposError`, `_call` wrapper, trailing
`{signal, onProgress, headers}` on every method; per 12 §1.6 each method derives its own `AbortController`
and closes its reader in `finally`). `index.js` re-exports the new groups; the esbuild bundle
(`make web` → `web/dist/repos.js`) is UNAFFECTED mechanically — new modules join the bundle, no new
devDependency, no config change. JSDoc `@typedef`s for each response shape land in `types.js`.

Client wiring: `client.repo(o/r)` gains `.issues`, `.pulls`, `.reviews`, `.checks`, `.releases`, `.social`,
`.access`, `.collaborators`; `client` gains `.notifications`, `.orgs`, `.invitations`, `.forks`, `.starred`.

**Seam registration (14 §14.3):** every endpoint below is registered by its feature package's
`api.RouteProvider` on BOTH lanes (`api.Lanes`; `internal/issues`, `internal/pulls`, `internal/reviews`,
`internal/checks`, `internal/releases`, `internal/notifications`, `internal/identity`). The submodule in
this section is the client mirror of exactly one provider; auth levels shown per method resolve per P6.

### 3.1 `issues.js` → `repo.issues` (02)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, labels, assignee, milestone, since, after, n})` | `GET …/issues` | `{issues: [], more}` |
| `create({title, body, labels?, assignees?})` | `POST …/issues` | `201 {num}` |
| `get(num)` | `GET …/issues/{num}` | thread header (P3) |
| `events(num, {after_seq, n})` | `GET …/issues/{num}/events?after_seq=&n=` | `{events: [], more}` seq window |
| `patch(num, fields)` | `PATCH …/issues/{num}` | one event per field group |
| `comment(num, body)` | `POST …/issues/{num}/comments` | `201 {event_seq}` |
| `react(num, {target_event_seq, content})` | `POST …/issues/{num}/reactions` | `201` (shipped as `reactions.add`; dup → 200 `{summary}`) |
| `unreact(num, {target_event_seq, content})` | `DELETE …/issues/{num}/reactions/{target_event_seq}/{content}` | self-removal (read; shipped as `reactions.remove(num, seq, content)`) |
| `labels.list()/create({name, color, description})/update(name, fields)/delete(name)` | `GET/POST/PATCH/DELETE …/labels[/{name}]` | label CRUD (triage+; names immutable; no single-get endpoint — 08's `get/put` spellings corrected 2026-09-04) |
| `milestones.list(query?)/get(id)/create(...)/update(id, fields)/delete(id)` | `GET/POST/PATCH/DELETE …/milestones[/{id}]` | milestone CRUD (triage+; 08's `patch` spelling corrected 2026-09-04) |

### 3.2 `pulls.js` → `repo.pulls` (03)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, base, head, sort, after, n})` | `GET …/pulls` | `{pulls: [], more}` |
| `open({title, body, head, base, draft?})` | `POST …/pulls` | `201 {num}` (write) |
| `get(num)` | `GET …/pulls/{num}` | header + `{mergeable, merge_state}` (mutable-collab: `private, no-cache` + folded ETag, issue #280) |
| `update(num, fields)` | `PUT …/pulls/{num}` | title/body/state (close/reopen; write) |
| `diff(num)` | `GET …/pulls/{num}/diff` | `text/plain` unified patch base…head (DiffPage parser input; ref-dependent SWR, no ETag — the shipped shape) |
| `commits(num)` | `GET …/pulls/{num}/commits` | `{commits: [Commit]}` |
| `merge(num, {strategy, commit_title?, commit_message?, delete_head?})` | `POST …/pulls/{num}/merge` | maintain; `202 {task}` + attach via `mergeTask(num)` poll (the task record carries progress + terminal outcome; shipped shape — 08's `merge_method/onEvent` spellings corrected 2026-09-04) |
| `mergeTask(num)` | `GET …/pulls/{num}/merge/task` | merge-task attach poll (progress + terminal outcome) |
| `updateBranch(num, {expected_head_sha?})` | `POST …/pulls/{num}/update-branch` | write; task `pull-update-branch` (shipped shape — 08's `onEvent` spelling corrected 2026-09-04) |
| `deleteHead(num)` | `DELETE …/pulls/{num}/head` | maintain |

`forks.js` → `repo.forks.create(opts, callOpts)` → `POST /api/v1/repos/{owner}/{repo}/forks`
(write + create rights on target) → `202` + TaskRecord, task kind `pull-fork` (top-level route, same
`pulls` RouteProvider per 03; shipped shape — 08's `(owner, repo, onEvent)` spelling corrected 2026-09-04).

### 3.3 `reviews.js` → `repo.reviews` (04)

| Method | Endpoint | Shape |
|---|---|---|
| `list(num)` | `GET …/pulls/{num}/reviews` | `{reviews: []}` |
| `submit(num, {state, body?, commit_sha, threads?})` | `POST …/pulls/{num}/reviews` | `201`; `state: APPROVE\|REQUEST_CHANGES\|COMMENT` (shipped as `submit` with `state`, not `create` with `event` — corrected 2026-09-04); `threads[]` = pending line-anchored comments |
| `get(num, seq)` | `GET …/pulls/{num}/reviews/{seq}` | one review |
| `dismiss(num, seq, {reason?})` | `POST …/pulls/{num}/reviews/{seq}/dismiss` | maintain+ |
| `requests.list(num)` / `requests.add(num, reviewers[])` / `requests.remove(num, reviewers[])` | `GET/POST/DELETE …/pulls/{num}/review-requests` | reviewer requests (write+) |
| `suggest(num, q?)` | `GET …/pulls/{num}/review-suggest?q=` | reviewer auto-suggest (access.json roles + commit authors) |
| `threads.list(num, {…})` | `GET …/pulls/{num}/threads` | list with anchors + resolution |
| `threads.create(num, {anchor, body})` | `POST …/pulls/{num}/threads` | create line thread from a diff hunk (shipped shape — 08's `threadCreate(num, {path, side, line, body})` corrected 2026-09-04) |
| `threads.get(num, tid)` | `GET …/pulls/{num}/threads/{tid}` | thread header (P3) |
| `threads.comment(num, tid, body)` | `POST …/pulls/{num}/threads/{tid}/comments` | `201` |
| `threads.resolve(num, tid)` / `threads.unresolve(num, tid)` | `POST …/pulls/{num}/threads/{tid}/resolve` \| `/unresolve` | resolve toggle |

### 3.4 `checks.js` → `repo.checks` (05)

| Method | Endpoint | Shape |
|---|---|---|
| `combined(sha)` | `GET …/checks/{sha}` | `{sha, state, total_counts, statuses: []}` (worst-of rollup; no-store — sha-addressed but mutable) |
| `statuses(sha)` | `GET …/checks/statuses/{sha}` | `{sha, statuses: []}` (shipped as a flat method, not a `statuses.get` group — corrected 2026-09-04) |
| `report(sha, {context, state, target_url?, description?, started_at?, completed_at?})` | `POST …/checks/statuses/{sha}` | CI surface: `checks:write` token or write role → `200` |
| `list({after, n})` | `GET …/checks?after=&n=` | `{checks: [{sha, state, contexts: []}], more}` (n 50/200) |
| `ciTokens.create({name, scopes})/list()/revoke(id)` | `POST/GET …/checks/tokens`, `DELETE …/checks/tokens/{id}` | CI tokens (admin; shipped as `ciTokens`, not `tokens` — corrected 2026-09-04) |

### 3.5 `notifications.js` → `client.notifications` (06)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, after, n})` | `GET /api/v1/notifications?state=&after=&n=` | `{notifications: [], more}` |
| `unreadCount()` | `GET /api/v1/notifications/unread_count` | `{count}` |
| `markRead(id)` / `markUnread(id)` | `POST /api/v1/notifications/{id}/read` \| `/unread` | → Notification |
| `markAllRead()` | `POST /api/v1/notifications/read_all` | `{updated}` |
| `stream(onFrame, opts)` | `GET /api/v1/notifications/stream` | per-user SSE (envelope §6); frames `event: notification` with the notification object; runs until cancelled — returns a cancel function |
| `watch.get(o, r)` / `watch.set(o, r, bool)` | `GET`/`PUT`/`DELETE …/api/watch` | `{watching, count?}` (read to toggle; 06 owns the verb — see Decisions) |

### 3.6 `orgs.js` → `client.orgs` + repo permission groups (01, 06)

| Method | Endpoint | Shape |
|---|---|---|
| `orgs.list()` / `orgs.create({name, ...})` | `GET/POST /api/v1/orgs` | sorted list; create (write auth) |
| `orgs.get(org)` / `orgs.put(...)` / `orgs.delete(org)` | `GET/PUT/DELETE /api/v1/orgs/{org}` | org profile; PUT/DELETE = org owner |
| `orgs.members.list(org)` / `orgs.members.get(org, principal)` / `put(org, principal, role)` / `delete(org, principal)` | `GET …/orgs/{org}/members[/{principal}]`, `PUT/DELETE …` | membership (PUT/DELETE = owner; shipped `put/delete` spellings, not `add/remove` — corrected 2026-09-04) |
| `orgs.teams.list/create(org)` / `get/put/delete(org, slug)` | `…/orgs/{org}/teams[/{slug}]` | teams (owner) |
| `orgs.teams.addMember/removeMember(org, slug, principal)` | `PUT/DELETE …/teams/{slug}/members/{principal}` | team membership (owner) |
| `orgs.invites.create(org, ...)` / `client.invites.mine()` / `accept(id)` / `cancel(id)` | `POST …/orgs/{org}/invitations`; `GET/POST/DELETE /api/v1/invitations[/{id}…]` | per 01 (owner invites; accept/cancel mine; shipped `cancel`, not `decline` — corrected 2026-09-04) |
| `repo.permissions()` | `GET …/api/permissions` | `{role}` per P6 (anonymous → `{role: null}` or `read` when `anonymous_read`) |
| `repo.access.get()` / `repo.access.put(bindings, version)` | `GET/PUT …/api/access` | role bindings; GET triage+, PUT admin (full-doc replace, CAS version, `409` on mismatch) |
| `repo.invites.list/create()/cancel(id)` | `POST/GET …/api/invitations`, `DELETE …/api/invitations/{id}` | repo invites (admin; shipped on `repo.invites`, not `repo.access.invitations` — corrected 2026-09-04) |
| `repo.collaborators.list()` | `GET …/api/collaborators` | effective bindings + resolution source |
| `repo.assignables()` | `GET …/api/assignables` | `[{principal, display}]` |
| `repo.webhooks.list/create(id?)/get/update/remove(id)/ping(id)/deliveries(id)/redeliver(id, seq)` | `GET/POST …/api/webhooks`, `GET/PATCH/DELETE …/webhooks/{id}`, `POST …/webhooks/{id}/ping`, `GET …/webhooks/{id}/deliveries`, `POST …/webhooks/{id}/deliveries/{seq}/redeliver` | per 06; the secret is never returned (`secret_set`); redeliver replays the retained body and answers 410 when it was not retained |
| `orgs.webhooks.list/create/get/update/remove/ping/deliveries/redeliver(org, id, seq)` | the `/api/v1/orgs/{org}/webhooks…` twins (both lanes) | per 06 §5.4; org-owner gate; same wire shapes as repo hooks |

### 3.7 `releases.js` + `social.js` (07)

`repo.releases`: `list({n, after})`, `latest({include_prereleases?})`, `get(tag)`, `put(tag, {body, name?,
  target?, …})` (a `target` is only valid while the tag does not exist — use with `draft:true`),
draft?, prerelease?}, ifMatch?)`, `remove(tag)`, `uploadAsset(tag, name, bytes, {sha256})`,
`deleteAsset(tag, name)`, `autodraft({tag, since})`, `releaseAssetUrl(tag, name)`. `repo.social`: `get()` → `{stars, watchers, forks,
viewer:{starred, watching}}`, `repo.star.set()/remove()` (watch lives in `repo.watch`, §3.5 — Decisions).
`client.social.myStarred({n, after})`, `client.social.userStarred(principal)` for the top-level starred lists
(shipped spellings — 08's `delete/client.starred/userStarred/star/unstar` corrected 2026-09-04). Asset bytes
download via the static `/{o}/{r}/releases/{tag}/assets/{name}` URL (`browser_download_url`); each asset
carries `downloads` (WP3.2). `repo.tagsApi` also exposes `list()` (`GET …/tags`) and `remove(name)`
(`DELETE …/refs/tags/{name}`; 409 when the tag backs a release or moved concurrently).

### 3.8 `collab.js` → `repo.collab` (08 §4, new in this change)

| Method | Endpoint | Shape |
|---|---|---|
| `stream(onFrame, {signal?})` | `GET …/api/collab/stream` | SSE, read-gated; `onFrame({kind, num?, seq?, sha?, tag?, actor?, at, …})`; the promise settles on stream end (clean EOF rejects so the page reconnects); abort via `signal` → 499, reader closed in `finally` |

Unlike `notifications.stream` (which resolves with a cancel function), `collab.stream` awaits the
stream lifetime so the page's `data-sse` reader reconnects mid-stream drops with backoff — drive it
through the SSE island, not directly.

### Concurrency — SDK additions
Same hazard set as 12 §1.6: leaked readers, unbounded parallel streams, popup-auth races. Avoidance is
inherited, not reimplemented: every new method goes through `core.js`'s `_call` (one derived
`AbortController` per call, reader closed in `finally`); `notifications.stream` and any task-attach method
return a cancellation function in addition to honoring `signal`; at most one popup-auth promise per client.
No new shared mutable state is introduced by the submodules — each is stateless over the client.

## 4. SSE wiring

One **repo collaboration stream** carries all live collaboration events for a repo; one **per-user stream**
carries notifications. Both use the 07 §6 envelope (`: walgit` opener, `notice`/`progress`/`task`/`result`/
`error` semantics, 10 s keepalives, no-store). Feature mutating handlers publish frames synchronously after
their CAS commits (P8); the streams are the transport only — **the timeline and lists remain the source of
truth**; frames invalidate cache keys, they do not carry full state.

- `GET /{o}/{r}/api/collab/stream` — repo-scoped, read auth (anonymous when `anonymous_read`). Frame
  `event: <kind>` with data `{num?, seq?, kind, sha?, tag?, actor, at}`; kinds: `issue`, `issue_event`,
  `pull`, `review`, `thread`, `check`, `release`, `access`. Defined here because it is cross-feature; each
  feature doc names the frames its handlers emit (02: `issue` `{num, card}` on header mutations and
  `issue_event` `{num, event}` on appended events; 03: `pull` with
  `{action: opened|closed|reopened|merged|head_force_pushed, num, title, state, author, base_ref, head_ref,
  head_sha}`; 04: `review` on posted/dismissed with rollup state, `thread` on comments/resolution with
  `thread_id`; 05: `check` with `{sha, context, state, combined_state, updated_at}`; 07: `release` with
  `{tag, action}`; 01: `access` with `{updated_at}` when access.json CAS commits).
- `GET /api/v1/notifications/stream` — per 06 (authenticated self-only, browser lane credentials); frames
  `event: notification` whose data is the notification object itself; runs until the client cancels.

| Page (template) | Streams | Frame handling |
|---|---|---|
| `collab/issue.html`, `collab/pull.html` | repo stream (filtered by `num`) + per-user stream | matching `issue`/`issue_event`/`pull`/`review`/`thread`/`check` frames invalidate the page; the page re-reads through the handler (htmx swap or navigation). Timeline rows are keyed by `(num, seq)`, so a late frame from an aborted stream can never duplicate a row |
| `collab/issues.html`, `collab/pulls.html` | repo stream | invalidate the list view on any `issue`/`pull` frame |
| `collab/checks.html`, commit detail, check pill | repo stream | `check` frames invalidate the checks view |
| `collab/releases.html` | repo stream | `release` frames invalidate the list |
| merge box (`partials/collab_threads.html`) | merge task attach | `progress`/`result`/`error` packets drive the fragment (htmx `every 2s` while running) |
| notifications page (`public/notifications.html`) | per-user stream | frames invalidate the tray; the unread count re-reads |
| ref/branch pickers | `repo.refStream` (existing §1.5 dialect) | unchanged |

**Contract status (2026-11-20):** the transport is the shipped `web/src/islands/sse.js` reader
(`data-sse` + `data-sse-url`/`data-sse-events`, coalesced 250 ms invalidation, 1 s→30 s backoff,
`ui:sse-<event>` dispatch that htmx consumes). The repo-scoped collab stream wiring lands per surface;
a page that does not subscribe renders the same content without live updates (never a broken control).

**Invalidation storm control:** a burst of frames (CI posting 30 check runs) MUST coalesce — the island
coalesces frames into one trailing invalidation per stream element (250 ms), and the page's refresh is a
single server render (the handler reads current state; there is no client promise cache to join). A
sustained frame rate therefore decays to one refetch per coalescing window, never one per frame.
Mutation responses always re-read (#41); timeline appends carry `num`+`seq` and never refetch the
whole window.

**Abort patterns:** one live reader per `data-sse` element (`WeakMap`-owned, cancelled on
`cleanup()`); a page that re-renders a subtree re-initializes idempotently (`data-island-ready`), so an
htmx swap never stacks a second stream. Errors set `data-sse-state="error"` and reconnect with capped
exponential backoff (1 s → 30 s, reset on open) — the page never throws on stream errors. Fetch-based
readers only (never `EventSource` — 12 §2.6: it cannot POST, set headers, read JSON payloads, or
distinguish a 401).

### Concurrency — SSE lifecycle
Hazard: leaked readers and stacked streams (one per navigation or keystroke) and out-of-order frames
overwriting fresh state — the §7.1/§7.3 class of incident in client form. Avoidance (13 §5 ownership rules
transposed): the caller (page) owns the signal, the SDK owns and closes the reader; one live stream per
component; rows/threads are keyed (`(num, seq)`, cache key) so a late frame from an aborted stream can never
overwrite fresh data; buffers are bounded (the frame set is a keyed map, never an unbounded queue). Reconnect
backoff prevents a hot-reload loop from stampeding the server.

## 5. Permissions-driven UI

The server is authoritative (P6); page gating is the server's own (the page handler applies
`s.gated`/repo visibility/role floors, 12 §2.4) and every affordance it renders is already
authorized. A denied write re-renders the page with the honest status (401/403/404) or an inline
`.err-line`; there is no client tray. The handler resolves the role from the same access read the API
uses (`repo.permissions()` / the identity gate), then shapes the page.

| Role | UI enables |
|---|---|
| anonymous (`null`) | read-only pages when `anonymous_read`; composer and all pickers hidden |
| `read` | view everything; comment on issues/PRs; open new issues; star/watch |
| `triage` | + label/assignee/milestone pickers, close/reopen others' issues, resolve threads, labels & milestones settings tabs |
| `write` | + new PRs, submit reviews, request reviewers, push |
| `maintain` | + merge (merge box button), releases create/edit/delete, protected-ref operations |
| `admin` | + settings access/collaborators/webhooks tabs, access.json editor, repo delete |

**Hide vs disable:** hide entirely when the role is absent (anonymous, or below `read`); render disabled
with a `title` tooltip when the role is present but object state forbids (locked conversation, draft PR,
missing required checks). The handler decides; templates render only what they are given — no template-side
role guessing.

## 6. Client cache keys — historical (`web/src/lib/data.js`, deleted with D-WEB-15)

These keys described the SPA's in-memory promise cache; they no longer exist. Kept as the record of
which endpoints shared data and how much staleness each tolerated. Server-rendered pages re-read the
same endpoints per request (page responses are `no-cache` + ETag, and the remaining JSON lanes keep
their 07 §4 cache classes).

| Key | TTL | Endpoint |
|---|---|---|
| `repo:{o}/{r}` | 5 s | `GET …/api` (existing) |
| `perms:{o}/{r}` | 30 s | `GET …/api/permissions` |
| `issues:{o}/{r}:{filter}` / `pulls:{o}/{r}:{filter}` | 5 s | list endpoints |
| `issue:{o}/{r}:{num}` / `pull:{o}/{r}:{num}` | 5 s | headers |
| `events:{o}/{r}:{num}:{after_seq}:{n}` | ∞ | event windows are immutable (P3: events never rewrite) |
| `labels:{o}/{r}` / `milestones:{o}/{r}` | 30 s | label/milestone lists |
| `diff:{o}/{r}:{num}:{headSha}` / `prcommits:{o}/{r}:{num}:{headSha}` | ∞ | sha-addressed content |
| `reviews:{o}/{r}:{num}` / `threads:{o}/{r}:{num}` | 5 s | review surfaces |
| `checks:{o}/{r}:{sha}` / `checkindex:{o}/{r}:{after}` | 5 s | CI updates results (05 GETs are no-store but mutable) |
| `releases:{o}/{r}` / `release:{o}/{r}:{tag}` | 30 s / 60 s | 07 |
| `social:{o}/{r}` | 30 s | 07 |
| `assignables:{o}/{r}` | 300 s | 01 |
| `notifications:me` | 5 s | 06 |
| `owners` | 5 s | `GET /api/v1/owners` (core, 07 §8) — the `/` page owner list |
| `repos:{owner}` | 5 s | `GET /api/v1/owners/{owner}/repos` (core, 07 §8) — per-owner sections on `/`, shared with the `/:owner` page |

The LRU cap (400) and tray behavior were the SPA's and are historical (12 §2.4's cache section
applies to the SDK/JSON lane, not to pages).

## 7. i18n and accessibility

**i18n: NONE — English-only v1.** All strings are inline literals in page/component modules; no extraction
keys, no locale files, no translation layer. A future i18n layer is additive (string table swap) and does
not need scaffolding now.

**a11y baseline (no new dependencies):** semantic HTML first (`nav`/`main`/`article` per timeline event,
`form` for composers, `button`/`a` distinctions preserved); popovers/menus follow the `menus.js` contract
(focus moves into the panel, `Esc` closes and restores focus to the opener, arrow/Home/End walk, Tab out
dismisses); native `<details>` pickers stay keyboard-native; live regions (`aria-live="polite"`) for
SSE-updated regions; diff line anchors are real DOM ids so deep links work with keyboard navigation; every
action reachable by keyboard; visible focus styles in `web/src/app.css`. This is the floor, not the
ceiling — no ARIA beyond what semantics require.

## Decisions

> **Reading note (D-WEB-14/D-WEB-15, 2026-11-20):** entries that cite `web/src/**`, `.jsx` files,
> `lib/*` helpers or `solid-js` are records of the removed SolidJS SPA and are historical. The
> contract they decided is realized by the server-rendered page layer
> (`internal/webui/*.go` + `internal/webui/templates/**`) and the SDK, unless the entry says otherwise.

- **SolidJS idiom (2026-09-04, D-WEB-6 adaptation — SUPERSEDED 2026-11-19/20 by D-WEB-14/D-WEB-15; historical).** 08's text describes a vanilla-ESM
  SPA (hand-rolled reactive core, hand-rolled router, `useData`/`mountStream` in `lib/`); the repo ships
  the SolidJS SPA per the D-WEB-6 amendment. This change implements 08's CONTRACT in the waves A–C
  SolidJS idiom: routes in `web/src/index.jsx` (`@solidjs/router`), state via Solid signals/stores,
  `useData` on Solid primitives, `mountStream` + new `mountStreamRetry` in `lib/sse.js`, shared
  components in `web/src/components/` (ThreadTimeline, CommentComposer, MergeBox, NotificationTray,
  perms/collab helpers), pure dependency-free helpers in `web/src/lib/collab.js` (TTL table,
  frame→key map, backoff). Runtime npm budget stays `solid-js` + `@solidjs/router`; the SDK stays
  dependency-free; no TypeScript. `index.js` does NOT re-export submodules — the attach pattern
  (`client.repo(o/r)` gains `.issues/.pulls/...`, `client` gains `.notifications/.orgs/...`) IS the
  re-export surface, established by waves A–C and kept.
- **08's §3 tables corrected toward the shipped wire (2026-09-04, same change).** Where 08 named an
  SDK method the waves did not ship (`react`, `labels.get/put`, `milestones.patch`, `reviews.create`,
  `threadCreate`, `statuses.get`, `tokens.*`, `members.add/remove`, `invitations.decline`,
  `access.invitations`, `releases.delete`, `client.starred`, `star/unstar`, `merge_method`, fork
  `(owner, repo, onEvent)`), the feature doc owns the wire and the shipped SDK/pages are the
  implementation of record — 08's tables now name the shipped shapes (each correction marked
  inline). `pull-merge` attach is the `mergeTask` poll (the task record carries progress + terminal
  outcome), not a generic SSE envelope — MergeBox polls it with a double-submit guard and
  exactly-once terminal flip.
- **Collab-stream client awaits the lifetime (2026-09-04).** Unlike `notifications.stream` (resolves
  with a cancel fn), `repo.collab.stream` awaits stream end and rejects clean EOFs so
  `mountStreamRetry` reconnects mid-stream drops with backoff; cancel via the caller signal.
- **This doc owns routes/components/SDK surface; feature docs own wire semantics** — one place to build the UI from; conflicts resolve toward the feature doc in the same change.
- **One repo collaboration stream (`/{o}/{r}/api/collab/stream`) instead of per-feature streams** — one connection per page, one parser, kinds namespaced; frames invalidate caches rather than carry state (P8 keeps handlers synchronous; the timeline is the backfill truth).
- **PR diff served as `text/plain` unified patch** — the 12 §2.8 parser is already the contract; no JSON diff shape to invent.
- **`repo.permissions()` endpoint for client gating** — P6 resolution is server-side; the UI reads one resolved role instead of re-implementing resolution order.
- **Cache-frames-not-patch for lists; append-only for timelines** — lists stay simple TTL caches; timelines use `(num, seq)` dedup so SSE and pagination compose.
- **Watch is a subscription verb owned by 06 (`client.watch.{get,set}`), not 07's `repo.social`** — both docs named `PUT/DELETE …/api/watch`; reconciled to 06 (subscriptions/notifications domain). `repo.social.get()` still returns `viewer.watching` for the header toggle's state; star stays in `repo.social`.
- **Hide for absent roles, disable for forbidden states** — anonymous users see no chrome they cannot use; authenticated users see what exists and learn why it is off.
- **Issue close actions live under the composer (2026-09-04, issue #33; hint dropped per #51).** The sidebar STATE card
  keeps a read-only state badge only — the "close / reopen from the comment box" hint was removed as
  hand-holding prose (#51); Close/Reopen and
  Comment-and-Close render in the composer's right-aligned action row (Close/Reopen always when the
  composer shows, Comment-and-Close only while open). Gating is unchanged: the controls show exactly
  when the composer shows (any resolved role) and the server stays authoritative (state PATCH is
  author-or-triage, 403s surface in the error tray) — `GET …/api/permissions` returns `{role}` with
  no viewer identity, so the client cannot detect authorship; no new endpoint for that. Comment-and-Close
  is two sequential calls (comment POST when the body is non-empty, then state PATCH) — there is no
  atomic comment+close endpoint, so a comment-posted/close-failed split keeps the text and the plain
  Close button finishes the job.
- **Honest close + system-row timeline (2026-09-05, issue #109).** Comments render as divider-separated
  entries (no per-comment boxes; the composer stays a distinct card); every non-comment event kind
  renders as one centered muted system line ("{actor} {fragment}", fragment from `lib/issue-events.js`,
  unit-tested in `web/test/unit/issue-events.test.js`). The API defaults an omitted `state_reason` to
  `completed`, so the UI never sends one: on an open issue both close controls are SPLIT buttons
  (issue #311 — primary segment closes immediately as `completed` in one click, ▾ menu offers the
  "… as not planned" alternate only; `closePatch` throws instead of building a reason-less body),
  and the closed header reads "Closed as …" from the recorded
  `state_reason` ("Closed" alone when none is recorded). Applies to the shared ThreadTimeline, so PR
  conversations get the same treatment.
- **Chronological ThreadTimeline (2026-09-09, issue #225).** The component renders oldest → newest
  (stable by-seq sort inside, over the newest-first wire order — the wire is unchanged, 02 §7
  Decisions). Both surfaces inherit it with no per-page code: the issue thread passes its
  newest-first assembly (view page + older windows), the PR conversation passes its events as-is.
  "Older events" sits atop the issue thread (older windows prepend above); SSE refetches append
  below (newest lands last). Scroll policy, documented in the component header: never autoscroll
  (a reading user is never yanked), prepends pin the viewport (`anchorScrollTop` in
  `web/src/lib/thread-order.js`, headless-tested in `web/test/unit/thread-order.test.js`).
- **English-only v1, no i18n scaffolding** — additive later, zero cost now.
- **Releases/stars SDK per 07's submodule plan** (`releases.js`, `social.js`) — absorbed verbatim to avoid conflicting paths.
- **Settings Danger Zone with typed-confirm (2026-09-04, issue #39).** `DangerConfirm` is the single confirm pattern for all future entries; Delete Repository reuses the existing `repo.delete()` (`DELETE …/api` → 204) and navigates to `/` on success because the repo page is gone. No new endpoint, no new SDK method, no new deps.
- **Owners page shows per-owner repos + intro (2026-09-05, issue #117).** `/` renders the intro card
  then one section per owner (newest-first, capped at `MAX_OWNERS` 50) with that owner's repos
  (newest-first, capped at `MAX_REPOS_PER_OWNER` 10, "+N more →" folding to `/:owner`). Data comes
  from the existing core listing endpoints via the existing SDK (`owners.list()`, `owners.repos(o)`)
  under the `owners` / `repos:{owner}` §6 keys — no new endpoint, no new SDK method, no new deps.
  Ordering key is the reverse of server order (the listing path exposes no creation timestamps; see the
  `newestFirst` JSDoc in `web/src/lib/owners.js`). Pure cap/order helpers live in
  `web/src/lib/owners.js` with headless cover in `web/test/unit/owners.test.js` (logic/DOM
  separation per 12 §5); the page contract itself lives in 12 §2.3.1.
- **Calm owners page + star counts (2026-09-05, issue #137).** Owner sections flatten to a
  `divide-y` divider stack (intro card kept; caps, overflow links, newest-first order from #117
  unchanged) and every repo row on `/` and `/:owner` shows `(N ⭐)` via `<StarCount>` on the
  shared `social:{o}/{r}` 30 s §6 key — single-flighted, non-blocking (muted `(…)` placeholder),
  worst case bounded by the #117 caps. No new endpoint, no new SDK method, no new deps.
- **Split-button close controls (2026-09-10, issue #311).** The #109 chooser menus (open the menu,
   then pick a reason — no one-click path) become GitHub-pattern split buttons: primary segment
   closes immediately as `completed` (the API default made explicit via `CLOSE_COMPLETED`; the menu
   therefore offers the not-planned alternate ONLY — the button IS the completed path), ▾ segment
   toggles the upward menu. Same for Comment-and-Close (primary posts the body, empty body just
   closes, then closes as completed). Both segments + menu share one root: a document click outside
   it dismisses (RefPicker/ReactionMenu pattern, removed in `onCleanup`), clicks on ▾ itself just
   toggle (inside the boundary, never close-then-reopen), Escape refocuses the toggle, Tab-out
   dismisses. All three controls in each split menu (primary, ▾, item) disable from the
   composer's `getBusy()` guard (single-flight, no double-close), as the chooser menus did.
   No backend change — `state_reason` vocabulary unchanged. Headless cover in
   `web/test/unit/split-close.test.js` (source-text state-machine pins, same convention as
   `clone-outside-close.test.js`); no new deps.
- **Cross-page cache invalidation for thread mutations (2026-09-11, issue #318).** Milestone
  reassignment showed stale membership in milestone-filtered lists until reload: the promise
  cache is global across Solid-router navigations, but invalidation rode the per-page SSE
  subscription — the issue page's `accept` (num-match) filter gated the shared
  `issues:{full}:*` invalidation too, and the milestones page subscribed to no `issue`
  frames at all. Three changes, no new endpoint, no polling, no new deps: (1) the issue
  page reconciles every thread mutation at the mutation site via
  `invalidateIssueLists(full)` (`data.js`: the `issues:{full}:` prefix — Issues.jsx query
  windows AND `issues:{full}:milestone:{id}` entries — plus `milestones:{full}` counts;
  uncached keys are silent no-ops); (2) the milestones page subscribes to `issue` frames
  so its inline lists + counts self-heal while mounted (remote actors included); (3) the
  `issue` frame→key map gains `milestones:{full}` (reassignment/close move counts without
  touching any milestone object) and the issue page drops its `accept` filter — per-key
  invalidation is already self-scoping (`issue:{full}:{num}` names the frame's own
  thread), so the filter only ever suppressed shared-prefix invalidation. The pull pages
  keep their num filters (same latent pattern — follow-up, not this fix). Headless cover
  in `web/test/unit/issue-invalidation.test.js` (frame→key set, prefix coverage of both
  stale surfaces, mutation-site refetch scoping) alongside the updated `collab-lib.test.js`
  frame-table pin.
- **Open-count badges next to the Issues/Pulls tabs (2026-09-11, issue #319).** The tab bar
  badges the open numerators from the shell's shared summary entry (`open_issues`/`open_pulls`
  ride `GET …/api` — 07 §9.1, zero new requests), rendered only when > 0 via the pure
  `tabBadge(summary, tabId)` helper (`lib/tabs.js`, headless-tested with the matcher). Badge
  styling reuses the notification-tray count-pill language (`.tab-badge` in `ui.css`). Live
  updates, three changes, no new endpoint, no polling, no new deps: (1) the `issue`/`pull`
  frame→key maps gain `repo:{full}` (invalidate-at-minimum — any mounted stream reconciles
  the badge); (2) the mutation site reconciles too (`invalidateIssueLists` gains the summary
  entry for issues, `Pull.jsx` reload gains it for PRs — the #318 pattern: the mutation's own
  frame may arrive at a page that already unmounted); (3) the server ETag covers the counts
  (`~c<index-version>` suffix, 07 §9.1 — a close/reopen with no ref move still busts SWR).
  Headless cover in the extended `repo-tabs.test.js` (badge numerators, hide-at-0) and the
  updated `collab-lib.test.js` / `issue-invalidation.test.js` frame-table pins.
- **TTL-aware SSE invalidation (Forgejo #396).** The per-tick coalescer could not bound live
  traffic — frames in separate tasks each flushed a full TTL-bypassing refetch round per cached
  key (~5 GET/sec bursts from one tab). The `scheduleInvalidate` flush (SSE path only) now
  dedupes prefix expansion per flush and skips already-fresh entries per the §6 TTL table, so
  sustained frame rates decay to TTL cadence while mutation `invalidate()` stays eager (#41).
  Immutable `events:` windows never SSE-refetch (timelines append frames directly). Headless
  cover: `web/test/unit/fetch-rate-guard.test.js` (burst budgets, post-TTL liveness,
  mutation-eager pin).

- **Rendered mention profile links (Forgejo #440, 2026-09-13).** The ThreadTimeline
  renderer's "comment bodies" contract now names the full markdown pipeline (ref + mention
  autolinks, 06 §7): `@username`/`@email` → `/{principal}`, `@org/team` unlinked, dead links
  acceptable (decision (a)). Renderer-only, every rendered-body consumer inherits it; no new deps.
  *(Implementation move, D-WEB-14/D-WEB-15: the pipeline is `internal/webui/markdown.go`; the
  SPA's `renderBody`/render-md path is historical.)*

## Explicitly out of scope

- Code search UI, Discussions, Projects/boards, Actions/CI running (P9) — no pages, no routes, no SDK groups.
- i18n, themes beyond the existing CSS, dark-mode variants.
- Mobile/responsive redesign beyond the existing Tailwind baseline; no additional CSS framework.
- Real-time collaborative editing (presence, live cursors) — SSE carries state changes, not keystrokes.
- Offline mode / service workers; every page needs a live server (the JSON API and the SDK likewise).
- Push notification channels (web push, email digests) — 06's per-user stream and webhooks are the only fan-out surfaces in v1.

- **Page-layer parity for repo invitations, avatar upload, and starred repositories (WP9 G9/G10/G17,
  2026-11-21).** No SDK or wire change: `repo.invites`, `users.avatar.upload`, and `client.starred` had
  the API methods; the rendered Go pages now expose the same operations — Settings → Access renders the
  invite form and the pending list with its accept link, `/{owner}?edit=1` carries the avatar file input
  (same allowlist as `users.avatar.upload`), and `/{user}/starred` lists the starred repos the viewer may
  read. Both surfaces call the SDK's underlying verbs through service method values, so the client and
  the page cannot drift.
