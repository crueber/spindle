# V1 audit — 2026-11-21

> Five independent audits of the v1 implementation, each required to produce rendered screenshot evidence.
> Verdicts are per area; a FAIL here has a named next step and is sent back as a dev pass. Reports live
> under `/tmp/opencode/audit-*.md` during the run; this file is the durable record.

## Summary

Round 1 (2026-11-21, pre-fix) found the gaps; every FAIL went back as a dev pass. Round 2 re-runs the same
audits against the merged fixes — **a verdict flips only on fresh rendered evidence**, never on a claim.

| Area | Round 1 | Round 2 (re-audit) | Evidence |
|---|---|---|---|
| CLI vs `gh` | PASS WITH FAILS (5) | **PASS** — all five fixed, independently reproduced | `/tmp/opencode/aud2-cli/` (7) |
| Threads vs GitHub Actions | FAIL — T1 only, no runner | **PARTIAL (close)** — real CI, real gating; pass 1 (`fix/threads-triggers`) closed PR-head delivery/ordering, `needs:`, dispatch inputs + UI and cancelled/`in_progress` checks; pass 2 (`fix/threads-runtime`) closed schedule, retention/GC, `permissions`/job token, the approval gate and the UI leftovers; round 3 (`fix/threads-retention-filters`) closed multi-repo retention and the filter layout and fixed three smaller gaps (job-token push loop, `skipped` state, first cron tick); only artifacts/caches (declared not-v1) remain | `/tmp/opencode/aud2-threads/`, `/tmp/opencode/aud3-threads.md` |
| Packages vs GHCR/OCI | protocol PASS / product FAIL (5) | **PASS** — protocol + product (one new page panic found and fixed) | `/tmp/opencode/aud2-packages/` |
| Web parity | FAIL — 5 breakages | **PASS on the five; 1 new gate hole** (authenticated non-admin) | `/tmp/opencode/aud2-web/` |
| Foundations / security | PASS with 5 defects | **PASS** — F1–F5 closed, no regressions | `/tmp/opencode/aud2-foundations/` (73) |
| UI editability (WP9) | not in round 1 | 18 gaps → **all closed** (G13 via threads pass 1, G18 via setup UX) | `/tmp/opencode/aud2-editability/` (13) |
| Settings/identity editors | not in round 1 | **6 defects found → all fixed → 9/9 verified in real Chromium** | `/tmp/opencode/aud4-closing/` |

Dev passes that closed round 1 (all merged): `fix/cli-audit` (#656), `feat/threads-a` (#657),
`fix/web-audit` (#658), `fix/foundations-audit` (#660), `fix/packages-parity` (#661), and
`fix/web-smoke-test` (#659, stale fetch smoke rewritten against the server-rendered contract).

The WP9 sweep (`aud2-editability.md`) is the one new round-2 area: the inventory and its 18 gaps live
in `docs/PLAN-V1.md` WP9, since that work package owns the editability contract. Its two P0s — every
org-settings write answering 503 because `SettingsDeps.OrgWrite` was never composed, and `/setup`
being unable to save (21 keys unrepresentable, a forced save writing `[""]` into untouched lists) —
are the most severe defects any audit has found, because both surfaces *render* as if they work.

---

## CLI (`audit-cli.md`)

**Verdict: pass with fails.** All 12 command families were exercised against a live auth-none server
and a token-auth server; tokens never printed, the state file is 0600, the full 0/1/2/3/4 exit matrix
was confirmed, `--json` is the endpoint body and completion is generated from the dispatch tables.
Screenshots: `help.png`, `json.png`, `json2.png`, `matrix.png`, `completion.png`.

### FAILs

1. **`threads run` / `threads cancel` call endpoints that do not exist** —
   `POST …/threads/{workflow}/runs` and `…/{run}/cancel`; the served and documented dispatch is
   `POST …/api/threads` with `{ref, workflow}`. The blanket 404→“feature not enabled” shim also
   misreports a genuine unknown run (`threads show … 999`) as exit 1 instead of exit 3.
2. **Per-command help is unreachable** — `peelGlobals` consumes `-h/--help` anywhere, so
   `spindle issue list --help` prints the global table and per-command flags are undiscoverable.
3. **`release create` cannot default its title and cannot target a new tag** — an empty name is sent
   without `--name` (400), and there is no `--target`, so the draft-with-new-tag flow is unreachable.
4. **`packages delete --tag T` still demands a DIGEST** — the positional is checked before `--tag`, so
   the documented tag form always exits 2.
5. **`issue create --label/--assignee` is non-atomic** — the issue is created, then metadata is
   PATCHed; a bad label exits 1 leaving the issue behind (reproduced: issue #3), so retries duplicate.

### Secondary

- No `pr diff`/`checkout`/`close`, no `-R/--repo`/`SPINDLE_REPO`, no `--jq`/`--template`/pagination.
- `checks status` rejects `refs/heads/<branch>`.
- `pr list --state all` returns 400.
- `docs/go/11_config_cli.md` §6.6 still says Threads endpoints are unserved (doc drift).

### Dev pass (2026-11-21, `fix/cli-audit`)

All five FAILs fixed with tests: `threads run` dispatches `POST …/api/threads {ref, workflow}` and
`show` maps 404→3 (cancel documents the T2 gap instead of calling a dead route); every subcommand
answers `-h` with its own usage/flags (table-walking test); `release create` defaults the title to the
tag and gains `--target` for draft-with-new-tag; `packages delete` accepts `--tag` or a digest;
`issue create` sends metadata in one request validated before numbering (no orphan on a bad label).
Secondary: `pr diff`, `pr close`, `-R`/`SPINDLE_REPO`, `checks status refs/heads/<branch>`,
`pr list --state all`. Deferred: `--jq/--template`, `--web`, `--watch`, `--paginate`, `pr checkout`,
`issue edit/delete`, `release download/edit/delete-asset`.

---

## Threads vs GitHub Actions (`audit-threads.md`)

**Verdict: FAIL — not at parity, and it cannot gate a merge green today.** What shipped is the T1
milestone: bucket-native run/job records plus a checks mirror. There is no runner, no executor, no
logs, no artifacts, no secrets, no cancel/rerun, no cron, and `pull_request` never triggers. Runs stay
`queued` forever and mirrored checks stay `pending` forever, so a `threads/*` required check
permanently blocks merges (`t-pr-mergebox.png`).

**Demonstrated working:** push-triggered run creation with branch filters; job records with
`threads/<workflow>/<job>` check contexts; statuses on `/checks`; `require_checks` refusing with the
right contexts; Threads list and run pages at 1440 and 390 with no page-level pan.

### FAILs

1. **No runner protocol or executor (T2/T3 absent).** `/api/v1/threads/runners/*` and
   `/jobs/{id}/attempt/1/start` 404; no `queue/` or `log/` objects; no runner binary. Cannot pass
   until §3's endpoints, the CAS claim and per-job container execution exist.
2. **`pull_request` is dead code and a merge fires no push run.** *(fixed in pass A)* `CreateRun(EventPullReq)` is never
   called; merging a PR produced zero runs because server-side publishes bypass `onPush`.
3. **No logs, cancel, rerun, artifacts or secrets.** All 404; no secret store, masking or job token.
4. **A narrow strict workflow subset, with all-or-nothing failure.** *(fixed in pass A: breadth + per-file isolation, error in UI/API)* Unsupported keys (dispatch inputs,
   `on: [push]`, `concurrency`, `strategy`, `permissions`, list `runs-on`, mapping `container`,
   job-level `uses`) make *every* workflow in the repo silently produce zero runs; the only signal is a
   server-log WARN.
5. **The job record discards what a runner needs** *(fixed in pass A: versioned `spec`)* — services, job `env`/`if`/`timeout-minutes`/
   `continue-on-error`, and step `with`/`env`/`if`/`shell`/`working-directory` are parsed then dropped,
   contradicting “a runner must not need the workflow file”.

### Next steps (dev passes)

- **Pass A — LANDED 2026-11-21** (`docs/features/15_threads.md` §2.1/§8): PR triggers
  (opened/synchronize/reopened, never close) via `pulls.PullEvents`; the merge task's base-ref
  publish reported through `pulls.BranchPublished` into the same `CreateRun` path as a client push;
  a versioned `Job.Spec` (v1) carrying services/env/if/timeout/continue-on-error/permissions/
  matrix/concurrency/job-`uses` and every step field; parser breadth for `on` string-or-list,
  dispatch inputs, concurrency, matrix+fail-fast, permissions, list `runs-on`, mapping `container`,
  job-level `uses`; per-file parse isolation with the error shown on the Threads page and
  `GET …/api/threads/workflows`. Still outstanding (unchanged by pass A): see FAILs 1 and 3.
- **Pass B (runner, after A):** runner protocol (register/declare/fetch/start/log/complete/cancel),
  attempt tokens, CAS queue claims, log chunks + SSE attach, cancel/rerun, secrets store/masking,
  read-only fork token, job-container executor (steps via `docker exec`, checkout inside, services,
  capacity 1/shared), matrix expansion + concurrency-group enforcement (the spec now carries both),
  then the 100-job scale test.

**Pass A landed** (`3e03616`): PR opened/synchronize/reopened triggers, a post-merge-publish hook, a
versioned `Job.Spec` (services/env/if/timeout/continue-on-error/permissions/strategy/concurrency/
steps), parser breadth (string/list `on`, dispatch inputs, concurrency, matrix, permissions, list
`runs-on`, mapping `container`, job `uses`), per-file failure isolation with the error surfaced on the
Threads page, and `GET …/api/threads/workflows`. Matrix expansion, concurrency enforcement and
permissions enforcement stay in pass B with the runner.

---

## Web parity (`audit-web.md`)

**Verdict: FAIL.** The collaboration wave largely works — fork PR picker, PR metadata + filter,
branch-protection editor, issue assignees, tags page + delete rules, draft-with-new-tag, download
counts, inert HTML assets, chrome on all eight tabs, import progress, signed-out gating, markdown
sanitizer, setup secret masking and `next` sanitization are all screenshot-backed — but five
user-visible breakages and one gate inconsistency block sign-off.

### FAILs

1. **`.tar.gz` archives are plain tar.** `/archive/<ref>.tar.gz` runs `git archive --format=tar`
   (no gzip stage) yet serves `application/gzip`, so `tar xzf` fails. Zip is correct. Root:
   `internal/archive/archive.go` `ArchiveArgv` + the serve label.
2. **SVG images never render.** `rawContentType` has no `.svg` case → `text/plain` + `nosniff`, so
   README `<img src="…logo.svg?raw=1">` shows broken. Fix must serve `image/svg+xml` **with**
   `Content-Security-Policy: default-src 'none'; sandbox` + nosniff (an `<img>` is inert; direct
   navigation must not execute script).
3. **Fork divergence is a dead end.** The page only says “Sync fork started”; the result (diverged,
   upstream message, PR link) lives only in the API, and the generated `pr_url`
   (`…/pulls/new?base=main&head=bob%2Fwidgets%3Amain`) lands on a form that does not split
   `owner/repo:branch` and ignores `from_repo`.
4. **390 px page pan on Tags and Release detail** (`scrollWidth` 579 and 569 vs 390). The
   `overflow-x-auto` wrapper sits in a grid item with automatic min-width, so it never constrains;
   every other audited page is exactly 390/390.
5. **Guests get live Settings forms.** Under token auth + anonymous read, `/settings/general`,
   `/settings/scheduled`, `/settings/mirror` render 200 with editable forms (Save, create mirror, push
   mirror) while POST is 403 and the other seven tabs are 403 — the page gate does not mirror the API.

### Honorable mentions

- External `<video>` is sanitizer-allowed but CSP-blocked (no `media-src`), so it never plays.
- Static-token git over HTTP fails: the 401 advertises only `Bearer`, never `Basic`.
- The effective-policy table clips its Effect cell at 1440.
- `/setup` denial renders HTTP 200 (content is the restriction page); it should be 401/403 to match
  the JSON API (found while verifying the live instance).

### Dev pass (2026-11-21, `fix/web-audit`)

All five FAILs closed with screenshots (`/tmp/opencode/fix-web/`): the archive is genuinely gzipped
(`tar tzf` + magic, 304 preserved); `.svg` serves as `image/svg+xml` with a sandbox CSP (README image
renders, scripted SVG inert when navigated); fork divergence renders its outcome with a working PR
link (`head=owner/repo:branch` parsing); Tags and Release are 390/390 (the absolutely-positioned
`.sr-only` cell was widening the grid item — wrappers are `relative`+`min-w-0`); settings read tabs are
admin-only (403, no upstream URL for guests). Foundations F2/F3 are covered by the same tags and
settings fixes. Extras: `media-src`, `Basic` in the 401 challenge for static tokens, the policy Effect
cell fits, `/setup` denial is 401/403. Open: `web/test/unit/smoke.test.js` still probes a default
`:8080` server and fails without one (pre-existing test rot).

---

## Packages vs GHCR/OCI (`audit-packages.md`)

**Protocol: PASS.** An unmodified Docker 29 client works end to end: Basic login, `/v2/` ping,
single-chunk and chunked uploads (416 gap detection, abort), manifests by tag and digest, multi-arch
index round-trip (`buildx imagetools inspect`), tag paging with `Link`, Range/HEAD, digest mismatch
rejected and never stored, tag/manifest delete, feature-toggle 404, and the full Bearer challenge →
`/v2/token` → scoped-token flow (a pull token cannot push; anonymous is pull-only).

**Product: PASS (fixed 2026-11-21).** Every product FAIL below is closed; the original finding is
kept verbatim so the register stays honest. The protocol was not rewritten — the fixes are the gate
reuse, the sweep, the surfaces, and two delete-semantics corrections.

### FAILs — all fixed 2026-11-21

1. **The repository owner cannot push to their own repo** without an `access.json`: the registry mints
   a pull-only token (`token scope does not grant push`). `packages.CanPush` → `identity.Resolve`,
   whose `SynthesizeDefault` only synthesizes email-owner bindings; the git gate has an owner rule the
   registry lacks. The registry must mirror the git authorization rule exactly.
   **FIXED:** `packages.CanPush` now calls `identity.CheckPush` — the git receive-pack gate itself —
   and `/v2/token` derives `push` from it, so the registry has no second rule to drift. Live evidence:
   `alice/private` created with no `access.json` (`git push` works), then
   `docker push 127.0.0.1:18302/alice/private:v1` succeeds as `alice` and `bob` gets
   `token scope does not grant push`. Tests: `TestCanPushReusesGitOwnerRule`,
   `TestTokenEndpointGrantsOwnerPush` (real `identity.Service`; owner/no-binding vs read-binding vs
   write-binding vs admin vs anonymous vs host-write-only).
2. **Deletion never reclaims storage.** After deleting every tag and the manifest digest, the blob GET
   still answers 200; the documented `packages-gc` maintainer task does not exist.
   **FIXED:** `packages-gc` is a `maintain.Sweeper` on the maintain loop's task path (single-flight,
   progress packets, `POST …/api/ops/packages-gc` force, 15-minute per-repo due gate, 10-minute
   finalize grace). A digest delete now removes the whole version tree (index + direct platform
   manifests) so its blobs become unreferenced, and the sweep deletes them plus expired uploads and
   abort tombstones. Tests: `TestGCReclaimsDeletedImage`, `TestGCSharedBlobSurvives`,
   `TestGCUploadsAndTombstones`, `TestGCBlobGraceKeepsFreshBlobs`, `TestGCSweepDueAndErrors`,
   `TestDeleteIndexVersionOwnsItsChildren`, `internal/maintain/sweepers_test.go`. Live: after the
   grace window the manual op reports `deleted N unreferenced blob(s)` and the blob GET is 404.
3. **No Packages page/tab** — `/o/r/packages` is a 404; the only web surface is the Features checkbox.
   **FIXED:** `/{o}/{r}/packages` renders tags (tag, digest, size, platforms, updated), a copyable
   `docker pull` command, a per-version manifest table and typed-confirmation deletes; the
   **Packages** tab sits after Tags and hides when `features.packages=false` (the page then 404s).
   Rendered proof: 1440×900 and 390×844, `scrollWidth === clientWidth`, zero console errors
   (`/tmp/opencode/audit-packages/pkgfix-*.png`).
4. **No JSON API and no SDK, and the shipped CLI is dead** — `/{o}/{r}/api/packages` 404s, so
   `spindle packages list` says “feature not enabled”; `web/sdk/src/packages.js` is missing.
   **FIXED:** the four documented endpoints (both lanes, discovery templates) and
   `web/sdk/src/packages.js`; `spindle packages list|show|delete` work against a live server
   (`delete --tag` no longer needs a digest). Tests: `TestPackagesAPISurface`,
   `TestExposedCoversAPIRoutes`, `web/test/unit/sdk-packages.test.js`,
   `cmd/spindle/cli_commands_test.go::TestPackagesCommands`.
5. **Delete by digest leaves dangling tags** — `tags/list` still advertises tags whose manifest is gone
   (each 404s); GHCR drops the pointers with the version.
   **FIXED:** a digest delete drops every pointer aimed at the manifest or any child it removes;
   `TestDeleteByDigestDropsTags` / `TestDeleteIndexVersionOwnsItsChildren` assert an empty
   `tags/list` afterwards.

### Secondary

- `DELETE` of an unknown tag returns 202 instead of 404. **FIXED:** both the registry and the JSON API
  answer `404 MANIFEST_UNKNOWN` (`TestDeleteTagAndManifest`, `TestPackagesAPISurface`).
- No registry events/webhooks. **DEFERRED with a written decision** (docs/features/14 §10): the
  events bridge is WAL-driven and registry writes are bucket objects; a `registry_package` event
  would need a new sink purely for fan-out. Polling `GET …/api/packages` covers the need.
- 2 GiB blob cap vs GHCR's 10 GB layer (configurable, but the default is low). **FIXED:** the default
  is 10 GiB (`internal/config`, docs/go/11 §2).
- `docker manifest inspect` needs `--insecure` over plain HTTP (documented client behaviour). Still a
  client caveat, unchanged.
- `auth.mode = none` makes the registry anonymously writable (expected for that mode; document loudly).

### Dev pass (2026-11-21, `fix/packages-parity`)

**DOCUMENTED:** docs/features/14 §4/§9 and the setup page's `packages` card.

### Fix verification (live, 2026-11-21, Docker 29.7.2)

Instance: coordinator-configured `mode=token` on `:18302`, repo `alice/private` created with no
`access.json`.

```
$ docker login 127.0.0.1:18302 -u alice   → Login Succeeded
$ docker push 127.0.0.1:18302/alice/private:v1
v1: digest: sha256:bef6aa9f… size: 856          # owner, no access.json  (was: token scope does not grant push)
$ docker push … -u bob
error from registry: token scope does not grant push
$ curl -u bob "…/v2/token?scope=repository:alice/private:pull,push"
{"access":[{"actions":["pull"],…}]}             # read-only principal: no push scope
$ curl -s -u alice …/alice/private/api/packages
{"repository":"alice/private","packages":[{…,"pull":"docker pull 127.0.0.1:18302/alice/private:v1"}],…}
$ curl -o /dev/null -w '%{http_code}' -X DELETE -u alice …/v2/alice/private/manifests/demo   → 202
$ curl -o /dev/null -w '%{http_code}' -X DELETE -u alice …/v2/alice/private/manifests/<digest> → 202
$ curl -u alice …/v2/alice/private/tags/list
{"name":"alice/private","tags":[]}              # pointers dropped with the version
$ curl -o /dev/null -w '%{http_code}' -u alice …/v2/alice/private/blobs/<layer>
200                                             # still inside the 10-min finalize grace
# (after the grace window)
$ POST …/alice/private/api/ops/packages-gc
packages-gc alice/private: scanned 2 manifests, deleted 2 unreferenced blob(s) (29795274 bytes), …
$ curl -o /dev/null -w '%{http_code}' -u alice …/v2/alice/private/blobs/<layer>
404                                             # reclaimed
$ POST …/api/ops/packages-gc                     # second sweep
deleted 0 unreferenced blob(s)                  # idempotent
$ spindle packages list alice/private            # TAG/DIGEST/SIZE/PLATFORMS/UPDATED header only
$ spindle packages delete alice/private --tag absent
HTTP 404: manifest unknown (exit 3)             # a real not-found, not "feature not enabled"
$ [features] packages = false  (as host admin)
page 404 · /api/packages 404 · /v2/…/tags/list 404 · repo page has no Packages tab
```

Rendered: `pkgfix-desktop.png` (1440×900) and `pkgfix-mobile.png` (390×844) in
`/tmp/opencode/audit-packages/`; both `scrollWidth === clientWidth` (1440/1440, 390/390), Packages
is the current tab, the tag row shows digest/size/platforms/updated and the copyable
`docker pull`. The first mobile capture exposed a real defect (document scrollWidth 602 while the
table card scrolled in place); the card is now `relative overflow-x-auto`, which restored 390.

---

## Foundations / security (`audit-foundations.md`)

**Verdict: PASS.** Both foundations hold: Precision Dark is applied on every page found in a 60-page
crawl (dark + light, 1440 + 390; measured 34 px rows, mono identifiers, correct contrast), and the
**built image renders the full stylesheet** — 54,675 B / 759 rules with `max-w-6xl` + `.flex{`,
styled at both widths, so the live-site incident class is closed end to end. Setup access is
rule-bound (`SPINDLE_SETUP_TOKEN` has no effect; no/invalid config opens it; token/oidc require an
admin; OIDC `admin_groups`/`groups_claim` and `admin_emails` verified through a real browser login
against a stub IdP). Identity gating, import (43 % → 100 % → navigation, failure card), console
hygiene, CSP/XFO/Referrer-Policy/nosniff on pages/API/404s/assets, and inert HTML release assets all
pass.

### Defects

1. **`/setup` is a dead end in `token` auth mode** (medium). The page said to
   sign in, but browser login existed only for OIDC, so `/_auth/login` answered
   501 with copy claiming the OIDC config was incomplete. **FIXED
   (`fix/foundations-audit`):** `POST /_auth/token` validates an admin static
   token and mints the ordinary signed `walgit_session` (no new credential
   type); the token tree now accepts that cookie and re-derives rights from the
   live token list; `/setup` renders the paste form (or the exact
   file/API recovery when no `session_secret` exists) and every `/setup` gate is
   unchanged (open for no/invalid config or `auth.mode = "none"`; otherwise
   admin). Tests: `TestTokenSignInMintsAdminSession`,
   `…RejectsBadAndNonAdminTokens`, `…Gates`,
   `TestSettingsSetupTokenModeRecoveryCard`, rendered proof at 1440/390.
2. **390 px pan on the tags page** (medium) — `scrollWidth` 620 vs 390; the 704 px `data-table
   min-w-[44rem]` grows the document despite the scrollable wrapper. Routed to `fix/web-audit`.
3. **Repository settings renders 200 for a nonexistent repo** (medium-low) while the API is 404 — the
   page gate does not mirror the API. Routed to `fix/web-audit`.
4. **Import echoes the submitted token into the HTML** (medium-low) — a validation failure re-rendered
   `name="token" … value="SUPERSECRET…"`, contradicting “sent once, never stored or logged”.
   **FIXED:** the field has no `value=` and every failure/terminal render scrubs
   the token (`formsImportData.scrubSecrets`), keeping the other inputs;
   `TestFormsImportValidationFailureNeverEchoesToken` asserts a failed response
   contains no submitted token.
5. **An invalid config ignores `[server].listen`** (low-medium) — it bound
   `0.0.0.0:8080` and pre-filled defaults, so fixing config through the UI could
   silently move the port. The same branch also dropped the `SPINDLE__SERVER__
   LISTEN` env overlay. **FIXED:** setup-only boot uses
   `config.LoadSetupOnly` (defaults ⊕ every decodable file field, then the env
   overlay/PORT — the overlay wins over the file, as on a valid boot), the page
   pre-fills those values and marks the rows the validator named, and an
   unparseable `server.listen` is now a validation error that falls back to the
   compiled-in listener so `/setup` stays reachable. Tests:
   `TestLoadSetupOnly*`, `TestSetupOnlyBootEnvListenWinsOverFile`,
   `TestSetupOnlyBootPrefillsFileValues`.

### Nits

Two amber primaries on every repository page (the page action plus the chrome
Clone) — **FIXED:** Clone is primary only when the page has no body primary;
code/issues/pulls/settings each show exactly one (rendered proof 1440/390,
`TestOnePrimaryPerRepoPage`). Issue vs PR row anatomy mismatch — **FIXED:** both
rows show an always-present state chip, labels, author · age and comment count
(`TestListRowsShareAnatomy`; `PROut.comment_count` rides the existing index
read). `/setup` Chrome DOM warning from two submit buttons in one form — open
(cosmetic, unchanged). Stale ignored `web/dist/htmx-ext-sse.js` — **FIXED:**
`build:vendor` deletes it, `make web` + the Dockerfile assert its absence, and
`web/embed_test.go` walks the embedded tree.

### Open item

- **Rejected pack left the push client hanging** (incidental, not one of the
  six checks). Pushing a branch after a maintainer pack rewrite reported
  `pack rejected: fatal: pack has 1 unresolved delta` and then never terminated
  the response (client sat 3+ minutes, twice); it did not reproduce on a fresh
  repo. Next: replay the first-push → maintainer-rewrite → delta-push sequence
  with `GIT_TRACE_PACKET=1`, ensure receive-pack closes on rejection, and check
  `index-pack --fix-thin` delta-base resolution. Not chased in this pass.

  **FIXED (`fix/receive-pack-hang`, 2026-11-21).** Two defects: receive-pack
  ingested thin packs WITHOUT `--fix-thin` (the flag was keyed on a capability
  the client cannot negotiate), which is why the delta base was "unresolved";
  and every refusal gated the report-status trailer on `report-status` while
  git requests only `report-status-v2`, so the client waited forever for a
  report. `thin=true` is now unconditional (as §3.2/§7 already specified),
  `PushRequest.WantsReport` gates the trailer (with per-ref `ng` lines), and
  the HTTP push exchange carries a `server.request_timeout` deadline. Repro:
  `internal/e2e/pushreject_test.go` — pre-fix it reproduces the exact audit
  line and hangs; post-fix the same push is refused in ~30 ms with
  `! [remote rejected] main -> main (unpacker error)` and exit 1.

---

## Round 2 — re-audits

### CLI (`aud2-cli.md`, 2026-11-21)

**Verdict: PASS.** A fresh build (`spindle 5c33f19`) against two live servers reproduced every fix
rather than trusting the notes:

1. `threads run` sends exactly `POST /demo/hello/api/threads` with `{"ref":"main","workflow":"ci.yml"}`
   (captured on a request-logging stub, byte for byte; the live server logs 201); `threads show
   <unknown>` exits **3** with the server's own 404; `threads cancel` makes **zero** network calls.
2. Per-command help works across a 51-command sweep (47 exact, the rest are the placeholder lines
   below); `spindle -h` still prints the global table.
3. `release create` defaults the title to the tag server-side, `--target` + `--draft` creates at the
   target sha, `--target` without `--draft` is exit 2.
4. `packages delete --tag v1` works with one positional; the digest form works; both/neither exit 2
   and leave the tag intact.
5. `issue create` with a bad label is one 400 with **nothing created** and no counter gap.

No CLI regressions across 12 families, the 0/1/2/3/4 matrix, tokens never printed, state `0600`
in a `0700` dir, completion, and `go test ./cmd/spindle/...`.

**Non-blocking observations** (recorded, not v1 blockers): help prints to stderr; `api -h` /
`completion -h` / `version -h` use placeholder/ignored usage; `--limit` is advertised on non-list
commands; `user list` can show an owner that `user show` 404s (lazy identity records); list routes on
a nonexistent repo return empty tables with exit 0; `release list` hides drafts; `user keys remove`
wants `id`, not the `SHA256:` fingerprint.

**New defect found while seeding (outside the CLI scope):** a post-merge push failed with `pack
rejected: fatal: pack has 1 unresolved delta` and then **hung the client indefinitely**. This is the
second independent sighting (the foundations audit recorded the same hang as an unconfirmed nit).
Filed as a git-ingest defect: a rejected pack must terminate the receive-pack exchange with an error,
never leave the client waiting. **FIXED** in `fix/receive-pack-hang`: unconditional `--fix-thin`,
+the report trailer for `report-status-v2`, and a bounded push exchange (the client now gets
+`! [remote rejected] … (unpacker error)` and exits 1 in milliseconds).

### Web (`aud2-web.md`, round 2)

**The five round-1 findings are genuinely closed**, each re-verified from scratch: `.tar.gz` is real
gzip (magic, decompressed tar byte-identical to `git archive`, 304 intact); SVG renders
(`naturalWidth 120`) with `sandbox` CSP + nosniff and a scripted SVG's script blocked when navigated;
fork divergence shows the outcome chip, upstream message and a PR link that preselects
`bob/widgets` + `refs/heads/main`; Tags and Release detail are 390/390 and 1440-clean; anonymous
settings tabs are 403 with no forms or upstream URL, and an unknown repo is 404.

No regressions across the round-1 passing surface (fork picker, PR metadata/filter, protection
editor, issue assignees, tag delete rules, pending-tag draft, download counts, inert HTML, chrome,
import progress, signed-out nav, sanitizer), plus the extras: `/setup` 401/403 with the card, git 401
advertising `Basic` and `Bearer`, `media-src` for external video, the policy Effect cell at 1440, one
amber primary per page.

**One new FAIL (same finding, stricter principal):** an **authenticated non-admin** still gets all
ten settings tabs at 200 with live forms, while the POSTs and `/api/access` answer 403, and the
Mirror tab prints the configured upstream URL. Root cause: `settingsRoleRank("")` falls through to
rank 5 ("admin") when identity resolves an authenticated non-member to no role. Dispatched as
`fix/settings-role-gate` (fail closed: an empty/unknown role ranks below every floor, with a
principal × tab matrix test).

### Packages (`aud2-packages.md`, round 2)

**Verdict: PASS** on every round-1 finding, re-verified with a real Docker 29 client against a
build of current `main`:

- **Owner push** works with **no `access.json`** (owner `docker push` succeeds; the token endpoint
  grants `pull,push`), a read-only principal is refused (`token scope does not grant push`), a write
  binding pushes, a non-member is denied — and a live `read → write → none` membership flip moved
  `git push` and `docker push` **in lockstep**, confirming the registry delegates to the git gate.
- **GC**: inside the 10-minute grace a sweep deletes 0 and the blob serves 200; after it, the sweep
  deleted 7 blobs (18.9 MB) and the blob 404s; a shared blob survives deleting one of two manifests;
  a second sweep deletes 0 in both fixtures.
- **Page/tab/API/SDK/CLI**: page renders at 1440 and 390 (390/390), tab present, full tag table,
  copyable pull, typed-confirmation delete; `features.packages=false` 404s the page, the API and
  `/v2` and drops the tab; all four endpoints on both lanes with the same gates; SDK tests 3/3;
  `packages list|show|delete` live, `--tag` without a digest, 404 → exit 3.
- **Protocol** re-ran clean: ping, Basic login, chunked upload (416 gap, abort), tag/digest push and
  pull, multi-arch index (`buildx imagetools inspect`), paging with `Link`, Range/HEAD,
  digest-mismatch rejection, the full Bearer → `/v2/token` scoped flow, anonymous public pull and
  refused anonymous push.

**One new defect, found by this audit and fixed in `0b2c3f9`:** the Packages page answered **HTTP
500 after a successful delete** — the page seam speaks `error` while the service speaks
`*packages.Error`, so success arrived as a typed-nil that the handler read as failure and
`packageErrStatus` dereferenced. Fixed at both ends (explicit conversion in the composition,
`normalizeErr` in the page, nil-guarded mapper) with `TestPackagesDeleteTypedNilSucceeds`.

**Observations, not regressions:** a private repo whose `access.json` omits its owner lets that owner
push but not read (identical in git — an inherited identity-model gap vs GHCR); the manual GC op
requires the host `write` flag, so a repo owner gets 403.

### Foundations (`aud2-foundations.md`, round 2)

**Verdict: PASS, no remaining FAILs.** Each round-1 defect was re-verified against a fresh build with
the round-1 binary (`40682eb`) reproducing the "before" on separate ports:

- **F1 closed** — `GET /_auth/login` in token mode renders a paste form (no OIDC copy); `/setup` anon
  is 401 with that form; an admin token → 303 + signed session → `/setup` 200 with all 146 rows (driven
  in a browser); invalid token → 401 with **0** occurrences of the submitted value; non-admin → 403,
  no cookie; a host without `session_secret` states the file/API paths with no dead form; OIDC still
  runs its full provider flow.
- **F4 closed** — the import token appears **0** times in the response (1 on the old binary), the
  field is empty, and the other inputs and the error survive.
- **F5 closed** — an invalid file at `127.0.0.1:18303` binds 18303 with 8080 closed and pre-fills the
  file's values (FILE badges, validator-named OIDC rows marked); `SPINDLE__SERVER__LISTEN` wins over
  the file.
- **Nits closed** — exactly one amber primary on every repo page; issue and PR rows share one
  anatomy; a stale `web/dist/htmx-ext-sse.js` can no longer ship (`/_ui/htmx-ext-sse.js` 404, guard
  tests).
- **Re-checked clean:** Precision Dark dark+light at 1440/390 with **0 px pan anywhere**, identity
  gating, import progress → completion, console hygiene (no CSP violations, no page errors, no htmx
  SSE noise), the full CSP/XFO/Referrer-Policy/nosniff set on pages/API/404/assets, and an uploaded
  `.html` downloading rather than executing.

**Non-blocking observations:** the plain auth pages (`/_auth/login`, rejected token) are unstyled raw
HTML; `/setup` shows two primaries (header Login + body Save — outside the repo-page rule);
`/favicon.ico` is gated but never requested by the templates.

### Threads (`aud2-threads.md`, round 2)

**Verdict: PARTIAL — Threads runs real CI and gates merges, but it is not yet at GitHub Actions parity.** Verified live with a real `spindle runner serve`, real containers and a real browser:

**Passing:** push → two jobs execute, checks `pending` (with `started_at`) → `success`, SSE logs stream in the browser at 1440 and 390; a failing step → check `failure` → the merge is refused (`blocked on threads/ci/lint (failure)`), a fix commit → merge `ok`, and the **merge commit gets its own push run** (the round-1 hole); PR open/reopen fire one run each and close fires none; a fork PR runs the **base-tip** workflow with `trusted=false`, `secret is []`, and a push with its job token is rejected with no ref landing (a trusted run logs `secret is [***]`); per-file workflow isolation (8 shapes, matrix → 4 cells, list `runs-on`, mapping container, permissions, concurrency, job `uses`; one bad file errors alone); services sidecar aliasing; cancel/rerun to attempt 2; 4 runners × 8 jobs claimed exactly once (2/2/2/2, 9 s, zero leftovers).

**FAILs:**

1. **PR head-push waits for the events sweep (default 5 minutes)** because a push never wakes the bridge — and a late delivery **rewound the PR head** (`pulls-sink head force-pushed 0bacfe12 → 361812b9`), so a reopen built a stale commit. This is the blocker for PR CI.
2. **`needs:` is not gated** — `needs: [lint]` started in the same second and finished 12 s *before* its dependency.
3. **`schedule` never fires** although the parser accepts cron (no maintainer evaluator), and the design doc calls it v1.
4. **No retention/GC** of runs and logs; **`permissions` is carried but not enforced** (there is no job token).
5. **Dispatch is API-only** and rejects declared `inputs` with 400; the Threads page links a dead `/api#threads`.
6. **No first-time-contributor approval gate** (the doc claims one); cancelled runs read as `failure`; no distinct `in_progress` check state; no commit-detail checks panel, run duration/filter, or runner attribution.
7. **Artifacts and caches are absent** — declared out of scope for v1, recorded for completeness.

**Round-2 fixes, pass 1 (`fix/threads-triggers`, LANDED 2026-11-21)** — closes FAIL 1, FAIL 2, FAIL 5, and the
cancelled/`in_progress`/duration half of FAIL 6:

- **FAIL 1 — CLOSED.** A landed push now wakes the events bridge inline in the shared push pipeline
  (`internal/server/bind_ssh.go`), so a PR head push produces its `pull_request` run in **53 ms**
  measured end to end (`internal/e2e/threads_triggers_test.go`, server configured with a *one hour*
  sweep: the run can only come from the wake). Head reconciliation is **forward-only by the event's
  own ordering**: `pr.json` carries `head_seq` (the last applied WAL sequence), a delivery at or below
  it is ignored, and a non-fast-forward move applies only when the event's own pre-image is the head
  we hold (a genuine force-push). Out-of-order delivery (newer first, older replayed) leaves the head
  at the newer sha with no force-push stamp and one `synchronize` run (`TestPullEventsOutOfOrderHeadDelivery`).
- **FAIL 2 — CLOSED.** `needs:` is gated in the queue, not the runner: the entry carries its
  dependency ids, a claim refuses a job until every dependency is terminal success, and a
  failed/cancelled dependency terminalises its dependents as cancelled (transitively, in waves) —
  `internal/threads/needs.go`, matrix in `needs_test.go` (chain, diamond, failure, cancel, parallel,
  matrix fan-in, enqueue-order starvation).
- **FAIL 5 — CLOSED.** `POST …/api/threads` accepts and validates `inputs` against the declared
  schema (string/choice/boolean, required/default; 422 naming the offending input) and the values
  reach job specs as `${{ inputs.* }}` / `inputs.*` in `if`; the Threads page has a **Run workflow**
  control (workflow select of `workflow_dispatch` files only, ref field, typed inputs, inline
  errors) and the dead `/api#threads` link is gone.
- **FAIL 6 (partial) — CLOSED.** Cancelled runs fold to `cancelled` (not `failure`), the checks enum
  gained `in_progress` and `cancelled` (both non-green for `require_checks`), and runs/jobs expose a
  derived duration. First-time-contributor approval, the commit-detail checks panel, filters and
  runner attribution remain for pass 2.

**Round-2 fixes, pass 2 (`fix/threads-runtime`, LANDED 2026-11-21)** — closes FAIL 3, FAIL 4, the
rest of FAIL 6, and leaves only FAIL 7 (artifacts/caches, declared not-v1):

- **FAIL 3 — CLOSED.** `schedule:` cron is validated by the parser over a documented five-field UTC
  subset (`internal/threads/workflow/cron.go`; names/macros/modifiers are that file's parse error) and
  evaluated by the maintainer task `threads-schedule` against a persisted last-fired watermark
  (`repos/<o>/<r>/threads/schedule.json`). The **watermark CAS is the commit point**: two instances
  sweeping the same minute create exactly one run, a restart never re-fires, and an unreadable
  watermark is an error, not a fire. Tests: tick math (`*/N`, ranges, lists, Vixie dom/dow, UTC),
  restart, two-instance dedupe, disabled workflow, plus a live-server e2e (one run, no second in the
  same minute).
- **FAIL 4 — CLOSED.** `threads-gc` now applies `threads.retention` (default **30 days**): terminal
  runs past the window are deleted with their job records, log frames and queue entries; running and
  young runs are kept; dead runner records (revoked past the window, unused expired registrations) go
  too. Checks are independent (`checks/` is 05's, never deleted), so a live PR's required check still
  gates after its run ages out — pinned by `TestRetainKeepsChecksForLivePR`. `permissions:` now scopes
  a real job token: the attempt token gains `push` when `contents: write`, is enforced on the git
  upload/receive path through a repository-scoped principal grant (fork runs pinned to read), and is
  revoked at completion/expiry. Proven live: a `contents: write` container pushes a branch, a
  `contents: read` one is refused and lands no ref.
- **FAIL 6 — CLOSED (approval gate + the UI leftovers).** A fork PR from a non-maintainer with no
  merged PR in the repository is created in `pending_approval` with its jobs written but **not
  enqueued**; a maintainer approves (queues it) or denies (cancels it) from the API or the run page,
  maintainer-authored and already-merged-contributor runs skip the gate, and the evidence is pulls'
  one-GET `merged_authors.json` projection (self-healing, fail-closed when unreadable). Proven live
  with a runner polling throughout: the held run was never claimed. The run list gained workflow/state/
  actor filters, job records carry the claiming runner's name, and the commit detail page renders the
  checks panel (context/state/duration/link) from the same combined view as `/checks`.
- **FAIL 7 — still open, by declaration.** Artifacts and caches remain out of the v1 parity claim
  (`15_threads.md` §1 "Not v1"); nothing in this pass claims otherwise.

**Round-2 fixes, pass 2 (`fix/threads-runtime`, LANDED 2026-11-21)** — closes FAIL 3 and FAIL 4 and the rest
of FAIL 6:

- **`schedule:`** is evaluated by the maintainer task `threads-schedule` over a persisted watermark:
  the CAS tick write precedes the run creation, so two instances produce one run per minute, a restart
  never re-fires, and a crash loses a tick rather than duplicating one. The parser rejects unsupported
  cron into that file's per-file error; `workflow_dispatch` remains the manual path.
- **Retention** (`threads.retention`, default 720 h): `threads-gc` deletes terminal runs with their
  jobs, log frames and queue entries, plus revoked/unused-expired runner records — bounded and
  idempotent. Checks are a separate family and are never deleted, so a live PR's required check still
  gates after its run ages out.
- **`permissions:` is enforced.** The `wjt_` token gains `push` only when the merged
  `permissions.contents` resolves to write (default read), enforced in the git receive/upload path via
  a repository-scoped grant, revoked at completion; fork runs pin to read. Steps get
  `$SPINDLE_JOB_TOKEN` by name, never argv.
- **First-time-contributor approval**: a fork PR from an actor without a merged PR is created
  `pending_approval` with jobs written but not enqueued; maintainer+ approves (queues) or denies
  (cancels). Maintainer and already-merged-contributor runs skip the gate.
- **UI**: run-list filters (workflow/state/actor), runner attribution and permissions per job, and a
  checks panel on the commit detail page.

Evidence: one run across two instances sweeping the same minute (and none after a restart); a 12 s
approval hold under a live runner; a container push accepted with `contents: write` and refused with
`contents: read`; `docs/EVIDENCE.md` E19. Six real-container e2e tests; coverage 95.0–96.2.

### Threads round 3 (`aud3-threads.md`, re-audit 2026-11-21)

**Verdict on the two round-3 FAILs: CLOSED (code + tests; a fresh live re-run is the audit's next
step).** Round 3 re-verified the nine round-2 scenarios against a six-repository instance and found
seven clean; two FAILs and three smaller gaps were dispatched to
`fix/threads-retention-filters`:

- **FAIL — retention swept only the first repository each hour. CLOSED.** `retentionGate` was one
  field on the process's single `threads.Service`, so the first repository of each hour consumed the
  1 h slot and every other repository's terminal runs/logs lived forever (observed: a 3-hour-old run
  with `retention = 2h` survived six kill/restart cycles). The cadence now lives in the maintainer's
  sweeper layer (`cmd/spindle/threads_sweep.go`: two `threads-gc` sweepers, queue every 5 min and
  retention hourly), each with a gate **keyed by repository**; `Service.Retain` has no clock. A
  restart forgets the map and repeats a pass for every repo (safe direction, never a starved one),
  and two instances may run the same repo concurrently because every delete is exact-key and
  already-gone-tolerant. The acceptance test is
  `TestRetentionSweepsEveryRepository` (two expired repos + one young, one interval → both swept,
  young kept, second pass a no-op), with `TestRetentionRestartsDoNotStarveEitherRepository` (six
  restarts) and `TestRetentionTwoInstancesDoNotDoubleDelete` alongside. Per-pass bounds unchanged.
- **FAIL — run-list filter controls rendered 50/28/29 px. CLOSED.** A `min-w-0` wrapper around a
  `width:100%` `.input` in a flex row collapsed to min-content at both 1440 and 390. The row now uses
  the canonical filter-bar grid (`card … grid-cols-2 sm:grid-cols-4`, labelled fields; style
  guideline §6, `pages/collab/issues.html`), each control in a `span.block.min-w-0` so it keeps its
  intrinsic height. Measured rendered controls (headless Chromium over CDP): **265 / 264 / 265 px
  wide at 1440×900, 160 px each at 390×844, 30 px tall**, form 1120/358 px, and
  `scrollWidth === clientWidth` at both widths (no page-level pan). Filtering behaviour and URL state
  are unchanged; the `filter` button stays secondary (the page's one `.primary` is Run workflow).
- **Gap — a job-token push retriggered workflows. CLOSED.** A `contents: write` job's push landed and
  produced a second `push` run (the audit's rebuild loop). The push funnel now marks such a push
  `byJob` (`JobRepo`/`JobPush`): the push lands, Threads creates no run, the events-bridge wake is
  skipped, and the periodic sweep cannot resurrect a `synchronize` either (the WAL `ci:<runner>`
  pusher maps to `pulls.RefEvent.ByJob`, which refreshes the head but never triggers). GitHub's
  `GITHUB_TOKEN` rule. Tests: `TestJobTokenPushIsMarkedAndDoesNotWake`,
  `TestJobTokenPushCreatesNoRun` (a human push right after does create one),
  `TestJobTokenRefEventNeverSynchronizes`.
- **Gap — a blocked dependent read `cancelled`. CLOSED.** A dependent that never ran because `needs:`
  failed is now `skipped` (job state, checks state, run page) instead of `cancelled`; the cascade is
  still transitive, an operator cancel / fail-fast sibling / denied approval still reads `cancelled`,
  and skipped is non-green (`only success` satisfies a required check) yet neutral in the run fold.
  Tests: `needs_test.go`, `internal/checks/states_test.go`.
- **Gap — the first cron sweep could lose a tick. CLOSED.** The schedule sweeper's gate now follows
  the wall-clock minute instead of a full cadence measured from the previous pass (a pass at 08:37:57
  read "a hair early" at 08:38:57, so tick 08:38 was lost entirely). The tick itself is always
  `Cron.Prev(now)` gated by the persisted watermark; with no watermark the current boundary is
  eligible at once. Test: `TestScheduleSweeperFirstTickIsTheNextMinute` (08:37:57 fires 08:37; the
  next pass at 08:38:00 fires 08:38, not 08:39).

Still behind GitHub Actions after this pass, unchanged and declared: artifacts/caches (not v1), the
action marketplace / reusable workflows / OIDC / environments (out of scope), and two recorded
observations not fixed here — the run page shows *declared* permissions on a fork run that is pinned
to read (the effective scopes are in the fetch/start answer), and cancel latency remains one
heartbeat (~25 s). The `/api` reference still has no Threads CI section.

## Settings / identity editors (`aud3-settings.md`, round 3)

The editors the editability sweep added had never been independently exercised. Most of the surface
genuinely works end to end — scheduled round-trip with cell-marked validation, repo and org webhooks
including a signed ping and redeliver, invite → accept where the role really grants push, policy
save/dry-run/typed removal and emptied-editor removal, WAL ops with a terminal task and no auto-retry,
avatar 415/413/good/remove, starred ordering and private visibility, and the 403/404/one-primary/
overflow/console guards — but six defects reproduced with rendered evidence:

1. **`/setup` coercion errors are keyless**: an invalid duration yields a banner and an errors table
   with an **empty KEY column**, no row marking, and the Advanced group stays collapsed (a keyed
   `config.Validate` error does mark the row, so the gap is `setup.Coerce`/`ParseValues`).
2. **A zero-edit `/setup` save rewrites the file**: 28 hand-written lines become a 205-line canonical
   document, and a no-op save **deletes an operator comment** (values are semantically identical and
   the second save is byte-identical).
3. **Policy dry-run makes the page deny the stored document** — the removal card says "No policy
   document is stored" while the document is live, until reload.
4. **Org invitations are email-only** and unclaimable by an email-less principal (`type=email` form; a
   static-token principal gets `409 invitation no longer pending`), while a username-addressed invite
   accepts fine.
5. **"Regenerate avatar" always 404s in token mode** (`no verified email`) even though the control is
   offered.
6. **The non-admin Access page nests a duplicate "Visibility & role bindings" card** — an unclosed
   `<section data-access-readonly>`.

Also observed: `/setup` row editors render the *running* config, so a second save before restart can
discard a just-added token or bundle row; webhook event names are undiscoverable from the page (the
real vocabulary is `opened`/`member_added`/…, not `push`); a newly created hook immediately receives
the retained event backlog. One methodology note: the audit's first build shipped a stale gitignored
`web/dist/ui.js`, which deadened the WAL buttons until `make web` — CI builds the web first, so
shipped images are unaffected, but the lesson is to build before judging a page locally.

All six are dispatched as `fix/settings-audit`.

### Dispositions (`fix/settings-audit`, 2026-11-21)

All six are closed by the dev pass; 1/2/3/6 carry fresh rendered proof on the fixed build, and the two
identity fixes are pinned by service/page tests plus a live `POST …/avatar` → 200 for a token principal:

| # | Defect | Disposition |
|---|---|---|
| 1 | `/setup`: a coercion-invalid value produced a keyless error — empty KEY cell, no row mark, Advanced group stayed collapsed (a keyed `config.Validate` error did mark it) | **FIXED.** `setup.Coerce` now returns `setup.Error{Key, Message}` (it implements `error`; lifted with `setup.KeyOf`), so parse failures mark their row and open a collapsed Advanced group exactly like validator failures. Struct-slice cells are keyed `key[i].field` and attach to the owning row editor by the bracket prefix; the JSON 422 body carries the same key. Every field type is pinned by `TestCoerceFailuresCarryTheirKey` + `TestSetupPageCoercionErrorMarksItsRow`; `TestUnknownSetupKeyCannotPassSilently` is the negative control. |
| 2 | `/setup`: a zero-edit Save rewrote a hand-written 28-line config into a 205-line canonical document and silently deleted an operator comment | **FIXED (backup, not comment preservation).** Toml keeps comments only on decode; preserving them needs a trivia-preserving writer (new dependency or hand-rolled parser, rejected by law 1), so the first canonicalising rewrite copies the previous file to `spindle.toml.bak-<utc>` (once), a byte-identical save writes nothing, the page states both next to Save, and the banner/API report the backup. Gates: `TestSaveSetupKeepsOneBackupOfExistingFile`, `TestSetupPageCanonicalSaveKeepsOneBackup`. |
| 3 | Push policy: after a dry run the Remove card said "No policy document is stored" while the document was live | **FIXED.** The dry-run panel now reads the stored version into `Exists` while still evaluating the posted text, so the verdict and the stored-policy facts (typed-confirm removal included) render together. Gate: `TestSettingsSeamsPolicyRawSaveAndDryRun`. |
| 4 | Org invitations were email-only (`type=email`) and an email-less static-token principal could not claim an invitation | **FIXED.** The org form addresses an **email or username** (the repo Access tab's rule) and the service matches the invited subject by email **or** principal name; a username-addressed invite is claimable by its token principal, a foreign principal still conflicts, and email invites are unchanged. Gate: `TestOrgUsernameInviteAcceptedByTokenPrincipal`. |
| 5 | "Regenerate avatar" always 404'd for a static-token principal (`no verified email`) even though the control was offered | **FIXED.** The seed falls back to the principal NAME (the initials-chip fallback) when no verified email exists; only an empty principal is unmappable. The seed-leak gate is now token-aware (a short username colliding with the static `shape-rendering` attribute is not a false rejection). Gate: `TestUserAvatarErrorBranches` + the updated regenerate assertions. |
| 6 | Non-admin Access page rendered a duplicate nested card (unclosed `<section data-access-readonly>`) | **FIXED.** The read-only section closes before a separate **Changing access** card, so there is exactly one `data-access-readonly` section, one "Visibility & role bindings" heading and no nesting. Gate: `TestSettingsRepoAccessReadOnlyCardIsNotNested`. |

Rendered proof (`/tmp/opencode/aud3fix/shots/`, 1440×900 and 390×844, all `scrollWidth === clientWidth`):
the coercion row marked with its Advanced group open, the Save copy + post-save backup banner, a policy
dry run with the Remove card intact, and the triage principal's single read-only Access card.

### Threads round-3 verification (`aud3-threads.md`)

**Verdict: 7 of 9 targeted scenarios pass; not yet parity.** Every round-2 headline FAIL is closed
except retention:

- **Passing live:** PR `synchronize` in **0.31 s** with `sweep_interval = 1h`; forward-only head with
  no duplicate runs across rapid pushes, a doubled notify and six restarts; `needs:` holding `test`
  until `lint` finished and cancelling it on failure; a minute cron firing exactly once per minute with
  no double fire across a restart; dispatch with typed inputs from the page and the API (bad input →
  422 naming it); `contents: write` job push accepted while `contents: read` and fork runs are refused;
  the first-time-contributor gate (`pending_approval` with unstarted jobs, approve runs, deny cancels,
  maintainer runs skip); and the end-to-end story — a failing run blocks a `require_checks` merge, a
  green fix merges.
- **FAIL — retention on a multi-repo instance:** `retentionGate` is one field on the single
  `threads.Service`, so only the **first repository swept each hour** runs `Retain`; a terminal run
  backdated 3 h with a 2 h retention survived six restart cycles. On a single-repo instance the
  mechanism is correct (old run + logs gone, young kept, the check still gates).
- **FAIL — run-list filters render 50 / 28 / 29 px wide** at both 1440 and 390 (a `min-w-0` wrapper
  around a `width:100%` input), so they are unusable even though the URL/API filters work.

Noted but not counted: no Threads section under `/api`; `uses:` is checkout-only; a job-token push
retriggers workflows (GitHub suppresses token pushes); the run page shows declared rather than
effective permissions; a blocked dependent reads `cancelled`; a fresh start can skip the first cron
tick. Artifacts and caches remain explicitly out of v1 scope.

Both FAILs and the five smaller gaps are dispatched as `fix/threads-retention-filters`.

### Closing verification (`aud4-closing.md`)

**Verdict: 9 / 9 PASS in real Chromium** (real clicks, real form POSTs with a real `Origin`, htmx running),
which matters because two fix passes had proved their work on an engine that cannot run htmx or drops
`Origin`. Screenshots in `/tmp/opencode/aud4-closing/`.

1. **`/setup` coercion errors** mark the row **and** open the `cache` Advanced group; the KEY column
   names the key for duration, size, int and a struct-slice cell (`bundles.strategy[0].keep`); the API
   twin's 422 carries `key`.
2. **First canonicalising save** writes `spindle.toml.bak-20261121T102634Z`, byte-identical to the
   hand-written file (comment preserved in the backup), and the page says so; a second save leaves
   md5/size/mtime untouched with one `.bak` and an identical `config dump`.
3. **Policy dry run** shows "1 allowed / 0 denied" *and* keeps the typed-confirm Remove card; the API
   still serves the stored policy.
4. **Org invite by username** is accepted by the email-less principal ("accepted — bound to org",
   roster `member`), membership is effective on the git path (read 200, write 403, org settings 403),
   and a different principal still gets 409 on a still-pending invite.
5. **Avatar regenerate** without an email → 303 + "Avatar regenerated" with the image installed. The
   seed is deterministic, so a repeat regenerate is byte-identical (expected).
6. **Non-admin Access page**: exactly one "Visibility & role bindings", a separate "Changing access",
   zero forms, zero nested sections.
7. **Retention**: three boots each swept **both** repos with expired runs in the same pass while
   leaving the young run alone; restarts never starved either repo.
8. **Run-list filters**: 265 px each at 1440 and 160 px each at 390, filtering works by clicking,
   `sw === cw`, and `run workflow` remains the only `.primary`.
9. **Job-token push** landed with **no** run while a human push 30 s later created one; a
   `needs:`-blocked dependent reads **`skipped`** (never started) with the check state `skipped`.

**Out-of-scope observations, dispositioned:** an out-of-range integer silently saving `MinInt64` is a
real bug (dispatched, with the invariant that every numeric coercion must produce a keyed error);
a foreign principal's refusal on a *live* invite says "invitation no longer pending" (message fix,
same pass); a `contents: write` job whose checkout is shallow cannot push (GitHub behaves identically,
so the default stays — documented with the `fetch-depth: 0` remedy and a legible error).

## Closing audit (`aud4-closing.md`, 2026-11-21)

**Verdict: 9/9 PASS in real Chromium 157** (headless, CDP, canonical host `walgit.localhost`), with the
round-3 fixes re-verified by clicking the controls as a user: keyed coercion errors marking rows and
opening the owning Advanced group, the first-save backup and byte-identical second save, policy
dry-run plus the stored-policy facts, the username org invitation accepted by an email-less token
principal, avatar regeneration in token mode, the single non-admin Access card, retention sweeping
every repository across restarts, usable run-list filters at 1440 and 390, and job-token push
suppression plus `needs:` `skipped`. Every measured page asserted
`document.documentElement.scrollWidth === clientWidth`.

Three observations were outside the nine items; each is closed here:

| Observation | Disposition (`fix/setup-overflow`) |
|---|---|
| 3. An out-of-range integer in a `/setup` field saved `MinInt64` with a success response (`bundles.strategy#0.keep = 99999999999999999999`). | **FIXED.** `setup.parseSettingInt` fell through `strconv.ParseInt` to `ParseFloat` and converted with `int64(f)`, which saturates on amd64. It and a new `parseSettingUint` now bound the float path to exactly representable values, and `Coerce` bounds every width (int8..int64, uint8..uint64, ByteSize, Duration, floats) with a keyed `setup.Error` naming the accepted range; `config.ParseByteSize`'s 2^63 boundary and `config.ParseDuration`'s pre-multiply overflow check are fixed the same way. `PUT /api/v1/setup` answers 422 with the key and writes nothing. Gates: `TestNumericCoercionNeverSaturates`, `TestStructSliceCellOverflowIsKeyedAndChangesNothing` (file byte-identical), `TestSetupAPIRejectsOutOfRangeNumbers`, `TestNumericBoundaryMessagesNameTheRange`; rendered proof on the page at 1440 and 390. Doc: 11_config_cli.md §3.7. |
| 1. A foreign principal opening a still-pending invitation got `409 invitation no longer pending` — the wrong cause for a live invite. | **FIXED (accurate wording, still 409, no leak).** The top-level routes carry only an id and there is no id→scope index, so an id absent from the caller's inbox names both knowable causes ("not addressed to your account, or already accepted or cancelled"); a row that outlived its issuer object says "no longer valid" naming the spent/swept-repo causes, and a repo deleted under a surviving object names the deletion. Same path for org and repo families. Gates: `TestInvitationRefusalWording`, `TestPublicFriendlyInviteErr`. Doc: 01_identity_permissions.md §7 + Decisions. |
| 2. A `contents: write` job with the default (shallow) checkout cannot push: `HTTP 400` with a 113-byte body. | **FIXED as documentation + legible refusal (no default change — GitHub behaves the same).** The checkout section of 15_threads.md states plainly that pushing requires full depth and shows the exact `with: fetch-depth: 0` snippet; the receive path refuses a leading `shallow <oid>` section with an ERR packet naming the cause and remedy (`fetch-depth: 0`, `git fetch --unshallow`) on both HTTP and SSH and logs repo/principal, so the job log shows the fix instead of a bare 400. Advertising `shallow` stays a follow-up. Gates: `TestParsePushRequestShallowRefused`, `TestParseShallowSection`, `TestPushFromShallowCloneNamesCauseAndRemedy`, `TestSSHReceivePackShallowCloneNamesRemedy`. Docs: 15_threads.md (checkout + Decision h), 04_git.md §7 step 1 + Decisions. |

### Post-audit follow-ups (2026-11-21, user-reported and self-found)

- **Markdown soft breaks** (`fix/markdown-softbreaks`): prose was wrapping at the author's source line
  width because the renderer emitted `<br>` for every newline — the `marked` `breaks: true` carry-over.
  Now a soft break folds to a space, only two trailing spaces or an unescaped trailing backslash
  hard-break, and lazy continuation works for lists and blockquotes. Pinned by
  `testdata/readme-breaks.md` goldens; measured 4 source lines → 3 rendered lines at 1440 and 7 at 390.
- **Threads job semantics** (`fix/threads-job-semantics`): job-level `if:` (server-decided before
  claim, `needs.<job>.result` and `success()/failure()/cancelled()` contexts, false ⇒ `skipped`) and
  `continue-on-error:` (record says `failure` + `tolerated: true`; the run fold ignores it) now apply;
  a step `id:` (previously a parse error) is accepted with `steps.<id>.outcome|conclusion`, and
  `outputs` parses but reads empty (documented). Real-container e2e for both.
- **CLI** (`fix/cli-threads-cancel`): `threads cancel` and `threads rerun` call the routes that have
  existed since B1/B2 instead of the stale "no route until the runner lands" message.
- **Docs** (`docs/threads-reference`, `docs/refresh-readme-agents`): `docs/threads.md` is the new
  user-facing reference (and a test parses its fenced examples against the real parser);
  `docs/features/15_threads.md` stays the design record. README and AGENTS were refreshed against the
  code — including the stale `- [ ] packages … not yet implemented` checklist line (packages shipped
  in v0.5.0), the Threads item pointing at the new reference, and eleven new field lessons.
- **Repository pulse / Insights** (`feat/repo-insights`): the parity-audit card's "repository pulse"
  is implemented as `/{owner}/{repo}/insights` — a cached `insights/pulse.json` projection (Pulse,
  Contributors, Code frequency, honest Facts) computed by one bounded `git log --shortstat` in the
  `insights-refresh` task (maintainer due gate + writer-triggered). Clone/view traffic is explicitly
  refused (no git access log exists) rather than invented; the page is server-rendered with CSS bars,
  no JS and no chart library, gated exactly like the Code page. Docs: `docs/features/18_repo_insights.md`;
  cost: EVIDENCE E20 (16.6 ms at 1k commits, 156.2 ms at 10k, one git call, one page GET).
- **CI determinism** (`fix/notify-coverage`): `internal/notify` coverage varied 94.9–95.3 % run to run
  because five fan-out loops chose randomly between a ready semaphore slot and a cancelled context,
  so `make cover` failed at random. Slot acquisition now re-checks the context after taking a slot
  (same policy, chosen by construction); 20/20 runs report **97.3 %**.
