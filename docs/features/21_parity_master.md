# 21 — Threads parity master plan (Forgejo-first, security-reviewed)

**Status:** complete (2026-11-27). The five waves and the Wave-6 follow-up tracks have landed; the
final matrix, the one remaining `partial` and the decisions awaiting acknowledgement are §1.13. This
document remains the single source of truth for Actions parity work in Threads.
It reconciles two capability cards into one ordered, parallelisable, security-reviewed plan. It does not
replace them: they stay the detailed capability inventories (every key, context, action kind and service
they list is still the requirement), and this document is what an agent implements from.

| Field | Value |
|---|---|
| Source card A (Forgejo) | `01a21819b2e6000000a918a3b0fdea` — "Threads: Forgejo Actions parity (100%)", 69 items, 10 lists |
| Source card B (GitHub) | `01a2181c57750000003249765b2a61` — "Threads: GitHub Actions parity (100%)", 90 items, 10 lists |
| Master card | `01a218365e4f000000fa0fe360ab27` — "Threads: parity master plan (Forgejo-first)", Spindle board, Backlog, label `Epic`, assigned `crueber` |
| Items reconciled | 159 source items → **119** canonical entries (`PAR-001` … `PAR-202`): 43 source items fold into a shared entry, 3 entries are born in reconciliation (§10), one checklist item is superseded by the product rule, and the gaps in the numbering are intentional |
| Docs revision checked | Forgejo **v17.0.2** (LTS v15.0.12) + GitHub Actions docs at 2026-11-26, per the source cards |
| Baseline verified | 2026-11-26 against `internal/threads/**` at `origin/main` (`1a32809e`) |

**Why one document and not two.** Two parity matrices would encode two different answers for keys the
two platforms spell the same (`permissions`, `continue-on-error`, implicit concurrency): Forgejo's docs
say "ignored", GitHub's say "applies". Threads has one engine and one job spec; it needs one answer per
key, with the other platform's behaviour recorded as a divergence. The master list below is that answer.

**Numbering.** Neither card's proposed path is usable: the Forgejo card says `docs/features/16_actions_parity.md`
— **16 is `16_moderation.md`**; the GitHub card correctly notices that 17 is `17_file_edit.md` and proposes
`21_github_parity.md`. This document takes **21**, for both platforms, and the per-capability matrix lives
beside it (`docs/features/parity/`, §9). There is no `16_actions_parity.md` and no `21_github_parity.md`;
do not create either.

---

## 1. Goal and definition of done

Make Threads run real Spindle workflows that were **written for Forgejo or GitHub**, after a one-way
conversion that the product performs explicitly (never by reading another system's directory at runtime).

> **Definition of done.** Every documented Forgejo v17 and GitHub Actions key, trigger and filter,
> context, expression function and operator, `uses:`/action-metadata form, workflow command, runner
> capability and platform service **either behaves identically here, or is an operator-approved written
> non-goal recorded in §7/§8 with a named signatory**. The plan closes at zero unapproved gaps: no `missing`
> row, and no `partial` row without a wave or a decision.

Three properties make that checkable rather than rhetorical:

1. **A committed conformance corpus.** `internal/e2e/testdata/parity/` holds byte copies of real
   workflows plus a `SOURCE` stamp (repository, path, commit SHA, fetch date) — byte copies with one
   sanitization: the private host is rewritten to `git.example.com` in every copied source and golden, so
   the publish transform (which rewrites that host in every text file) cannot change their bytes and break
   the stamped hashes:
   - **Forgejo (the operator's own, first):** `weave/.forgejo/workflows/ci.yml`,
     `weave/.forgejo/workflows/publish-github.yml`, `scriobh/.forgejo/workflows/ci.yml`,
     `scriobh/.forgejo/workflows/release.yml`.
   - **GitHub (4–6 real public workflows, per the GitHub card's audit — re-verify and pin the SHA at fetch
     time):** `actions/starter-workflows` `ci/node.js.yml`; `goreleaser/goreleaser`
     `.github/workflows/build.yml`; `actions/upload-artifact` `.github/workflows/test.yml`;
     `docker/build-push-action` `.github/workflows/test.yml`; `sigstore/cosign-installer` `action.yml`
     plus a workflow that uses it; `actions/checkout` `.github/workflows/test.yml`.

   Each entry is converted once by the importer (§2.3) to `.spindle/workflows/`, the converted file is
   committed as a golden, and the harness drives discovery → run creation → job execution → checks →
   artifacts and asserts the outcome. A corpus entry passes only when it runs **unmodified after
   conversion**. A rename is acceptable only where the matrix row it belongs to is an *approved non-goal*,
   and the golden then asserts the rename is the **only** difference.
2. **A living matrix with a gate.** One row per `PAR-` entry in `docs/features/parity/parity.json`, rendered
   to `matrix.md`, checked by `make parity` (§9). A row is green only when its acceptance test is committed
   and passing.
3. **No silent acceptance.** A key we do not implement is a named parse error or a named refusal — never
   inert. This is the one rule both source cards agree on and the baseline already follows for
   `pull_request.types` and container keys; the plan extends it to `permissions` scopes, remote action
   forms and workflow commands.

**Priority classes** (kept from both cards, and they still bind):

- **P0** — accepted syntax with *different* semantics: the file parses, the result is wrong.
- **P1** — blocks ordinary real workflows (parse error, missing trigger/context/action kind).
- **P2** — documented long tail.
- **P3** — ecosystem and ergonomics.

Waves are ordered by *when a real repository stops needing its old CI*, not by priority alone: priority
decides order **inside** a wave.

### 1.1 Wave 1 outcome (2026-11-26)

Wave 1 is merged. The changes on `main`, in landing order:

| Merge | What landed |
|---|---|
| `e4a3f949` | syntax core: `on.push.tags`, `needs: <string>`, `matrix.include`/`exclude` with GitHub's merge rules, `startsWith`/`contains`/`format`, the closed `github`/`forgejo`/`forge` fields including `actor`, `secrets.*` validation, and `${{ }}` validation in every documented step site |
| `14cc2f89` | runner env: `GITHUB_TOKEN`/`FORGEJO_TOKEN` alias the scoped job token, `GITHUB_SERVER_URL`/`GITHUB_API_URL`, `ubuntu-latest` label mapping, checkout `fetch-tags`/`ref`/`path`, `$GITHUB_ENV`/`$GITHUB_PATH` with protected-name refusals, and job `continue-on-error` with GitHub semantics (Forgejo divergence recorded, D-4) |
| `6ce73cf9` | the one-way importer (`spindle threads import --from forgejo\|github`), `--apply` on a branch only |
| `19549aa8` | tag pushes create runs, `github.ref_name` is the tag, and a tag/branch pair with the same short name is independent |
| `96276b4d` | implicit supersede (a newer push on the same full ref cancels the older in-flight run; concurrency groups reconciled) and `make parity` with the living matrix |
| `df8464b5` | runtime substitution in step fields (`github.*`, `env.*`, `secrets.*`, `matrix.*`, `inputs.*`, steps/needs outputs; one pass; absent supported value empty, undeclared output an error), the run ref carried to the container so a tag job sees the tag in `GITHUB_REF`/`GITHUB_REF_NAME`, and secrets masked |

**The corpus.** All four real Weave/Scriobh workflows convert, parse and plan with **zero refusals**; the
tags-only file fires on a matching tag and on nothing else, and `scriobh-release` keeps its four
`include` cells. The goldens are the importer's own output (`f13a2bc0`).

**The matrix** (`docs/features/parity/parity.json`, 119 rows) at Wave 1 close: **20 `have`**, **50
`partial`**, **49 `missing`** (no `non-goal` yet). Wave 1 owns 24 rows: 20 green and 4 still `partial` —
`PAR-030` (substitution in `runs-on`/`container.*`/`services.*`), `PAR-032` (the remaining `github.*`
identity/ref fields), `PAR-056` (an expression-valued `continue-on-error`), and `PAR-200` (the corpus does
not yet run unmodified end to end, and the GitHub half of the corpus is absent). Wave 2 owns the other
56 rows (30 `partial`, 26 `missing`).

**What remains for Wave 2.** Finish the four Wave-1 partials above; then the Wave-2 items that need the
D-2/D-3/D-5/D-10 machinery — `pull_request_target`, `workflow_run`, remote `uses:` with the allowlist and
SHA pinning, and the cache service — plus the trigger/context/command long tail the matrix assigns to that
wave. The matrix is the list; this paragraph only names the themes.

### 1.2 Wave 2 outcome (2026-11-26)

Wave 2 is merged. The changes on `main`, in landing order:

| Merge | What landed |
|---|---|
| `3dd07cbd` | the closed `permissions:` vocabulary (`contents`, `issues`, `pull-requests`, `id-token`; `read-all`/`write-all`; every other scope a named parse error), the job token as a scoped platform-API credential, and `enable-openid-connect` with the fixed OIDC claim set, subject templates, required audience and five-minute TTL |
| `bb1b6222` (#789) | runner surfaces: rerun-failed, arbitrary `shell: <cmd> {0}` plus `python`/`python3`, service health gates, `$GITHUB_STEP_SUMMARY` (and workflow commands, groups, `add-mask`, `stop-commands`, `set-output`, debug), the hardened `$GITHUB_OUTPUT` heredoc, repository-local reusable workflows, and composite limits |
| `aca7b492` | environments (`environment:` forms, a structural `${{ }}` parse error, wait timer / team reviewers / prevent-self-review / deployment branch+tag allowlist), pending-deployment UX, concurrency's one running + one pending slot with pending-cancel, the two retention windows and GC live-run guard, `name:docker://…` runner labels, and operator limits |
| `5b801214` | `paths`/`paths-ignore` and the filter dialect, the full emitted `pull_request.types`, `release`, `issues` and `issue_comment`, cron names/macros/`L`/`NW`/`#`/per-entry timezone, dispatch `number`/`environment`, the bounded `github.event.*` payload plus `$GITHUB_EVENT_PATH`, `runner.*`/`RUNNER_*`, and `vars.*` |

**The matrix** (`docs/features/parity/parity.json`, 119 rows) at Wave 2 close: **45 `have`**, **37
`partial`**, **37 `missing`**, no `non-goal`. Wave 2 owns 56 rows: 25 green, 17 `partial`, 14 `missing`.

**Three parser gaps carried as follow-ups** (named in their matrix rows, never accepted-and-inert):

- `on.workflow_call` typed inputs and `jobs.<id>.secrets` for reusable workflows (`PAR-075`): a
  repository-local callee's jobs expand correctly, but `with:`, callee `outputs:` and secret passing
  are refused by name.
- `github.action_path` (`PAR-074`): a composite action that reads it is a named parse error rather
  than an empty value.
- `services.<id>.credentials` (`PAR-078`): the pull identity remains the runner's, supplied by its
  docker config or mirror (`--image-prefix`).

**What remains for Wave 3.** The Wave-2 rows still open, in clusters: `pull_request_target`
(`PAR-015`) and the rest of the issue/review/label/milestone trigger family (`PAR-020`); remote and
JavaScript actions (`PAR-070`, `PAR-071`), Dockerfile-action inputs (`PAR-072`) and the composite/
reusable tail above (`PAR-074`, `PAR-075`); the cache service (`PAR-095`, `PAR-096`); matrix/runner
shape gaps — `max-parallel`, `runs-on` substitution, `defaults.run`, timeout parity, `hashFiles`
(`PAR-052`…`PAR-055`, `PAR-041`); org/user secrets and non-secret variables (`PAR-115`, `PAR-117`);
operator policy (`PAR-179`) and the UI-shaped rows (`PAR-190`, `PAR-191`); and offline validation
(`PAR-199`). The matrix is still the list.

### 1.3 Wave 3 outcome (2026-11-27)

Wave 3 is merged. The changes on `main`, in landing order:

| Merge | What landed |
|---|---|
| `2ba1cd52` | reusable workflows with a typed interface: `on.workflow_call` inputs (`string`/`boolean`/`number`, `required`/`default`), declared secrets and output templates; `with:`/`secrets:`/`inherit` (same repository only; cross-repository `inherit` refused, D-6), caller-readable outputs, a 4-level cap with cycle detection, and cross-repository calls pinned to a commit with explicit secrets and an audit record; `${{ github.action_path }}` for every composite level |
| `f5cd5a05` | advanced runner surfaces: background/`wait`/`wait-all`/`cancel`/`parallel` steps with process-group kill on every exit path (D-7), daemonless docker (kaniko/buildah, no socket), `services.<id>.credentials` refused by name with runner-owned registry auth (`--registry-auth-file`), and the operator container grants `--container-privileged`/`--container-network`/`--container-valid-volume`/`--docker-host`, refused in combination with `--allow-fork-runs` (PAR-179, D-8) |
| `1b8f2b89` | the cache service (D-10): `actions/cache` save/restore with `restore-keys`, the `default`/`branch:<n>`/`pr:<n>`/`pr:<n>:untrusted` scopes, no PR→PR reads, fork writes confined, merged-PR scopes discarded, 1 GiB per entry / 5 GiB per repository / 256 entries / 7-day idle eviction; artifact v4 inputs, cross-run artifacts behind the `workflow_run`/D-3 approval hold (409 naming the parent), the delete API, retention-aware 404s and zip-slip rejection; `setup-*` cache recipes named in their refusal |
| `5f138f14` | GitHub-only triggers and merge semantics: `repository_dispatch` (bounded payload, write gate, default branch), `workflow_run` plus the `requested`/`completed` edges and approval hold, `merge_group` as a named refusal until the merge queue card, merge-ref semantics (`refs/pull/N/merge` and the computed merge commit as `GITHUB_SHA`), `create`/`delete`/`deployment`/`deployment_status`, `pull_request_target` per D-2, per-repository `send_write_tokens` (D-13), per-repository caps and one named queue reason per job |
| `c701d547` | the parser accepts the runner's background/`wait`/`parallel` model, so the D-7 keys are authorable rather than a runner-only shape |

Supporting commits in the same wave: `28961a27`/`d367adff`/`b18041b8`/`de16e2a0`/`b8d2b9e5` (the
per-track implementations), `589e9132` (`action_path` runner half), `4afbd921` (background kill
ordering under a job timeout), `8aae80d8` (the `workflow_run` hold's 409), `2070accd` (the e2e keys a
PR run on its payload head), and `5b0536d9`/`a0fa98a1` (matrix regeneration).

**The matrix** (`docs/features/parity/parity.json`, 119 rows) at Wave 3 close: **57 `have`**, **36
`partial`**, **26 `missing`**, no `non-goal`. Wave 3 owns 29 rows: **7 green** (`PAR-018`, `PAR-024`,
`PAR-080`, `PAR-098`, `PAR-099`, `PAR-118`, `PAR-156`), **14 partial** and **8 missing**. The wave also
closed five Wave-2 rows: `PAR-075`, `PAR-078`, `PAR-095`, `PAR-097` and `PAR-179`. Eighteen rows moved
in total — twelve to green, six to partial (`PAR-016`, `PAR-019`, `PAR-021`, `PAR-076`, `PAR-096`,
`PAR-153`).

**Carried partials (named, not silent).**

- **Nested remote `uses:`** — a remote marketplace action inside a composite is still refused; the
  remote-action resolver is PAR-070/PAR-074 (Wave 4). Nested reusable calls themselves resolve
  (`owner/repo/…@sha`, explicit secrets), which is what `PAR-076` landed.
- **Cross-repository execution e2e** — `PAR-076`'s pinned remote callee is unit-tested (resolution,
  secrets, audit, refusals); the execution e2e of a pinned remote callee is not, and is the row's
  remaining acceptance gap.
- **`setup-*` green pending marketplace actions** — `PAR-096`'s cache half is complete and each
  refusal quotes the recipe; the setup actions are marketplace JavaScript (PAR-070/PAR-071), so the
  row cannot turn green before the JavaScript runtime does.
- **Per-organization caps** — `PAR-153` landed the per-repository `concurrency_cap` (0…500) with a
  named hold reason; the per-organization cap needs the org ownership seam and remains open.
- **`merge_group`** — `PAR-016` is a named parse error until the separate **Merge queue** card lands
  the emitter and its `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref; no queue semantics are
  implemented here.
- `PAR-019`'s documented cross-run hand-off now has its test
  (`TestCrossRunRefusedWhileWorkflowRunHeld`), and its note records that trigger-side contract; the
  row stays partial for the cross-run API's own home (PAR-098/PAR-099).

### 1.4 Wave 4 — expressions and flow (2026-11-27)

The expression/flow leftovers from Waves 1–3 landed as one track. What changed:

| Area | What landed |
|---|---|
| expressions (PAR-038, PAR-040) | one evaluator in `internal/threads/workflow` serves `if:` and every `${{ … }}` substitution site: ordered comparisons, value-returning `&&`/`||`, `[ ]` index/property access, the documented `needs.*`/`steps.*` filters, `endsWith`/`join`; the per-site context availability table is enforced at parse **and** evaluation time with a named error |
| `hashFiles` (PAR-041) | a workspace walk in the job container (glob dialect, excludes, deterministic sorted hashing, per-step cache), 1000-file / 100 MiB caps named in the error, no match = empty string; available in step sites and job outputs, a named refusal in a job `if:` |
| flow keys (PAR-052…PAR-055) | `strategy.max-parallel` with CAS admission in expansion order; `runs-on` and `container.image` substitution (the two structural exceptions, D-16); workflow/job `defaults.run` with literal values (D-17); 360-minute job timeout default, expression-valued timeouts and a distinct `timed_out` conclusion (D-18) |
| titles and trust (PAR-057, PAR-058) | `run-name:` rendered once into the run record, list, page and API; a job `if:` may read `secrets.*`/`vars.*`, with an empty secrets map on an untrusted run and no value in the reason or log (D-21) |

**At this close (all four Wave-4 tracks merged):** 92 `have`, 20 `partial`, 5 `missing`, 2 signed
non-goals; the nine expression/flow rows above moved to `have`, the actions wave landed
`PAR-070`…`PAR-074`, the deployments/checks wave landed `PAR-132`…`PAR-136` and
`PAR-192`…`PAR-195`, and the fleet wave landed `PAR-154`/`PAR-155`/`PAR-157`, the runner set
(`PAR-172`…`PAR-178`), `PAR-180`…`PAR-182` and `PAR-196`…`PAR-198`.

The importer now translates `run-name`, `defaults` and `max-parallel` (a defaults value with a
`${{ }}` span, an unknown defaults key, and a max-parallel without a matrix stay named refusals).
`toJSON`/`fromJSON` landed (PAR-037); `case(...)` remains a named parse error because it is not a
function in this language.

### 1.5 Wave 4 — fleet, operations and ergonomics (2026-11-27)

Wave 4's fleet, operations and ergonomics track. It merged with the queue admission, importer and
expression work above; this section records the fleet facts.

- **Schedule operations (PAR-154).** The engine record `repos/<o>/<r>/threads/schedule_ops.json` carries
  per-entry enable/disable without a commit; `GET …/api/threads/schedules` (and `spindle threads schedule
  list`) shows the next fire, the persisted last-fire watermark and the coalesced-tick count; `run-now`
  is a manual trigger that works on a disabled entry and is audited. The sweeper fires at most once per
  entry per pass and reports `Coalesced` — a backlog is never replayed.
- **Prioritization (PAR-155).** Claim order is **priority descending, then FIFO by enqueue sequence**,
  implemented in both the global index (`claimGlobal` sorts the hint) and the per-repository head (a
  `Priority` map orders `Pending` without an extra GET). Priority 0…10 is set only by a manual dispatch
  (`priority` / `--priority`) and never bypasses a hold, a pause, a cap or a trust gate. `GET
  …/api/threads/queue` and `spindle threads queue` render the order.
- **CI pause (PAR-157).** A per-repository `ci_paused` switch (settings, `POST …/api/threads/pause`, page
  toggle; maintain+, audited) stops claims while in-flight jobs finish. The marker is mirrored on the
  queue head so the claim path stays one GET; every queued job names "CI is paused for this repository";
  resume restores claims immediately.
- **Runner scopes, groups, ephemeral workers and sizing (PAR-172/173/175/176).** The admin-set policy is
  the scope (instance / organization / repository); groups add inherited labels and a visibility
  allowlist *intersected* with the runner's policy (materialized onto the record, so a group can only
  narrow); `ephemeral` makes a one-job worker whose record and credential are deleted after its terminal
  attempt; `--max-cpus`/`--max-memory` make a declared ceiling and produce an exact named queue reason.
- **Managed pool (PAR-174) is a signed non-goal (D-14).** `spindle runner pool` refuses by name; the
  documented path is scale-out ephemeral/capacity-1 workers under the operator's supervisor.
- **Offline registration and fleet operations (PAR-180/181).** `spindle runner serve --config` serves
  several `[[runner]]` scopes from one process with 0600 credential files; list/drain/resume/rotate/
  deregister exist for the runner (own credential) and the host administrator (audited). Rotation has a
  15-minute overlap, so a worker never holds a dead token; drain claims nothing new; deregister deletes
  the record and credential; revoke remains a real 401.
- **Isolation and stock images (PAR-177/178).** Per-attempt container/network/volume names are unique and
  deterministic; the capacity model and the untrusted default are stated in `docs/threads.md` §8.3. The
  stock catalogue (node/python/go/rust/docker/Ubuntu) is documented and printable (`spindle runner
  images`) and every entry is the upstream image **pinned by manifest digest**; a workflow naming a
  stock label with no `container.image` resolves to that pinned image, a runner's own
  `label:docker://…` mapping and an explicit workflow image both keep precedence, and an unknown label
  is refused by name (`spindle runner images --label …`). spindle does not publish or mirror those
  images itself; an air-gapped instance uses `--image-prefix`.
- **Non-goals (PAR-182)** are the signed D-15 set — LXC/host backends, hosted Windows/macOS, artifact
  attestations — each refused by name in code (label declare, `runs-on` queue reason, permissions) and
  listed by `threads.NonGoals()`.
- **Authoring, notifications and CLI (PAR-196/197/198).** The validator runs the push parser and reports
  key paths and lines; the Threads page gains an editor/validate pane; a user can watch a repository and
  gets one in-app notice per failed run with its link (optional webhook), and the same record is
  delivered by email through the operator-configured stdlib SMTP notifier — off by default, masked with
  the run's job mask values, and deduped by the inbox key (D-27);
  `threads run --input/--priority`, `threads watch`, `threads jobs` and the schedule/pause/queue verbs.

### 1.6 Wave 4 outcome (2026-11-27, final wave)

Wave 4 merged as four tracks. Each is recorded above; this is the consolidated result and the
honest end state of the programme.

| Merge | Track | What it closed |
|---|---|---|
| `432f56c7` | actions ecosystem | remote `uses:` resolve → pin → digest-verify → embed, under `[threads.actions]` (`deny`/`allowlist`/`open`, `require_sha`, instance-or-one-source `source`, `max_bytes`); JavaScript actions with a declared Node minimum (`node20`/`node24`), Dockerfile/registry actions with no host socket, nested remote composites (depth 10, cycles refused); `setup-*` cache mapping; `action_loads` on the run header; named refusals |
| `79ed93e3` | deployments, checks and API | deployment records/statuses and the `…/api/threads/deployments` API; the `deployments` permission scope; one serialization slot per environment with a visible queue reason; the environment UI (protection, deployments, secrets, variables); check annotations, badges, run-list filters; per-job cancel (soft/force); environment `vars.*` |
| `bc32f697` | fleet and operations | schedule enable/disable/run-now/coalesce; CI pause; priority 0–10; failure notifications and per-user subscriptions; runner groups, ephemeral one-job workers, CPU/memory ceilings, offline `[[runner]]` config, drain/resume/rotate/deregister; `threads explain`; signed non-goals (D-14, D-15) |
| `e120621a` | expressions and flow | ordered comparisons, value-returning `&&`/`||`, `[]` access and filters; the per-site context-availability matrix; `hashFiles`; `strategy.max-parallel`; `runs-on`/`container.image` substitution (the two structural exceptions, D-16); workflow/job `defaults` (D-17); 360-minute `timeout-minutes` with `timed_out` (D-18); `run-name`; job `if:` secrets (D-21); importer translation of `run-name`/`defaults`/`max-parallel` |

**Matrix at Wave 4 close** (`docs/features/parity/parity.json`): **119 rows — 92 `have`, 20 `partial`,
5 `missing`, 2 `non-goal`** (the three `missing` rows and one `partial` closed before Wave 5 are
recorded in §1.7/§1.8). By wave:

| Wave | Rows | `have` | `partial` | `missing` | `non-goal` |
|---|---|---|---|---|---|
| 1 | 24 | 19 | 5 | 0 | 0 |
| 2 | 55 | 43 | 8 | 4 | 0 |
| 3 | 21 | 12 | 7 | 1 | 1 |
| 4 | 19 | 17 | 1 | 0 | 1 |

Wave 4 owns 19 rows: 17 turn `have` (`PAR-057`, `PAR-117`, `PAR-132`/`PAR-133`/`PAR-134`/`PAR-136`,
`PAR-154`/`PAR-155`/`PAR-157`, `PAR-180`/`PAR-181`, `PAR-192`…`PAR-195`, `PAR-196`, `PAR-198`), one
stays `partial` (`PAR-197`, email), and the last is the signed non-goal `PAR-182` (D-15). The
previously signed `PAR-174` (D-14) is a Wave-3 row and remains `non-goal`. The wave also **closed**
rows planned in earlier waves: the expression/flow set (`PAR-038`, `PAR-040`, `PAR-041`,
`PAR-052`…`PAR-055`, `PAR-058`), the actions set (`PAR-070`…`PAR-074`), and the fleet set
(`PAR-172`/`PAR-173`/`PAR-175`/`PAR-176`/`PAR-178`).

### 1.7 Programme status at Wave 4 close

The programme is complete at five waves: every row is either green, or a signed non-goal, or a row
whose gap is named below. Nothing is silently absent.

#### No `missing` rows remain

The Wave-4 follow-up closed `PAR-015` and `PAR-115` (above); Wave 5 (§1.8) closed the last three —
`PAR-003`, `PAR-039` and `PAR-199`. Every row is now `have`, `partial` with a named clause (below) or
a signed non-goal, and `make parity-closed` is the check that it stays that way.

`PAR-019` closed in Wave 6 (§1.10); its matrix-metadata follow-up is gone.

#### The one `partial` row, and the clause that stays open

| Row | Capability | Open clause | Owner |
|---|---|---|---|
| `PAR-016` | `merge_group` trigger wired to the merge-queue card | Named parse error by design until the separate **merge-queue** card lands the emitter and the `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref. No queue semantics here. | T3 |

`PAR-037`, `PAR-081`, `PAR-177` and `PAR-197` closed in the Wave-6 ecosystem leftovers (§1.12):
the expression row's acceptance names the real platform function set (D-25), the daemonless builder
has registry evidence (D-8's row), the stock catalogue is digest-pinned and resolves at fetch time
(D-26), and email is a configured-off-by-default stdlib notifier (D-27).

#### External dependencies (named, not implied)

- **`merge_group` (`PAR-016`)** — the separate **Merge queue** backlog card: the emitter, the
  `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref and its gating checks. Nothing here can
  substitute for it.
- **The public GitHub mirror's CI** — the published tree's workflows install `git`, Chrome and the Go
  and Node toolchains at run time because the merge gate uses the private prepped image
  (`github.com/crueber/spindle-ci@sha256:…`). Moving the mirror to an image-based gate needs a
  **public CI image**; until then the mirror's gate is slower and depends on the package mirrors.
- **A user secret scope (`PAR-115`)** — declined rather than blocked (D-22): a run executes as the
  repository, so a per-user credential has no honest reader. An environment scope exists; a user one
  would need a product decision that contradicts the trust model.

#### The two signed non-goals

- **`PAR-174` — managed runner pool (D-14).** `spindle runner pool` refuses by name; the documented
  path is scale-out ephemeral or capacity-1 workers under the operator's supervisor.
- **`PAR-182` — LXC/host backends, hosted Windows/macOS, artifact attestations (D-15).** Refused by
  name at the label declare, the `runs-on` queue reason and the permissions vocabulary, and listed by
  `threads.NonGoals()`. (A remote `uses: actions/attest-build-provenance` is governed by the action
  policy like any other remote action; the *enforced* attestation refusal is
  `permissions: attestations`, which is a parse error.)

### 1.8 Wave 5 — core leftovers (2026-11-27)

The last three `missing` rows closed here (§1.7). What landed:

| Area | What landed |
|---|---|
| secret scopes (`PAR-115`/`PAR-116`, D-22) | Organization-level secrets with a per-secret repository policy (`all`/`selected`; an unselected repository gets nothing), resolved **environment > repository > organization** and recorded per job as `secret_scopes` (scope only, never a value). One masking rule shared by server and runner (`threads.MaskValues`): longest-first, multi-line whole and line-by-line, short values as whole tokens, and the server re-masks every stored frame and step result. The org Settings tab and `/api/v1/orgs/{org}/threads/secrets` are owner-gated and audited. **D-22: a user scope is declined** — a run executes as the repository, not a person. Fork and `pull_request_target` runs resolve none, proven by `internal/e2e/threads_secrets_test.go` and `threads_pr_target_test.go`. |
| `job`/`strategy` contexts (`PAR-039`) | `job.status` (`success`/`failure`/`cancelled`, a continue-on-error failure stays success) in a step field and a step `if:`; `strategy.job-index`, `job-total`, `fail-fast`, `max-parallel` at every cell-scoped site (step, job name, job lock key, job outputs, `runs-on`, timeouts). The closed field set is validated at parse *and* evaluation time; a plain job's strategy context is empty, and a job `if:` or a workflow-level group naming strategy is a named parse error. The server writes each cell's index/total and the runner substitutes the same values. |
| migration banner (`PAR-003`) | The Threads page lists `.forgejo/workflows`/`.github/workflows` files (display only) and prints the exact `spindle threads import --from <platform> --repo .` command when the repository has no `.spindle/workflows`. `threads.Service.ListForeignWorkflows` is a read-gated directory listing; nothing is parsed or imported, no run is created, and discovery still opens only `.spindle/workflows`. The importer's report ends with the `threads validate` next step. |
| `threads validate` (`PAR-199`) | `spindle threads validate [PATH\|-] [--repo DIR] [--json]`: operator-local, running `threads.ValidateWorkflowSource` — the push path's own validator — so the issue text is byte-identical to the server's (pinned over a bad-file table). A file, a directory of direct `*.yml`/`*.yaml` children, or stdin; `--repo` checks `DIR/.spindle/workflows`. Exit 0 valid, 1 invalid/unreadable, 2 usage. `threads explain` is unchanged and shares the parser, not the output. |
| context and substitution partials (`PAR-004`/`PAR-030`/`PAR-032`/`PAR-056`/`PAR-151`) | `spindle.*` is the canonical run context with `github.*`/`forgejo.*`/`forge.*` as value-identical aliases (one closed field set, one availability table); the full github field set resolves with real values where the run has one and named evaluation errors for `actor_id`/`repository_owner_id`/`run_number`/`ref_protected`; `container.image` substitutes per matrix cell; `continue-on-error` and `concurrency.cancel-in-progress` take one expression, evaluated per cell at creation. The remaining structural refusals are documented and named (`services.<id>.image`, `environment`, `defaults.run.*`, the env layers above a step). |

**The matrix at this close:** **119 rows — 112 `have`, 5 `partial`, 0 `missing`, 2 `non-goal`**
(`docs/features/parity/parity.json`, the machine source of truth; `matrix.md` is its render). By wave:

| Wave | Rows | `have` | `partial` | `missing` | `non-goal` |
|---|---|---|---|---|---|
| 1 | 24 | 23 | 1 | 0 | 0 |
| 2 | 55 | 54 | 1 | 0 | 0 |
| 3 | 21 | 18 | 2 | 0 | 1 |
| 4 | 19 | 17 | 1 | 0 | 1 |

The §4 master-list status column is generated from `parity.json` at this close, so a row cannot read
`missing` here while the matrix says otherwise. The two `non-goal` rows are `PAR-174` (D-14) and
`PAR-182` (D-15); the five `partial` rows are the table in §1.7, each with its open clause and
owning track, and no row is `missing`.

### 1.9 Wave 6 — container keys and step state (2026-11-27)

Two `partial` rows closed here, on top of the context/substitution rows in the same wave; the matrix is
now **119 rows — 104 `have`, 13 `partial`, 0 `missing`, 2 `non-goal`**.

| Area | What landed |
|---|---|
| container keys (`PAR-077`, D-23) | `container.entrypoint`/`command` now own the container's process under docker's own semantics (scalar or list spelling; a workflow-owned container is verified alive after `docker start` and an early exit is a named failure). `container.ports` applies as an ephemeral **loopback** publish per port (a host-side port is refused by name), `volumes` apply under the existing `--container-valid-volume` allowlist, `--init`/`--read-only`/`--user`/`--workdir` stay allowlisted, and `--tty` is a named refusal (docker exec has no terminal). `container.credentials` stays a **named refusal** (D-23): the pull identity is the runner's `--registry-auth-file`, so a private image pulls without a workflow password entering the payload or argv. Any container grant — privilege, docker endpoint, shared network, volume allowlist — now refuses `--allow-fork-runs`; before this, only privilege and the docker endpoint did. `internal/threads/runner`'s docker e2e (`TestDockerJobContainerKeys`) proves entrypoint/command/ports/volumes against a real daemon. |
| step state (`PAR-092`) | `$GITHUB_STATE` is implemented as the documented pre→post hand-off: the per-action file under `/spindle/state` is read at the end of the pre/main phase (success or failure) and the post hook receives each value as `STATE_<name>` — the `actions/cache` contract. Names allow `.`/`_`/`-` (a real cache key is hyphenated); `::save-state::` is the deprecated spelling and feeds the same map (the file wins); caps are 1 MiB per action / 128 names / 64 KiB per value; state travels by name on the docker client's environment, never argv. Post hooks run in reverse registration order after a failed main, a failing post fails the job, and `TestDockerActionStatePreToPost` proves the hand-off and both failure semantics against a real daemon. `$GITHUB_STEP_SUMMARY` is unchanged. |

### 1.10 Wave 6 — trigger and edge partials closed (2026-11-27)

Wave 6 closes the five trigger/edge rows that had a named service or emitter gap. What landed:

| Area | What landed |
|---|---|
| `PAR-020` issue/review/definition triggers | `pull_request_review` (submitted/dismissed) and `pull_request_review_comment` (created, including review-opened inline threads) emitted by the review service with the PR merge ref and fork trust rule; `label` and `milestone` definitions emitted by the issues service (created/edited/deleted, and a milestone state flip as opened/closed); `issue_comment` now also covers a PR conversation comment. `discussion`/`discussion_comment` and `pull_request_review_thread` are named refusals (no discussion surface; thread resolution is not a separate trigger). |
| `PAR-021` repository/check edges | `fork` emitted by pulls' completed fork task on the original repository; `watch` (`started`) by a first star; `check_run` (`created`/`completed`) for an EXTERNAL CI report, with Threads' own mirror deliberately never re-firing it (no self-trigger loop). `status`, `check_suite`, `registry_package`, `gollum`, `page_build`, `branch_protection_rule`, `public` and `merge_group` stay named parse refusals. |
| `PAR-019` workflow_run hand-off | The acceptance is pinned end to end: a fork parent publishes an artifact, the follow-up is held for approval with nothing executed, and the approved follow-up downloads the parent artifact by `github.event.workflow_run.id`. |
| `PAR-076` cross-repo execution | A pinned remote callee executes on a real runner with an explicit secret, downgrade-only permissions and the audit record; an 11-level call chain is refused at the engine's documented 4-level cap. |
| `PAR-153` org cap | `orgs/<org>/threads/settings.json` (`concurrency_cap` 0…500, org-owner audited) bounds live runs across the org. Admission uses one org index (`orgs/<org>/threads/cap.json`) with a named hold reason and a terminal-fold release that works across repositories; the settings projection reports live/held usage, and removing the cap releases every held run. |

**The matrix at this close:** **119 rows — 109 `have`, 8 `partial`, 0 `missing`, 2 `non-goal`**
(`docs/features/parity/parity.json`). The five rows above moved from `partial` to `have`; the
remaining eight are the table in §1.7 with their clauses unchanged.

### 1.11 Wave 6 — observability and execution (2026-11-27)

The last observability items and the corpus execution tier closed here. Three of them exposed engine
bugs that only a real workflow could show, which is why the execution tier is now part of the evidence.

| Area | What landed |
|---|---|
| log fidelity (`PAR-190`) | A log frame is a **record**, not a bare chunk: `{time, stream, kind, text, step, name, conclusion, exit_code, duration_ms}` stored per `(attempt, seq)`. `kind` names the structural markers (`step_start`, `step_end`, `group_start`, `group_end`, `truncated`), so a reader renders step boundaries and `::group::` folds instead of guessing from text. The page renders a native `<details>` per group (collapsed when the group closes), a time gutter at each line start (the frame's capture time), and a **named** `log frames X..Y missing` gap when the runner could not write a frame — a dropped frame is never silent, and the runner's close-time notice states it too. The panel replays JSON, tails SSE, dedupes by sequence, **re-bases its cursor when a re-run changes the attempt**, and survives a 401 by waiting for re-authentication and resuming from the same cursor. The whole run downloads as `.txt` or `.zip` (`GET …/threads/{run}/logs.txt|logs.zip`, API twins under `…/api/threads/{run}/…`): one entry per job — including a job that produced no output — plus a manifest, with a visible 16 MiB per-job cap. `TestSharedOutputStreamKeepsStdoutStderrOrder` pins the interleaving guarantee the runner relies on (stdout and stderr are the same comparable writer, so `os/exec` gives both one pipe). |
| run graph (`PAR-191`) | `threads.BuildRunGraph` is a read model over the job records (`RunView.Graph`): one node per workflow job key, matrix cells grouped under their job with their own check context and duration, edges from `needs:` carrying the dependency's folded state, longest-path levels, and a `blocked` node naming the dependency that stopped it. The run page renders it as HTML levels (links, `after <key> (state)` edges, a blocked sentence), so it is keyboard reachable and works at 390 px; `TestA11yRenderedPages` drives it in a real browser at both viewports. |
| corpus execution (`PAR-200`) | `make parity-exec` (`SPINDLE_PARITY_EXEC=1`) boots a server and a runner and executes **the committed golden of the real `crueber/weave` publish workflow, byte-unmodified**, dispatching it with `bump: none` and a stub repository script that asserts the argv the workflow computed. A missing docker daemon is a **failure**, not a skip, when the tier was requested. The run also exposed three engine gaps, fixed with it: an authored multi-line `env:` (a literal block) was flattened to one line; `Authorization: token …` (Forgejo's and GitHub's spelling, which the corpus fetch idiom uses) was unparseable; and the default step shell was `sh`, where GitHub/Forgejo default to bash — with `sh -e` still the stated fallback on an image without bash (D-24). A non-root job also gets a writable `HOME` (`/tmp/spindle-home`) so `git config --global` works. |

**Where the execution tier runs.** Not in the standard gate: every CI job is a container job on the
`docker` label with no daemon inside it (the same constraint the image job works around with buildah), so
a daemonless gate cannot start job containers. `make parity-exec` runs on any host with a Docker daemon —
a developer machine, or an operator's host-mode runner — and prints the exact failing job log when a
corpus entry fails. The gate's own `make parity` remains docker-free.

**The matrix after Wave 6:** **119 rows — 112 `have`, 5 `partial`, 0 `missing`, 2 `non-goal`**. The
three closed rows are `PAR-190`, `PAR-191` and `PAR-200`; the five open clauses in §1.7 are unchanged
except that `PAR-081` remains partial on its registry fixture (a buildah push needs a registry the suite
does not run).

### 1.12 Wave 6 — ecosystem leftovers (2026-11-27)

The four remaining ecosystem `partial`s close here; only `PAR-016` (the separate merge-queue card)
stays `partial` by design.

| Area | What landed |
|---|---|
| expression functions (`PAR-037`, D-25) | The row's acceptance named `case`, which is **not** a function in GitHub's or Forgejo's expression language (nor in this engine); the acceptance and capability are amended to the real platform set (`success`/`failure`/`always`/`cancelled`, `startsWith`/`endsWith`/`contains`/`format`/`join`, `toJSON`/`fromJSON`, `hashFiles`). `TestExpressionFunctionSetMatchesPlatform` pins the exact closed set and one conformance value per function; `hashFiles` inside a job remains `PAR-041`. `case(...)` stays a named parse/evaluation error rather than invented semantics. |
| daemonless registry evidence (`PAR-081`, D-8) | `TestE2E_ParityExecutionRegistryBuild` (opt-in `make parity-exec`) starts a local `registry:2`, runs buildah in a job container with **no docker socket**, builds a small image, pushes it, verifies the registry tag, and pulls/runs the image back. Buildah keeps its documented kernel prerequisite (the e2e declares the operator grant); the existing tests still pin the socket refusal and the exit-127 remedy. |
| stock images (`PAR-177`, D-26) | The catalogue is upstream images pinned by manifest digest: `ubuntu:24.04`, `node:22-bookworm`, `python:3.13-bookworm`, `golang:1.25-bookworm`, `rust:1-bookworm`, `docker:27-cli`, each `@sha256:…`. A stock label in `runs-on` with no `container.image` resolves to that image at fetch time; a runner's own `label:docker://…` mapping and an explicit workflow image keep precedence. `spindle runner images` prints the table and `--label` resolves one, refusing unknown labels by name. spindle still publishes no images of its own. |
| email notifications (`PAR-197`, D-27) | The third notification channel is a standard-library SMTP notifier (`net/smtp` + `crypto/tls`, no new dependency), configured off by default under `threads.smtp.*`. It sends one message per (subscriber, failed run) to the subscriber's verified address, rides the inbox record's deterministic key (so it is sent only when the record is first created), and masks the message with the run's job mask values; an unresolvable mask list skips email rather than sending unmasked. The in-app inbox and webhook are unchanged. |

**The matrix at this close:** **119 rows — 116 `have`, 1 `partial`, 0 `missing`, 2 `non-goal`**.
`PAR-016` is the only open clause (§1.7, external dependency: the separate Merge queue card), and
`make parity-closed` is green with `make parity`.

### 1.13 Programme close — final status (2026-11-27)

The programme is complete. `docs/features/parity/parity.json` is the machine source of truth and
`make parity-closed` is the closure gate; `matrix.md` is its fresh render. There is no wave left to
run and no unapproved gap.

| Metric | Final value |
|---|---|
| Rows | 119 |
| `have` | **116** |
| `partial` | **1** |
| `missing` | **0** |
| `non-goal` | **2** (signed) |

**The one remaining `partial`.**

| Row | Capability | Open clause | Owner |
|---|---|---|---|
| `PAR-016` | `merge_group` trigger wired to the merge-queue card | A named parse error by design until the separate **Merge queue** card lands the emitter and the `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref. No queue semantics are implemented here. | T3 |

**The two signed non-goals.** `PAR-174` — managed runner pool (D-14): `spindle runner pool` refuses
by name and the documented path is ephemeral or capacity-1 workers under the operator's supervisor.
`PAR-182` — LXC/host backends, hosted Windows/macOS, artifact attestations (D-15): each is refused
by name at the label declare, the `runs-on` queue reason or the permissions vocabulary, and listed by
`threads.NonGoals()`.

**Decisions awaiting operator acknowledgement.** The 2026-11-26 batch (D-1…D-21) was decided before
its code landed. Six decisions were implemented *under* this plan after that batch — the code, the
docs and the tests carry them — and this is the acknowledgement request. Each line names what
changes if it is declined.

| ID | Decision (as implemented) | If declined |
|---|---|---|
| **D-22** | **User-scope secrets are declined.** Secrets resolve environment > repository > organization, with a per-secret repository policy; a user scope does not exist because a run executes as the repository, not a person. | `PAR-115`'s user half reopens: a per-user secret store plus a rule for which runs may read a person's credential must be designed. It contradicts the current trust model, so it is a product change, not a patch. |
| **D-23** | **A workflow-supplied registry login is refused** (`container.credentials` and `services.<id>.credentials`, named error). The pull identity is the runner's (`--registry-auth-file`), so a private image pulls without a workflow password entering the payload or argv. | `PAR-077`/`PAR-078` accept a per-job throwaway docker config; the password then travels in the job payload (bucket) and docker's environment, and the rule that every container grant excludes fork runs must be revisited. |
| **D-24** | **The default step shell is `bash`** (`bash --noprofile --norc -eo pipefail -c`, GitHub/Forgejo's Linux default), with `sh -e -c` as the stated fallback on an image without bash; `shell: sh` remains available. | Revert to a `sh` default: the real corpus publish workflow's `<<<` here-string stops executing, so `make parity-exec`'s byte-unmodified claim fails until that workflow is rewritten. |
| **D-25** | **`case` is removed from `PAR-037`'s acceptance**; the row covers the real platform function set (`success`/`failure`/`always`/`cancelled`, `startsWith`/`endsWith`/`contains`/`format`/`join`, `toJSON`/`fromJSON`, `hashFiles`). `case(...)` stays a named parse/evaluation error. | `case(...)` becomes a Spindle extension and the row claims parity with a function neither platform has; no platform-conformance value can exist for it, so `TestExpressionFunctionSetMatchesPlatform` would have to fork the language. |
| **D-26** | **Stock toolchain labels resolve to digest-pinned upstream images** (`ubuntu:24.04`, `node:22-bookworm`, `python:3.13-bookworm`, `golang:1.25-bookworm`, `rust:1-bookworm`, `docker:27-cli`, each `@sha256:…`); a runner's own mapping and an explicit `container.image` keep precedence, and `runner images --label` refuses an unknown label. | `PAR-177` returns to `partial`: naming a stock label without `container.image` queues instead of running, and every workflow must name an image or rely on a runner mapping. |
| **D-27** | **Email is a standard-library SMTP notifier, off by default** (`threads.smtp.*`), one message per (subscriber, failed run), deduped by the inbox record key and masked with the run's job mask values. | The third channel is removed (no new dependency), so `PAR-197`'s UI/webhook/email acceptance narrows to two channels or needs a different transport and its own security review. |

Acknowledgement is a reply on the master card; the matrix rows carry `decision: D-22`…`D-27` until
then, and a declined line reopens its row rather than being recorded as settled.

---

## 2. The product rule: Spindle workflows only

This is a product decision, not a technical limitation. It is the one rule the operator has stated
unconditionally, and it changes the shape of several source-card items.

### 2.1 Discovery

- The only discovered location is **`.spindle/workflows/*.yml` / `*.yaml`** — one directory, no nesting,
  at the commit being built. `.forgejo/workflows/` and `.github/workflows/` are **never read**, not as a
  fallback, not as a migration mode, not behind a config flag. The engine has no code path that opens a
  foreign directory. (Baseline: already true; `internal/threads/discover.go`,
  `GitReader.ListWorkflowFiles`. This is `PAR-001`, a verify-and-close row.)
- A repository that carries a foreign workflow directory gets a **migration banner** in Threads →
  Workflows naming the importer command and the files it found. The banner reads files for *display only*,
  on an explicit page request, and never creates a run. (`PAR-003`.)

### 2.2 Language

- The product's words are **Spindle workflow**, **Threads**, the `spindle` CLI, the Threads page. The docs
  say "a Spindle workflow that uses GitHub-compatible syntax", never "a GitHub workflow" or "a Forgejo
  workflow" as a supported artifact.
- The accepted syntax deliberately **is** the Actions syntax, because that is what authors know and what
  the corpus proves. That is a compatibility promise, not an identity: `${{ github.* }}`, `GITHUB_*` env
  vars, `actions/checkout`, `FORGEJO_*` aliases and `forgejo.*`/`forge.*` contexts are **compatibility
  aliases**, documented in one table (WORKFLOWS §12a) with the canonical spelling beside each.
- **Decision D-1 (decided 2026-11-26 — approved):** introduce **`spindle.*`** as the canonical expression context
  and `SPINDLE_*` as the canonical env vars, with `github.*`/`forgejo.*`/`forge.*` and their env spellings
  kept as value-identical aliases. Rationale: `SPINDLE_*` already exists beside `GITHUB_*` in
  `runner/plan.go:540-594`, so the aliasing machinery is already there and costs nothing; without a canonical
  spelling the product's own docs have to teach another company's namespace as the primary one. The alias
  table is the same either way, and parity is unaffected (an alias resolves identically). If the operator
  declines, the fallback is to document `github.*` as "the Actions-compatible spelling Threads accepts",
  with no canonical context — also parity-complete, just less honest about identity.

### 2.3 The importer replaces discovery (what it supersedes)

A source item that asks for native discovery of another system's directory is **wrong** and is replaced by
a converter:

- **`PAR-002` — `spindle threads import`**: `--from forgejo|github <path> [--write] [--json]`, reading one
  file (or a directory) and emitting `.spindle/workflows/<name>.yml` plus a conversion report. Read-only by
  default; `--write` writes the converted file and `--write --apply` commits it **on a new branch** (never a
  force push, never the default branch). It never executes the source, never edits the source directory, and
  exits non-zero naming every key it cannot translate. Security treatment: §7 **S14**.
- The engine then runs the converted file like any other Spindle workflow. Conversion is a **migration aid**;
  there is nothing to maintain at run time, no second parser, and no compatibility mode to test.
- Golden conversions of the corpus are committed, so an importer change is visible as a diff, and the
  conversion report is asserted: a key that cannot be translated must appear in the report, never vanish.

**Superseded source items** (and named in a comment on both cards): Forgejo card list *Triggers & events*
item 1 ("read `.forgejo/workflows/*.{yml,yaml}` (and the `.github/workflows` fallback) as well as
`.spindle/workflows`") and Top-10 gap 4 ("No `.forgejo/workflows` discovery — P1 (blocks every file)"); the
GitHub card has no discovery item, so its *Conformance harness* item (corpus under
`internal/threads/testdata/github-corpus/`) and its *living matrix* item (`docs/features/21_github_parity.md`)
are re-pointed at this document and at `.spindle/workflows/`.

---

## 3. Baseline verified in code (2026-11-26)

The cards' baselines are accurate; these are the anchors an implementer should trust, plus four findings
that changed an item's shape. Verified by reading the code and by two scratch parse runs (since removed).

| Area | Status | Anchor |
|---|---|---|
| Discovery | **have** | `internal/threads/discover.go`; `GitReader.ListWorkflowFiles` |
| Trigger vocabulary | **partial** | `workflow.go:85-176` — `push`, `pull_request`, `workflow_dispatch`, `schedule` only, closed set |
| `on.push.tags` | **missing (P0)** | parsed at `workflow.go:182-186`; `Fires` (`workflow.go:798-834`) reads only `Branches`. Verified: a tags-only workflow returns `Fires("push","main","") == true` **and** `Fires("push","v1","") == true` |
| `needs: <string>` | **missing (P1)** | `Job.Needs []string`; verified parse error `cannot unmarshal !!str 'a' into []string` |
| `strategy.matrix.include/exclude` | **missing (P1)** | `workflow.go:428-452` (refused by name) |
| `${{ }}` substitution | **partial (P0)** | `workflow/template.go` `FillContext` is lenient (unknown spans stay literal); `TemplateSupported` covers `github.*`, `matrix.*`, `inputs.*` only |
| Built-in env | **partial (P0)** | `runner/plan.go:540-594`: no `GITHUB_TOKEN`, `FORGEJO_TOKEN`, `GITHUB_SERVER_URL`, `GITHUB_API_URL`, `GITHUB_ACTOR`, `GITHUB_EVENT_PATH`, `RUNNER_*`, `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_STEP_SUMMARY`, `GITHUB_STATE` (grep: zero hits) |
| `permissions` | **partial** | open map at `workflow.go:329-357`; only `contents`/`id-token` reach `tokens.go:104-112` |
| Concurrency | **partial** | admission CAS (`concurrency.go`); no implicit group, no pending-cancel, `cancel-in-progress` is a bool only |
| OIDC | **partial** | `oidc.go:729-730`: `sub` is always `repo:<o>/<r>:ref:<ref>` (no environment branch); 5-minute TTL; audience required |
| Actions | **partial** | `internal/threads/actions.go`: built-ins + `./path` composite + `docker://`; no remote, no node runtime, no pre/post |
| Artifacts | **have (same-run)** | `internal/threads/artifacts.go`, `runner/artifacts.go`; cross-run refused |
| Cache | **missing** | zero hits for cache in `internal/threads` |
| Container policy | **have** | `internal/threads/containerpolicy.go` (allowlist; volumes refused) |
| CLI | **partial** | `cmd/spindle/cli_misc.go:170-200`: `list`, `show`, `run`, `cancel`, `rerun` only — no `validate`, no `watch`, no inputs |

Four findings that adjust the source items:

1. **`GITHUB_SERVER_URL` is missing and is a Wave-1 blocker.** Weave's `ci.yml` builds its clone URL from
   `${GITHUB_SERVER_URL}` in a workflow-level `env:` block, and Scriobh's release flow curls
   `$GITHUB_SERVER_URL/api/v1/...`. Neither card has an item for the *env var* (they cover
   `github.server_url` the context). Fixed as `PAR-111`.
2. **`needs: <string>` is a parse error, not merely "refused"** — and `needs: test` appears in **both**
   Weave files. It is a Wave-1 blocker, not a P2 nicety (`PAR-050`).
3. **`startsWith(...)` is on Weave's release path** (`if: startsWith(github.ref, 'refs/tags/v')`), so the
   function library is partly Wave 1 (`PAR-037`), not Wave 2 as the Forgejo card's P1 implies.
4. **`docs/threads.md` §11 is stale**: it still says "No OIDC / cloud credentials, no environments or
   protection rules", while §5.5/§5.6 and `15_threads.md` §2.6/§2.7 record both as shipped. The docs track
   owns the correction (`PAR-202`).

---

## 4. Reconciled master list

119 canonical entries. Column meanings:

- **Prov** — provenance: `F` only from the Forgejo card, `G` only from the GitHub card, `B` from both,
  `—` born in this reconciliation (no source item asked for it).
- **Status** — `have` / `partial` / `missing`, verified in code (§3).
- **W** — wave (`1`…`4`); `1V` = verify-and-close row in Wave 1 (test written, no build work).
- **Track** — the owning work track (§6); a second track appears only where an item needs a coordinated
  seam, and the lead track is named first.
- **Sec** — the security decision that governs it (§7), `—` when none applies.

The acceptance column is the **only** definition of green for that row; §9 makes "green" mechanical. Priority
(P0–P3) is folded into the wave — Wave 1 is the P0 set plus the corpus blockers — and survives in the
matrix JSON's `priority` field so a row can be re-ordered without renumbering.

### 4.1 Discovery, file format and migration

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-001 | One discovered location: `.spindle/workflows/*.{yml,yaml}`, no foreign fallback, ever | F | have | 1V | T3 | A repo containing only `.forgejo/workflows/ci.yml` yields zero workflow files and a named hint pointing at `threads import` | S14 |
| PAR-002 | One-way importer `spindle threads import` (converted file + report + golden diff) | F | have | 1 | T1 | Converting the 4 Forgejo corpus files produces files that parse and a committed golden per file; an untranslatable key is named in the report and exits non-zero | S14 |
| PAR-003 | Migration banner: a foreign workflow directory is *shown* with the importer command, never run | — | have | 2 | T6 | The Threads page lists the foreign files and the exact `spindle threads import` invocation; no run is created by visiting it | S14 |
| PAR-004 | Canonical `spindle.*` context / `SPINDLE_*` env with `github.*`/`forgejo.*` as aliases (D-1) | — | have | 2 | T1 | `spindle.ref`, `spindle.event_name` and `github.ref`, `forgejo.ref` resolve to the identical value in one table test | — |

### 4.2 Triggers and events

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-010 | `on.push.tags` fires on tag pushes (`refs/tags/*`) with tag globs and `GITHUB_REF=refs/tags/X` | B | have | 1 | T1 | A `v1.2.3` tag matching `tags: [v*]` starts exactly one run; a non-matching tag starts none | — |
| PAR-011 | A tags-only trigger stops matching every branch push | F | have | 1 | T1 | `on: push: {tags: ["v*"]}` on a branch push creates no run (the current `matchAny(nil)` behaviour is pinned as a failing-first test) | — |
| PAR-012 | `on.push.paths` / `paths-ignore` (and `pull_request.paths`) with `**`, ANDed with `branches` | B | have | 2 | T1+T3 | A docs-only push against `paths: ['src/**']` creates no run; a mixed commit does; a `paths`-only trigger uses the pushed commit's diff | — |
| PAR-013 | Filter glob dialect: `*`, `**`, `?`, `+`, ordered `!` negation, `branches-ignore`/`tags-ignore` | B | have | 2 | T1 | The documented filter cheat-sheet is a passing table test, including ordered include/exclude | — |
| PAR-014 | Full `pull_request.types` vocabulary and its emitters (closed, labeled, ready_for_review, review_requested, …) | B | have | 2 | T3 | Each documented type fires from the real event through the pulls seam; the default stays opened/synchronize/reopened | — |
| PAR-015 | `pull_request_target`: base-tip file, base secrets, write token, head fetchable | B | have | 2 | T3 | A fork PR run executes the base file and never the head's, with secrets resolving and the badge set | S5 D-2 |
| PAR-016 | `merge_group` trigger wired to the merge-queue card (no queue semantics here) | G | partial | 3 | T3 | A queue entry starts a `merge_group` run whose checks gate the merge | — |
| PAR-017 | `release` trigger (created/published/edited/deleted/prereleased/released) with `event.release.*` | B | have | 2 | T3 | Publishing a release runs a workflow that reads its tag and asset list | — |
| PAR-018 | `repository_dispatch` with `types` + `client_payload`, reachable from API/CLI/scoped token | G | have | 3 | T3 | A dispatch exposes `github.event.client_payload.*`; a token without `contents: write` is refused | S10 |
| PAR-019 | `workflow_run` (workflows/types/branches filters, upstream run + artifacts) with the untrusted-trigger rule | G | have | 3 | T3 | A follow-up workflow downloads the first run's artifact and is itself gated when the parent was untrusted | S7 D-3 |
| PAR-020 | `issues`, `issue_comment`, `pull_request_review`(+`_comment`), `label`, `milestone`, `discussion`(+`_comment`) | B | have | 2/3 | T3 | Each parses, is emitted by its owning package, and carries the documented payload; an `issue_comment` run uses the default-branch file | — |
| PAR-021 | Repository/check/deploy triggers: create/delete, fork, watch, status, check_run/check_suite, deployment(+_status), registry_package, gollum, page_build, branch_protection_rule, public | G | have | 3 | T3 | Each parses and fires from a synthesized event; a trigger for a surface spindle lacks is refused by name, not inert | — |
| PAR-022 | Cron parity: names, `@daily`-family macros, `L`/`W`/`#`, per-entry `timezone:` with the DST rule; non-default-branch schedules ignored | B | have | 2 | T1 | GitHub's and Forgejo's cron examples all parse and fire at the documented minute; the Berlin DST entry behaves as documented | — |
| PAR-023 | Dispatch input types `environment` and `number` plus the `github.event.inputs.*` alias | G | have | 2 | T1+T6 | An `environment` input renders a picker and both `inputs.x` and `event.inputs.x` agree | — |
| PAR-024 | PR merge-ref semantics: the simulated merge commit (`refs/pull/N/merge`) is what runs | G | have | 3 | T3 | A merge-only failure goes red; `GITHUB_REF`/`GITHUB_SHA` name the merge commit; the head is still what a fork run cannot control | S13 |

### 4.3 Expressions, contexts and functions

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-030 | `${{ }}` in every documented site: run, with, env, name, if, shell, working-directory, runs-on, container.*, services.*, environment, defaults, outputs, concurrency | B | have | 1/2 | T1 | `container: image: debian:${{ matrix.variant }}` pulls that tag; `runs-on: ${{ matrix.os }}` selects; no supported span stays literal | S3 |
| PAR-031 | `secrets.*` as an expression, empty (never literal) on an untrusted run | B | have | 1 | T1 | `env: TOKEN: ${{ secrets.TOKEN }}` yields the value, a fork run yields empty, and the log masks it | S3 S13 |
| PAR-032 | `github`/`forgejo`/`forge` contexts: full documented field set, identical values; identity/ref fields (actor, actor_id, triggering_actor, ref_type, ref_protected, head_ref, base_ref, run_number, run_attempt, workflow_ref, workflow_sha, action_path, action_ref, repository_owner(+_id)) | B | have | 1/2 | T1 | One table test per field, resolved in a step and in `if:`; `${{ forgejo.actor }} == ${{ github.actor }}` | — |
| PAR-033 | `github.event` payload + `GITHUB_EVENT_PATH`/`FORGEJO_EVENT_PATH` JSON | B | have | 2 | T1 | `jq -r .pull_request.number "$GITHUB_EVENT_PATH"` returns the PR number; `github.event.action` agrees | — |
| PAR-034 | `runner` context and `RUNNER_OS/ARCH/NAME/TEMP/TOOL_CACHE/ENVIRONMENT/DEBUG` | B | have | 2 | T2 | `${{ runner.os }}` is `Linux`; `${{ runner.temp }}` is a writable directory inside the job | — |
| PAR-035 | `vars` context with repository/organization/user/environment variables, documented precedence, reserved-name refusal | B | have | 2 | T3 | `${{ vars.X }}` resolves per scope; a `GITHUB_`/`SPINDLE_`-prefixed name is refused at save | — |
| PAR-036 | `env` context in expressions (workflow → job → container → step layering, visible to `if:`) | B | have | 2 | T1 | `if: env.FLAG == '1'` follows the documented layer order in one table test | — |
| PAR-037 | Expression functions: contains, startsWith, endsWith, format, join, toJSON, fromJSON, hashFiles (the platform set; `case` is not a function) | B | have | 1/2 | T1 | One conformance case per platform function with the platform-expected value; `hashFiles('**/go.sum')` returns a hash inside a job | D-25 |
| PAR-038 | Operators: loose cross-type equality, `<`/`<=`/`>`/`>=`, `[ ]` index, property access, `*`/object filters, value-returning logical and/or (the operand, not a boolean) | B | have | 2 | T1 | `1 == '1'`, `true == 'true'`, `'a' < 'b'`, `github['sha']` and a filter over a list all match | — |
| PAR-039 | `job`/`jobs`/`strategy` contexts (`job.status`, `strategy.job-index`, `job-total`, `max-parallel`) | G | have | 3 | T1 | Each resolves in a step; `job.status` reflects a tolerated failure correctly | — |
| PAR-040 | Per-key context availability, fail-closed with a named error (never a literal, never an empty string) | B | have | 2 | T1 | A context outside the documented set for that key is a named parse/eval error; the availability table is a test table | — |
| PAR-041 | `hashFiles` workspace walk in the job container (patterns, cache, caps) | B | have | 2 | T2 | Two calls in one job hash the same bytes; a >N-file walk is refused with the cap named | — |

### 4.4 Jobs, matrix and flow control

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-050 | `needs` as a bare string parse (`needs: test` ≡ `needs: [test]`) | B | have | 1 | T1 | `needs: test` in both Weave files parses and orders the job; a matrix dependency still waits for every cell | — |
| PAR-051 | `strategy.matrix.include`/`exclude` with the documented expansion rules, object cells | B | have | 1 | T1 | The three documented `include` examples produce exactly the documented cell sets; Scriobh's `include:` list runs unmodified | — |
| PAR-052 | `strategy.max-parallel` and its effect on cell admission | B | have | 2 | T3 | 6 cells with `max-parallel: 2` never have three running | — |
| PAR-053 | `runs-on` matrix/inputs substitution and the `runs-on: {group:, labels:}` form | B | have | 2 | T1 | `runs-on: [docker, "${{ matrix.arch }}"]` selects the matching runner; the group form is honoured or refused by name | — |
| PAR-054 | `defaults.run.{shell, working-directory}` at workflow and job level | B | have | 2 | T1 | The documented `defaults` example runs unchanged; a step-level key still wins | — |
| PAR-055 | Timeout parity: 360-minute job default, expression-valued, `timed_out` conclusion; step-level shape | B | have | 2 | T3 | An idle job is cancelled at the documented limit and reads `timed_out`; a step `sleep` past its own limit kills the container | — |
| PAR-056 | `continue-on-error` as an expression, and the Forgejo divergence recorded (D-4) | B | have | 1/2 | T1 | `continue-on-error: ${{ matrix.experimental }}` tolerates one cell; the matrix row states the Forgejo difference and names the approval | — |
| PAR-057 | `run-name` (expression-valued run title) | G | have | 4 | T6 | The run list and API show the rendered value | — |
| PAR-058 | Job `if:` may read `secrets` and `vars`; an untrusted run reads an empty secrets context | B | have | 2 | T1 | `if: secrets.DEPLOY_KEY != ''` evaluates on a trusted run and is false (not an error) on a fork | S13 |
| PAR-059 | Re-run failed jobs only, preserving successful jobs and their checks | B | have | 2 | T3 | A partially failed run reruns only the failed jobs; the attempt increments; green checks are untouched | — |

### 4.5 Steps, actions and execution

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-070 | Remote `uses: owner/repo[/path]@ref` and full URLs: resolve, pin to a SHA, cache, honour allowlist/mirror policy | B | have | 2 | T4 | `uses: https://code.forgejo.org/actions/setup-go@v5` runs unmodified; the resolved SHA is in the run header and the log; a non-allowlisted repo is a named refusal | S1 D-5 |
| PAR-071 | JavaScript actions (`runs.using: node20` / `node24`): `INPUT_*`, `GITHUB_OUTPUT`/`GITHUB_STATE`, pre/post hooks with pre-if/post-if | B | have | 2 | T4 | A published node action passes its own test suite; a post hook runs after a failed main in the documented order | S1 |
| PAR-072 | Docker/Dockerfile actions: no `action.yml`, `runs.args`, `with.args`/`with.entrypoint`; `docker://` args parity | B | have | 2 | T4 | A Dockerfile-only action builds and runs; `args` overrides the image CMD; `docker://` verify-and-close lands as a matrix row with its existing tests | S12 |
| PAR-073 | `actions/checkout` full input set: ref, repository, path, fetch-depth, fetch-tags, submodules, token, persist-credentials, clean, sparse-checkout | B | have | 1 | T2 | Weave's `fetch-tags: true` dispatch checkout runs unmodified; every documented input is honoured or refused by name | S10 |
| PAR-074 | Composite parity: `${{ inputs.* }}`, `github.action_path`, nested remote `uses:`, per-step env layering, output threading | B | have | 2 | T4 | `sigstore/cosign-installer` runs unmodified and its composite reads `action_path` | — |
| PAR-075 | Reusable workflows, local: `on.workflow_call` (typed inputs, secrets, outputs) + `jobs.<id>.uses`/`with`/`secrets:` | B | have | 2 | T3 | The documented chain runs, each callee is visible on the run page, its output is consumable, permissions are downgrade-only | S8 |
| PAR-076 | Reusable workflows, cross-repo/remote: SHA-pinned resolution, explicit secrets only, 10-level cap | G | have | 3 | T3 | `secrets: inherit` to a cross-repo callee is refused by name; a pinned remote callee runs; nesting 11 is refused | S8 D-6 |
| PAR-077 | `container` keys applied, not inert: credentials, ports, init, tty, entrypoint, command | B | have | 3 | T2 | The `sleep infinity` entrypoint example runs as PID 1; `ports` is an ephemeral loopback publish; a refused key is named; a private image pulls under the runner's identity (D-23) | S12 D-23 |
| PAR-078 | `services.<id>` parity: Forgejo's `cmd` spelling, credentials, `--health-*` options and readiness gating | B | have | 2 | T2 | The documented postgres service with `--health-cmd pg_isready` gates the job; a 60 s miss fails the job naming the service | — |
| PAR-079 | `steps[*].shell` as an arbitrary command with `{0}`; `node`/`python` aliases; bash→sh fallback | B | have | 2 | T2 | `shell: dash -e {0}` runs; `python` maps to `python {0}`; a missing bash falls back with a log line | — |
| PAR-080 | Step `background`/`wait`/`wait-all`/`cancel`/`parallel` (new keys) — or an approved non-goal (D-7) | G | have | 3 | T2 | Either each behaves as documented (process group killed on exit) or the matrix row is an approved non-goal | — |
| PAR-081 | Docker-in-job: daemonless builders documented as the parity path; optional runner-level daemon/privilege grant | B | have | 2 | T2 | A buildah corpus job builds and pushes; a job asking for the socket on a non-granting runner is refused by name | S2 D-8 |
| PAR-082 | Built-in action surface verify-and-close (`docker://` never receives secrets or the job token; digest policy) | F | have | 1V | T7 | The existing tests are named in the matrix row and re-run in the parity job | S13 |

### 4.6 Command files, artifacts and cache

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-090 | `$GITHUB_ENV`/`$FORGEJO_ENV`: later steps only, heredoc, caps, protected-name refusal | B | have | 1 | T2 | `echo "TAG=$TAG" >> "$GITHUB_ENV"` is visible to later steps and not before; `LD_PRELOAD` is refused by name | S4 |
| PAR-091 | `$GITHUB_PATH`/`$FORGEJO_PATH` | B | have | 1 | T2 | A directory appended in one step is on `PATH` in the next, prepended, and never replaces it | S4 |
| PAR-092 | `$GITHUB_STEP_SUMMARY` (1 MiB/step, sanitized markdown) and `$GITHUB_STATE` for pre/post | B | have | 2 | T2 | The documented Mermaid example renders safely on the job page; state written by a pre hook reaches the post hook as `STATE_<name>` after a failed main | S4 S15 |
| PAR-093 | Workflow commands: `::error`/`::warning`/`::notice` with file/line/col/title, `::group::`/`::endgroup::`, `::add-mask::`, `::stop-commands::`, `::set-output` if honoured | B | have | 2 | T2 | `::error file=a.go,line=3::msg` is a line-anchored annotation on job and commit; a masked value never prints later | S15 |
| PAR-094 | `$GITHUB_OUTPUT` heredoc, caps and collision rules (last wins; malformed line is a named failure) | B | have | 2 | T2 | The documented multi-line heredoc publishes an output readable via `steps.<id>.outputs`; a malformed line names the line | — |
| PAR-095 | Cache service: key/restore-keys, branch and default-branch scope, `cache-hit`, eviction, size budget | B | have | 2 | T4 | Run 2 restores run 1's cache; a miss is not a failure; lookup order is a test table; a fork run cannot write a base-readable entry | S6 D-10 |
| PAR-096 | `setup-*` cache integration (`cache: npm` / `pip` / `go`) on the cache service | G | have | 2 | T4 | `setup-node` with `cache: npm` restores and saves with no separate cache step | S6 |
| PAR-097 | Artifact v4 inputs: overwrite, compression-level, include-hidden-files, pattern, merge-multiple, artifact-ids, 255-char names, no-name download-all | B | have | 2 | T5 | `pattern: dist-*` with `merge-multiple: true` yields one merged tree; a no-name download fetches all | S7 |
| PAR-098 | Cross-run artifacts (download from another run id, same repository only) | G | have | 3 | T5 | A `workflow_run` job downloads the upstream artifact; a cross-repository request is refused by name | S7 D-3 |
| PAR-099 | Artifact list/delete API, page list and a sweep report | G | have | 3 | T5 | Bytes freed are reported per family; a delete removes blob and manifest | S7 |

### 4.7 Secrets, tokens and OIDC

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-110 | Automatic token aliases: `FORGEJO_TOKEN`/`GITHUB_TOKEN` in env, `secrets.GITHUB_TOKEN`, `forgejo.token` | B | have | 1 | T3 | Weave's `FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}` checkout works unmodified; the value never appears in argv or the plan payload | S10 |
| PAR-111 | Server/API addresses: `GITHUB_SERVER_URL`, `GITHUB_API_URL`, `FORGEJO_SERVER_URL` env and `github.server_url`/`api_url`/`graphql_url`/`retention_days` | — | have | 1 | T3 | Scriobh's `$GITHUB_SERVER_URL/api/v1/...` curl succeeds; `${{ github.server_url }}` equals the env var | — |
| PAR-112 | Permission scopes: every documented scope drives a real capability or is refused by name | B | have | 2 | T3 | `issues: write` lets a job comment; an unimplemented scope is a parse error naming it (never inert) | S10 D-12 |
| PAR-113 | Job token as a scoped platform API credential (comment, label, release, package) | G | have | 2 | T3 | A workflow comments on its own PR over HTTP with `$GITHUB_TOKEN`; the same token is refused another repository | S10 |
| PAR-114 | Token default recorded: scoped + `contents: read`; Forgejo's implicit-write token is an approved divergence | F | have | 1 | T3 | The matrix row states both behaviours and the approval; a workflow relying on implicit write fails with a 403 that names the missing scope | S10 D-12 |
| PAR-115 | Organization and user secret scopes with documented precedence and repository policy | B | have | 2 | T3 | An org secret resolves in every org repo; a repo secret of the same name wins; an unselected repo gets nothing | D-22 |
| PAR-116 | Secret availability in `env`/`with`/`if`; fork runs empty; masking completeness (`::add-mask::`, annotations, summaries) | B | have | 2 | T3 | A fork run's `${{ secrets.X }}` is empty and its env var unset; a masked value never appears in a later log line, annotation or summary | S13 S15 |
| PAR-117 | Non-secret `env` variables (repository/organization/environment) alongside environment secrets | G | have | 2 | T3 | `${{ vars.X }}` resolves differently per environment; `env` and `vars` never collide | — |
| PAR-118 | Fork token policy: per-repo send-write-tokens setting, `pull_request_target` trust split, first-run approval | G | have | 3 | T3 | Flipping the setting makes a fork run write-capable, the change is audited, and the run page says so | S13 D-13 |
| PAR-119 | OIDC enable-key parity: `enable-openid-connect: true` beside `permissions: id-token: write` | F | have | 2 | T1 | A Forgejo workflow with both spellings parses and mints a token with zero edits | S9 |
| PAR-120 | OIDC claim and exchange parity: `iss`, PR `sub`, environment `sub`, `aud` default, `ref_protected`, `ref_type`, `job_workflow_ref`, `runner_environment`, `?audience=`, TTL, error shapes | B | have | 2 | T3 | A provider trust policy written for either platform validates a Threads token; the `environment` claim appears only when a job names one | S9 |

### 4.8 Environments, deployments and approvals

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-130 | `environment`/`environment.url` expression substitution (or a named parse error, never a literal bucket key) | B | have | 2 | T1 | `environment: ${{ matrix.env }}` resolves; an unresolvable reference is the file's named error | — |
| PAR-131 | Protection parity: wait timer, ≤6 required reviewers, prevent-self-review, deployment branch/tag policy, team reviewers | B | have | 2 | T3 | All five are enforced and shown on the deployment; a team member's approval counts | — |
| PAR-132 | Deployment records: per-environment history, `deployment_status`, environment URL on the repo page | G | have | 3 | T3+T6 | Each deployment has a state history and a working URL link | — |
| PAR-133 | `deployments` permission and deployment API (`POST /deployments`, statuses) | G | have | 3 | T3 | A workflow-created deployment appears in the UI with its statuses; a token without `deployments: write` is refused | S10 |
| PAR-134 | Environment UI parity: deployments, secrets, variables and protection-rule tabs | G | have | 3 | T6 | All four surfaces exist and write; mobile and desktop checks pass | — |
| PAR-135 | Pending-deployment UX: run-list badge, reviewer notification, approval audit entry | G | have | 2 | T6 | A reviewer is notified, the badge names the environment, the audit names who approved what | — |
| PAR-136 | Deployment queuing: a second deployment to a busy environment serializes instead of failing | G | have | 3 | T3 | Two concurrent deploys to one environment run in trigger order, the second visibly queued | — |
| PAR-137 | Environment verify-and-close: secret precedence, fork exclusion, and "Threads exceeds Forgejo (no environment syntax)" | F | have | 1V | T7 | Existing tests are named in the matrix rows; the row states the exceedance so nobody builds Forgejo parity work here | — |

### 4.9 Concurrency, scheduling, retention and GC

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-150 | Implicit supersede: a new push / PR `synchronize` cancels the superseded in-flight run of that workflow+ref when no `concurrency` block exists | F | have | 1 | T3 | Two rapid pushes leave one in-flight run; the superseded run reads `cancelled` with a reason naming the superseding run | — |
| PAR-151 | `cancel-in-progress` as an expression; `group` reading `vars`/`inputs`/`matrix`/`github` | B | have | 2 | T1 | `cancel-in-progress: ${{ github.ref_name != 'main' }}` behaves as documented; an unresolvable group still fails closed | — |
| PAR-152 | Group semantics: cancel the *pending* run by default, `cancel-in-progress: false` sequences, `queue: single` / `max` | G | have | 2 | T3 | Three rapid pushes to one group leave one running and one pending; sequencing runs in trigger order to completion | — |
| PAR-153 | Per-repository and per-organization concurrency caps | G | have | 3 | T3 | A cap of 2 queues a third run with exactly one named reason | — |
| PAR-154 | Schedule operations: next-fire in UI/API, enable/disable without a commit, operator-visible watermark | G | have | 4 | T6 | The page shows the next fire per cron and a disable takes effect immediately | — |
| PAR-155 | Run prioritization | F | have | 4 | T3 | A prioritized run's jobs are claimed before older queued work; the ordering is documented | — |
| PAR-156 | Queue observability: exactly one named reason per queued job (no runner, labels unmatched, group held, approval, capacity) | G | have | 3 | T6 | Every queued job shows exactly one named reason, and the reason changes when the cause clears | — |
| PAR-157 | Per-repository CI pause/resume with in-flight jobs finishing | G | have | 4 | T3 | A paused repo claims nothing and resumes cleanly | — |
| PAR-158 | Retention parity: configurable ceiling (≥90 days), keep-forever, separate run/log and artifact windows with artifact expiry defaults | B | have | 2 | T5 | A 90-day setting is honoured and reported; an artifact outlives its run only when its own window allows; keep-forever works | — |
| PAR-159 | GC parity: expired artifacts, unreferenced blobs, cache eviction, reclaimed bytes per family | G | have | 2 | T5 | GC reclaims and reports the bytes it freed per family; a dry run reports the same numbers without deleting | — |

### 4.10 Runners and infrastructure

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-170 | Hosted-label semantics: `ubuntu-latest` (and friends) via a documented operator mapping; `runs-on: {group:, labels:}` | B | have | 1 | T3 | A `ubuntu-latest` corpus job runs with zero edits on an instance whose operator mapped the label; an unmapped label queues with a named reason | S11 |
| PAR-171 | Label→backend+image mapping (`name:docker://image`, `name:lxc://template`, `name:host`) | F | have | 2 | T3 | A runner declaring `ubuntu-latest:docker://node:lts` runs that job in that image | S11 S17 |
| PAR-172 | Registration scopes: instance, organization, user, repository | F | have | 3 | T3 | An org-scoped runner serves only that org and appears in its repositories' lists | — |
| PAR-173 | Ephemeral runners: one job, then the record and credential are gone | F | have | 3 | T3 | The record is gone after one job and its credential is rejected | — |
| PAR-174 | Managed runner pool (ephemeral, container-backed, autoscaled) so a fresh instance runs queued jobs unattended | G | non-goal | 3 | T3 | A fresh spindle boots and a queued job runs unattended inside the documented bounds | S11 D-14 |
| PAR-175 | Runner groups with org/repo visibility and inherited labels | G | have | 3 | T3 | A group's runner is claimable only by allowed repositories, with inherited labels matched | — |
| PAR-176 | Larger-runner sizing (per-job cpu/memory) or a named queue reason | G | have | 3 | T3 | A job asking 4cpu/16GB lands on a matching runner or is queued with that exact reason | — |
| PAR-177 | Stock toolchain images (node/python/go/rust/docker CLI + buildx) published and documented | G | have | 3 | T3+T8 | The JS matrix, Go build and docker build corpus rows run on stock images | S2 D-26 |
| PAR-178 | Isolation parity: concurrent jobs share no namespace, and the capacity model is documented | G | have | 3 | T3 | Two concurrent jobs cannot see each other's processes or volumes; the doc states which setting buys which guarantee | S11 D-14 |
| PAR-179 | Operator policy knobs: `container.privileged`, `network`, `valid_volumes`, `docker_host`, `runner.timeout` — each with a safe default | F | have | 2 | T3 | Each knob exists with a documented default; `privileged` is refused unless the runner opted in | S2 S12 D-8 |
| PAR-180 | Offline registration (config file, no browser) and multi-scope from one process | F | have | 4 | T3 | A runner registers from a config file and serves two scopes from one process | — |
| PAR-181 | Fleet operations: list/drain/revoke, zero-downtime rotation | G | have | 4 | T3 | Draining finishes in-flight jobs and claims nothing new; revocation is a real 401 | — |
| PAR-182 | **Non-goal:** LXC and unisolated host backends; hosted Windows/macOS runners (D-15) | B | non-goal | 4 | T8 | The matrix rows carry the non-goal, its reason and the named signatory; a workflow asking for one is a named refusal | S17 |

### 4.11 Observability, API, CLI and UI

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-190 | Log fidelity: `##[group]` folding, per-step timing, whole-run log download (txt/zip), streaming that survives session expiry | B | have | 2 | T2+T6 | Grouped logs collapse; a zip download contains every job's log; an expired session reconnects instead of dying | S15 |
| PAR-191 | Run graph UI: job DAG with live status, matrix cells grouped under their job, per-step drill-down | G | have | 2 | T6 | A matrix run renders one row per cell with its check and duration; the DAG shows blocked dependents | — |
| PAR-192 | Checks parity: per-check annotations, re-run from the PR panel, per-branch required-check config, check-run API accepting annotations; check-context naming decision recorded | B | have | 3 | T6 | An external CI posts annotated checks that gate the merge; the naming decision names the contexts required checks use | — |
| PAR-193 | Workflow/runs API and badges (`…/badge.svg`) | G | have | 3 | T6 | A README badge reflects the branch's latest conclusion; the API lists/filters runs | — |
| PAR-194 | Run-list filtering parity: branch, event, status, workflow, actor, with pagination | G | have | 3 | T6 | The documented filter set narrows the list correctly, and the page stays usable at 390 px | — |
| PAR-195 | Cancel/rerun granularity: whole run, single job, force-cancel, and a "cancelled by X" record | G | have | 3 | T3+T6 | Cancelling one job leaves its siblings running; the record names the actor | — |
| PAR-196 | Authoring affordances: validate-on-save with key-level errors, schema-assisted editor | G | have | 4 | T6 | An invalid key is reported in the editor, not only after a push; the message names key and file | — |
| PAR-197 | Notifications parity: failure notifications (UI/webhook/email) and per-user workflow subscriptions | B | have | 4 | T3+T6 | A failed watched workflow notifies its subscribers once, with the run link | D-27 |
| PAR-198 | CLI parity: `threads run --ref` with inputs, `threads watch`, run/job listing | G | have | 4 | T6 | The CLI dispatches with inputs and follows the run to completion; exit codes match the API | — |
| PAR-199 | `spindle threads validate` offline validation with the same errors the server reports | F | have | 2 | T1+T6 | A file validates offline with byte-identical error text to the server for a table of bad files | — |

### 4.12 Conformance and traceability

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-200 | Conformance corpus + harness (`internal/e2e/parity_test.go`, `testdata/parity/forgejo`) driven through discovery → run → checks → artifacts | B | have | 1 | T7 | `make parity` runs the corpus; a deliberately broken row makes it fail; a Forgejo corpus entry runs unmodified after conversion | — |
| PAR-201 | `make parity` gate: no `missing` row, no `partial` without a wave or decision, no `have` row without an existing, passing named test | B | have | 1 | T7 | Removing a named test from the tree fails the gate with the row id; adding a row without a test fails | — |
| PAR-202 | Living matrix (`docs/features/parity/parity.json` + generated `matrix.md`) with the docs-revision stamp; docs drift corrected | B | have | 1 | T8 | Every `PAR-` id appears once; the stamp names both docs revisions; `docs/threads.md` §11 matches §5.5/§5.6 and `15_threads.md` §2.6/§2.7 | — |

---

## 5. Wave plan (Forgejo first)

The operator runs Forgejo most, so Wave 1 must make **real Forgejo repositories work** — not "most of
Forgejo", the four files in the corpus. Waves are cumulative; a later wave never re-opens an earlier
decision.

### Wave 1 — make Weave and Scriobh run (P0 divergences + corpus blockers)

Twenty-four entries, in four batches that can land in parallel:

**1a — Syntax core (T1).** `PAR-010` (tags fire), `PAR-011` (tags-only stops matching branches),
`PAR-030` (substitution in the sites the corpus uses: `run`, `with`, `env`, `name`, `if`),
`PAR-031` (`secrets.*`), `PAR-032` (the `github`/`forgejo` fields the corpus reads: `event_name`, `sha`,
`ref`, `ref_name`, `repository`, `workflow`, `job`, `run_id`, `event.inputs`), `PAR-037` (`startsWith`,
`contains`, `format`), `PAR-050` (`needs: <string>`), `PAR-051` (`matrix.include`/`exclude`).

**1b — Environment and token aliases (T3).** `PAR-110` (`GITHUB_TOKEN`/`FORGEJO_TOKEN`/
`secrets.GITHUB_TOKEN`), `PAR-111` (`GITHUB_SERVER_URL`/`GITHUB_API_URL`), `PAR-114` (record the
permissions-default divergence), `PAR-170` (`ubuntu-latest` mapping so `publish-github.yml` is schedulable).

**1c — Execution surfaces (T2).** `PAR-073` (checkout `fetch-tags`, `ref`, `path`), `PAR-090`
(`$GITHUB_ENV`), `PAR-091` (`$GITHUB_PATH`).

**1d — Semantics and proof (T3/T1/T7/T8).** `PAR-150` (implicit supersede — without it, two pushes race the
same deploy), `PAR-056` (reconcile job-level `continue-on-error`, record the divergence), `PAR-002`
(importer), `PAR-001`/`PAR-082`/`PAR-137` (verify-and-close rows), `PAR-200`/`PAR-201`/`PAR-202` (harness,
gate, matrix).

Wave 1 closes when: `spindle threads import --from forgejo` converts the four files, each converted file
parses, and `make parity` shows the Forgejo corpus green with every remaining difference named as an
approval-gated non-goal.

*Why these and not "all P0s":* the two other source-card P0s are already correct here or are the same
item — `${{ }}` in structural fields is part of `PAR-030`; the missing token is `PAR-110`. Conversely,
`PAR-012` (paths), `PAR-014` (full PR types) and `PAR-035` (`vars`) are P1 but have **no** corpus
occurrence, so they are Wave 2: shipping them in Wave 1 would delay the only goal that matters.

### Wave 2 — Forgejo completeness

Everything Forgejo documents that the corpus does not exercise, plus the GitHub keys that behaviourally
overlap: paths filters and the glob dialect (`PAR-012`, `PAR-013`), full PR types (`PAR-014`),
`pull_request_target` (`PAR-015`), `release` (`PAR-017`), issues/comment triggers (`PAR-020`), cron parity
(`PAR-022`), dispatch input types (`PAR-023`), the rest of the substitution sites (`PAR-030`), event payload
(`PAR-033`), runner context (`PAR-034`), `vars` (`PAR-035`), `env` context (`PAR-036`), remaining functions
(`PAR-037`), operators (`PAR-038`), context availability (`PAR-040`), `hashFiles` (`PAR-041`),
`max-parallel` (`PAR-052`), `runs-on` substitution (`PAR-053`), `defaults` (`PAR-054`), timeout parity
(`PAR-055`), job `if:` secrets (`PAR-058`), rerun-failed (`PAR-059`), remote actions
(`PAR-070`), JS actions (`PAR-071`), Dockerfile actions (`PAR-072`), composite parity (`PAR-074`), local
reusable workflows (`PAR-075`), services (`PAR-078`), shell parity (`PAR-079`), docker-in-job (`PAR-081`),
step summary/state (`PAR-092`), workflow commands (`PAR-093`), output heredoc (`PAR-094`), cache
(`PAR-095`, `PAR-096`), artifact inputs (`PAR-097`), permission scopes (`PAR-112`), job token as API
credential (`PAR-113`), org/user secrets (`PAR-115`), secret availability/masking (`PAR-116`), `env`
variables (`PAR-117`), OIDC enable-key (`PAR-119`), OIDC claims (`PAR-120`), environment substitution
(`PAR-130`), protection parity (`PAR-131`), pending-deployment UX (`PAR-135`), `cancel-in-progress`
expression (`PAR-151`), group pending-cancel (`PAR-152`), retention (`PAR-158`), GC (`PAR-159`), label
mapping (`PAR-171`), operator knobs (`PAR-179`), log fidelity (`PAR-190`), run graph (`PAR-191`),
`spindle threads validate` (`PAR-199`).

### Wave 3 — the GitHub-only surface

Triggers that only exist on GitHub: `merge_group` (`PAR-016`), `repository_dispatch` (`PAR-018`),
`workflow_run` (`PAR-019`), the repository/check/deploy trigger family (`PAR-021`), merge-ref semantics
(`PAR-024`), `job`/`jobs`/`strategy` contexts (`PAR-039`), cross-repo reusable workflows (`PAR-076`),
container keys applied (`PAR-077`), step background/parallel (`PAR-080`), cross-run artifacts (`PAR-098`),
artifact API (`PAR-099`), fork token policy (`PAR-118`), deployments (`PAR-132`, `PAR-133`, `PAR-136`),
environment UI (`PAR-134`), caps (`PAR-153`), queue observability (`PAR-156`), runner scopes/ephemeral/
pool/groups/sizing/isolation/stock images (`PAR-172`…`PAR-178`), checks parity (`PAR-192`), API/badges
(`PAR-193`), list filtering (`PAR-194`), cancel granularity (`PAR-195`).

### Wave 4 — ecosystem and ergonomics

`run-name` (`PAR-057`), schedule operations (`PAR-154`), prioritization (`PAR-155`), CI pause (`PAR-157`),
offline registration (`PAR-180`), fleet ops (`PAR-181`), the non-goal rows (`PAR-182`), authoring
affordances (`PAR-196`), notifications (`PAR-197`), CLI (`PAR-198`).

### Dependency edges

```
PAR-002 importer ──► PAR-200 corpus (converted goldens)
PAR-030/031/032/037 (syntax core) ──► every Wave-2 trigger, matrix, action and environment item
PAR-037 hashFiles ──► PAR-041 (runner walk implemented once)
PAR-051 matrix include ──► PAR-052 max-parallel, PAR-053 runs-on substitution
PAR-070 remote actions ──► PAR-071 JS runtime, PAR-074 nested remote composite, PAR-076 cross-repo reusable, PAR-096 setup-* cache
PAR-095 cache ──► PAR-096 setup-* cache
PAR-097 artifact inputs ──► PAR-098 cross-run artifacts ──► PAR-019 workflow_run
PAR-112 permission scopes ──► PAR-113 job token API ──► PAR-133 deployments API
PAR-131 environment protection ──► PAR-136 deployment queuing, PAR-132 deployment records
PAR-171 label mapping ──► PAR-174 managed pool ──► PAR-178 isolation parity
PAR-173 ephemeral runners ──► PAR-174 managed pool
PAR-192 check annotations ──► PAR-093 workflow commands (the annotation sink exists first)
PAR-201 gate ──► every row that turns green (a row without a test cannot close)
```

---

## 6. Parallel work tracks

The split exists so several agents can work **now** without touching the same file. Its unit is the
**file set**, not the package: two tracks may share a Go package as long as they own different files, and
`go test ./internal/threads/...` still compiles both. The prompt's suggested split survived contact with the
code with two adjustments, both forced by evidence: **artifacts split server-side (T5) from runner-side (T2)**
because the runner's upload path already lives in `internal/threads/runner/`, and **the importer joins T1**
because it is a syntax-level component (it reads a foreign grammar and emits ours), not a docs chore.

### 6.1 The ownership contract

1. **One owner per file set.** A PR touching a file another track owns needs that owner's review. New files
   inside an owned directory belong to that directory's owner.
2. **A shared file has exactly one owner.** `Makefile` → T7 (parity targets only); `docs/FEATURES.md` and the
   docs tree → T8; `AGENTS.md` → nobody in this plan; `internal/threads/containerpolicy.go` → T2 even though
   it lives in package `threads`, because its two enforcement points (server at creation, runner at plan
   time) must change together.
3. **Parser first, always.** A parser change (T1) lands before any runner/service item that consumes the new
   shape, and the consuming PR names the parser test it depends on. This is the only hard merge order.
4. **One item, one owner.** Where §4's Track cell names two tracks, the **first** is the lead and carries the
   card item; the second reviews. The card therefore has exactly one checklist line per `PAR-` id.
5. **The matrix row moves with the code.** Every PR updates `parity.json` and the card item (AGENTS law 12).

### 6.2 The tracks

| Track | Owns (file set) | Wave 1 | Wave 2 | Wave 3 | Wave 4 | Conformance proof |
|---|---|---|---|---|---|---|
| **T1 — Syntax, expressions, triggers & importer** | `internal/threads/workflow/**` · `internal/threads/expr.go` · `internal/threads/group.go` · new `internal/threads/importer/**` · new `cmd/spindle/threads_import.go` | PAR-002, PAR-010, PAR-011, PAR-030, PAR-031, PAR-032, PAR-037, PAR-050, PAR-051, PAR-056 | PAR-004, PAR-012, PAR-013, PAR-022, PAR-023, PAR-033, PAR-036, PAR-038, PAR-040, PAR-053, PAR-054, PAR-058, PAR-119, PAR-130, PAR-151, PAR-199 | PAR-039 | — | Parser/expression table tests, importer goldens, `TestDocExamplesParse` over every `yaml` block |
| **T2 — Runner & execution** | `internal/threads/runner/**` · `internal/threads/containerpolicy.go` | PAR-073, PAR-090, PAR-091 | PAR-034, PAR-041, PAR-078, PAR-079, PAR-081, PAR-092, PAR-093, PAR-094, PAR-190 | PAR-077, PAR-080 | — | Runner unit tests (fake docker + real `sh`), command-file tables, corpus jobs |
| **T3 — Server services & orchestration** | `internal/threads/{ci,dispatch,schedule,discover,concurrency,queue*,gc,needs,jobif,attempts,model,spec,service,threads,environments,approval,oidc,tokens,secrets,runners,keys}.go` · new `internal/threads/trigger_*.go` | PAR-001, PAR-110, PAR-111, PAR-114, PAR-150, PAR-170 | PAR-014, PAR-015, PAR-017, PAR-020, PAR-035, PAR-052, PAR-055, PAR-059, PAR-075, PAR-112, PAR-113, PAR-115, PAR-116, PAR-117, PAR-120, PAR-131, PAR-152, PAR-171, PAR-179 | PAR-016, PAR-018, PAR-019, PAR-021, PAR-024, PAR-076, PAR-118, PAR-132, PAR-133, PAR-136, PAR-153, PAR-172, PAR-173, PAR-174, PAR-175, PAR-176, PAR-177, PAR-178, PAR-195 | PAR-155, PAR-157, PAR-180, PAR-181, PAR-197 | Service tests with CAS fakes, admission tables, `internal/e2e/threads_triggers_test.go`, OIDC claim table |
| **T4 — Actions & cache** | new `internal/actions/**` · new `internal/actionscache/**` · `internal/threads/actions.go` | — | PAR-070, PAR-071, PAR-072, PAR-074, PAR-095, PAR-096 | — | — | Resolution tables against a fake source, a node-action fixture, cache lookup tables |
| **T5 — Artifacts, outputs & retention** | `internal/threads/{artifacts,http_artifacts,outputs}.go` · new `internal/threads/artifactgc.go` | — | PAR-097, PAR-158, PAR-159 | PAR-098, PAR-099 | — | Artifact input tables, retention/GC with a fake store, reclaimed-bytes assertions |
| **T6 — API, pages, SDK & CLI** | `internal/threads/http*.go` (not `http_artifacts.go`) · `internal/webui/{threads*,admin_runners,env}.go` · threads and environments templates · `web/src/islands/threadlog.js` · threads SDK · `cmd/spindle/cli_misc.go` (threads group) | — | PAR-003, PAR-135, PAR-191 | PAR-134, PAR-156, PAR-192, PAR-193, PAR-194 | PAR-057, PAR-154, PAR-196, PAR-198 | `httptest` handler tables, render tests, a real-browser drive at desktop and ~390 px |
| **T7 — Conformance & e2e** | `internal/e2e/{parity,threads}_*_test.go` · `internal/e2e/testdata/parity/**` · new `internal/devtools/paritygate/**` · `Makefile` (parity target) | PAR-082, PAR-137, PAR-200, PAR-201 | — | — | — | Itself: `make parity`, plus a deliberately broken row that must fail it |
| **T8 — Documentation & migration** | `docs/threads.md` · `docs/features/15_threads.md` · `docs/features/21_parity_master.md` · `docs/features/parity/**` · `docs/FEATURES.md` · `docs/WORKFLOWS.md` (after the `wf-docs` merge) | PAR-202 | — | — | PAR-182 | Doc-example parse tests, the gate's stale-matrix check, link check |

### 6.3 Merge order and what can start immediately

1. **Hour one:** T7's skeleton (corpus directories + `paritygate` + `make parity` + a `parity.json` holding
   only the `have` rows) and T8's render of `matrix.md`. Everything after this adds rows instead of creating
   the harness.
2. **Next:** T1's Wave-1 batch, as **per-item PRs** rather than one branch — it unblocks every other track,
   and a single 20-file parser PR is unreviewable.
3. **In parallel:** T3's and T2's Wave-1 batches (disjoint files), and T4's new packages the same day (new
   directories, so no collision) even though its items are Wave 2 — a resolver with no consumers yet is the
   cheapest thing to build early.
4. **Waiting on a seam, not idle:** T5 starts in `artifactgc.go` (no dependency) and takes artifact input
   metadata once T4's `actions.go` input-validation seam exists; T6 starts on log download, run-list
   filtering and the validate-on-save plumbing from shapes that already exist, and waits for the shapes it
   renders (annotations, deployment records) rather than inventing them.
5. **T8 continuously.** The `docs/WORKFLOWS.md` promotion (the `wf-docs` branch) lands before the migration
   section is rewritten; until then that file is **not** edited from this plan.

**Can start immediately:** T1, T2, T3, T4, T7, T8, and the dependency-free parts of T5/T6.
**Blocked:** T2's `PAR-073` checkout inputs wait on T1's `PAR-030` (`with:` substitution) — T1's first PR;
T5's `PAR-097` input metadata waits on T4; T6's `PAR-192` annotations wait on T2's `PAR-093` command parser.
Nothing in Wave 1 is blocked on a wave.

| **T1 — Syntax, expressions, triggers & importer** | `internal/threads/workflow/**` · `internal/threads/expr.go` · `internal/threads/group.go` · new `internal/threads/importer/**` · new `cmd/spindle/threads_import.go` | PAR-002, PAR-010, PAR-011, PAR-030, PAR-031, PAR-032, PAR-037, PAR-050, PAR-051, PAR-056 | PAR-004, PAR-012, PAR-013, PAR-022, PAR-023, PAR-033, PAR-036, PAR-038, PAR-040, PAR-053, PAR-054, PAR-058, PAR-119, PAR-130, PAR-151, PAR-199 | PAR-039 | — | Parser/expression table tests, importer goldens, `TestDocExamplesParse` over every `yaml` block |
| **T2 — Runner & execution** | `internal/threads/runner/**` · `internal/threads/containerpolicy.go` | PAR-073, PAR-090, PAR-091 | PAR-034, PAR-041, PAR-078, PAR-079, PAR-081, PAR-092, PAR-093, PAR-094, PAR-190 | PAR-077, PAR-080 | — | Runner unit tests (fake docker + real `sh`), command-file tables, corpus jobs |
| **T3 — Server services & orchestration** | `internal/threads/{ci,dispatch,schedule,discover,concurrency,queue*,gc,needs,jobif,attempts,model,spec,service,threads,environments,approval,oidc,tokens,secrets,runners,keys}.go` · new `internal/threads/trigger_*.go` | PAR-001, PAR-110, PAR-111, PAR-114, PAR-150, PAR-170 | PAR-014, PAR-015, PAR-017, PAR-020, PAR-035, PAR-052, PAR-055, PAR-059, PAR-075, PAR-112, PAR-113, PAR-115, PAR-116, PAR-117, PAR-120, PAR-131, PAR-152, PAR-171, PAR-179 | PAR-016, PAR-018, PAR-019, PAR-021, PAR-024, PAR-076, PAR-118, PAR-132, PAR-133, PAR-136, PAR-153, PAR-172, PAR-173, PAR-174, PAR-175, PAR-176, PAR-177, PAR-178, PAR-195 | PAR-155, PAR-157, PAR-180, PAR-181, PAR-197 | Service tests with CAS fakes, admission tables, `internal/e2e/threads_triggers_test.go`, OIDC claim table |
| **T4 — Actions & cache** | new `internal/actions/**` · new `internal/actionscache/**` · `internal/threads/actions.go` | — | PAR-070, PAR-071, PAR-072, PAR-074, PAR-095, PAR-096 | — | — | Resolution tables against a fake source, a node-action fixture, cache lookup tables |
| **T5 — Artifacts, outputs & retention** | `internal/threads/{artifacts,http_artifacts,outputs}.go` · new `internal/threads/artifactgc.go` | — | PAR-097, PAR-158, PAR-159 | PAR-098, PAR-099 | — | Artifact input tables, retention/GC with a fake store, reclaimed-bytes assertions |
| **T6 — API, pages, SDK & CLI** | `internal/threads/http*.go` (not `http_artifacts.go`) · `internal/webui/{threads*,admin_runners,env}.go` · threads and environments templates · `web/src/islands/threadlog.js` · threads SDK · `cmd/spindle/cli_misc.go` (threads group) | — | PAR-003, PAR-135, PAR-191 | PAR-134, PAR-156, PAR-192, PAR-193, PAR-194 | PAR-057, PAR-154, PAR-196, PAR-198 | `httptest` handler tables, render tests, a real-browser drive at desktop and ~390 px |
| **T7 — Conformance & e2e** | `internal/e2e/{parity,threads}_*_test.go` · `internal/e2e/testdata/parity/**` · new `internal/devtools/paritygate/**` · `Makefile` (parity target) | PAR-082, PAR-137, PAR-200, PAR-201 | — | — | — | Itself: `make parity`, plus a deliberately broken row that must fail it |
| **T8 — Documentation & migration** | `docs/threads.md` · `docs/features/15_threads.md` · `docs/features/21_parity_master.md` · `docs/features/parity/**` · `docs/FEATURES.md` · `docs/WORKFLOWS.md` (after the `wf-docs` merge) | PAR-202 | — | — | PAR-182 | Doc-example parse tests, the gate's stale-matrix check, link check |

## 7. Security decisions

Every P0/P1 design decision, balanced explicitly: the parity behaviour we owe, the threat, the control that
keeps parity without the threat, the residual risk we accept, and a verdict. Where parity and security
conflict the default is the **secure compatible subset** — never a silent weakening — and a feature that
needs the weaker behaviour becomes an operator knob, off by default.

Verdict scale: **safe as specified** (build it as written) · **compatible subset** (build the secure subset
and record the divergence) · **decided** (the operator chose an option on 2026-11-26; the choice and its
one-line rationale are recorded in the verdict and indexed in §7.18). Every item that needed a signatory
has one; where an earlier draft said `needs operator decision`, the chosen option is recorded instead.

### S1 — Remote and marketplace actions (`uses: owner/repo[/path]@ref`, full URLs)

- **Parity behaviour.** Resolve any repository action from a configurable source (Forgejo's
  `DEFAULT_ACTIONS_URL` analogue), refs as tag/branch/SHA, fetch once per run, cache, and honour
  `pre`/`post` and the action's declared inputs and outputs.
- **Threat.** Arbitrary third-party code executes in the job container with the job's environment —
  secrets and the job token included. A mutable tag can be retargeted after review; a typo-squatted owner
  is indistinguishable at a glance; a compromised upstream runs until someone notices; a fork PR can name
  any action (it runs untrusted anyway, but the same action in a trusted run is the vector that matters).
- **Control that keeps parity.** Resolve every ref to a **commit SHA at first fetch**, record it in the run
  header and the log, and print it in the job log with the source URL. An instance **allowlist**
  (`threads.actions.allow`, modes `deny`/`allowlist`/`open`) with `require_sha` on by default outside the
  allowlist. Mirror support so an instance serves approved actions from its own object store. Revocation =
  remove from the allowlist + delete the cache entry. No implicit credential beyond what the job already
  has; a `docker://`-style action gets no token (unchanged).
- **Residual.** An allowlisted action's upstream compromise is trusted until the pinned SHA is bumped; the
  action still reads the job's secrets (inherent to the model, and stated in WORKFLOWS: *an action is code
  with your job's privileges*).
- **Verdict.** **compatible subset — decided 2026-11-26 (D-5):** ship `allowlist` mode with `require_sha`
  outside the allowlist and the mirror optional; resolution and revocation stay as controlled above, so
  the supply-chain control does not depend on an operator remembering to turn it on.

### S2 — Docker-in-job

- **Parity behaviour.** `docker build` / `docker push` / `docker compose` work in a job the way they do on
  a hosted runner. Forgejo's runners also give no socket, and instead expose runner-level `container.privileged`
  and capability grants.
- **Threat.** A host socket is root on the runner node. `--privileged`/`--cap-add` is a kernel-level escape.
  Both are one line in a fork PR's workflow.
- **Control that keeps parity.** No socket by default; **daemonless builders are the documented parity
  path** (buildah/podman/kaniko — already WORKFLOWS §9.4). An operator may grant a *runner* the socket
  (`--allow-host-docker`, exists); that runner is marked `unsafe` in `/admin/runners`, is excluded from
  fork runs by default, and its jobs carry the marker on the run page. `container.privileged` becomes a
  runner-declared capability a job must **ask for** (`container.options: --privileged`), refused unless the
  runner opted in, and such a runner never receives an untrusted run.
- **Residual.** A socket-granted runner's jobs are node-root-equivalent by construction — that is the
  operator's declaration, not a workflow's. Daemonless builds depend on host kernel features and fail
  visibly where they are unavailable.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-8):** daemonless builders are the parity path;
  a host socket or privilege grant is a **runner declaration** an operator makes once, never a key a
  workflow can ask for, so an anonymous PR can never widen its own isolation.

### S3 — `${{ }}` in `run`, `with` and `env`

- **Parity behaviour.** GitHub substitutes before the shell or action sees the text; `run: echo
  "${{ secrets.TOKEN }}" | docker login` is idiomatic and must work.
- **Threat.** Substitution turns a secret into **script text** and, worse, into `docker create` argv
  (`--env NAME=value` is readable via `docker inspect`) and into the job payload in the bucket. Masking
  cannot catch a derived value.
- **Control that keeps parity.** Substitute server-side at plan time. Where the shape allows, a secret in
  `env` is delivered **by name** (`EnvFrom`, the existing job-token pattern), so the value never enters the
  payload. A secret substituted into `run`/`with` is registered with the masker before execution and
  **refused** in `container.entrypoint`, `container.command`, `container.options` and `services.*`
  equivalents (argv is host-visible). Every substituted secret is masked in log frames, annotations and
  summaries.
- **Residual.** `echo ${{ secrets.X }}` prints unless masked; masking is exact-substring best effort, so a
  base64-wrapped value escapes. Documented in WORKFLOWS §7.1, unchanged from today.
- **Verdict.** **safe as specified** (with the env-indirection rule and the argv refusal).

### S4 — Command files (`$GITHUB_ENV`, `$GITHUB_PATH`, `$GITHUB_OUTPUT`, `$GITHUB_STATE`, `$GITHUB_STEP_SUMMARY`)

- **Parity behaviour.** Per-step files; `NAME=VALUE` and heredoc; later steps only; `GITHUB_PATH` prepends;
  outputs readable through `steps.<id>.outputs`.
- **Threat.** A step writing `LD_PRELOAD`, `BASH_ENV`, `NODE_OPTIONS` or a trojaned `PATH` entry for a later
  step; collision with a reserved name (`GITHUB_TOKEN`, the OIDC request token); a newline in a name
  breaking the format; unbounded size; `STATE` surviving into a post hook of a different action.
- **Control that keeps parity.** One parser for all five files: strict grammar, no `\n` in names, caps
  (64 KiB/value, 1 MiB/job, 50/step), last-wins documented. A **protected-name refusal list** for loader and
  platform variables (`LD_PRELOAD`, `LD_LIBRARY_PATH`, `NODE_OPTIONS`, `BASH_ENV`, `ENV`, `SHELL`,
  `GITHUB_TOKEN`, `FORGEJO_TOKEN`, `SPINDLE_JOB_TOKEN`, `ACTIONS_ID_TOKEN_*`, `GITHUB_ENV`, `GITHUB_PATH`,
  the file paths themselves) — refused by name, non-zero step. `PATH` is writable only through
  `GITHUB_PATH`, which prepends. Each step's file is fresh (structural isolation); values inherited from a
  secret are masked.
- **Residual.** The job container is one trust domain: a step that writes the environment for a later step
  already ran code in that container. The control prevents escalation **into the runner or host**, not into
  the job.
- **Verdict.** **safe as specified**.

### S5 — `pull_request_target`

- **Parity behaviour.** Run the base-branch workflow with base secrets and a write token; the head is
  fetchable so the workflow can comment, label, or build a preview.
- **Threat.** The canonical *pwn request*: the workflow checks out the head and runs it with secrets and a
  write token. Nothing in the YAML says "this is attacker code".
- **Control that keeps parity.** The trigger is **off per repository by default**
  (`threads.allow_pull_request_target`), the run always reads the **base tip's** file, and a
  `pull_request_target` job that names the PR head in `actions/checkout` (`ref:`/`repository:`) is a **named
  refusal at plan time** unless the repository also sets `pr_target_allow_head_checkout` (a second,
  separately audited switch). The first-time-fork approval gate applies. The run carries a `pr_target` badge
  and an audit entry.
- **Residual.** A `run:` step can `git fetch` the head itself and execute it; no plan-time inspection sees
  inside a script. That is why the default is off, and why enabling it is an explicit, audited operator act.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-2):** per-repository opt-in, head checkout
  behind a second switch, both audited. Off by default keeps the pwn-request off an instance that never
  asks for it; the second switch makes the deliberate case reviewable instead of impossible.

### S6 — Cache

- **Parity behaviour.** `actions/cache` + restore/save, exact/prefix/restore-key lookup, branch and
  default-branch scope, `cache-hit`, setup-* integration, eviction, size budget.
- **Threat.** Cache poisoning: an untrusted run writes an entry a trusted branch restores and *executes*
  (`~/go/pkg/mod`, `node_modules`, a compiled binary, `.git`); cross-fork leakage; key enumeration leaking
  repository and path names.
- **Control that keeps parity.** Entries are scoped by `(repository, trust class, ref)`. A PR/fork run
  **restores read-only** from its base branch and **writes only into its own PR-scoped namespace**, which no
  other ref reads and which is discarded at merge (never promoted). Keys are HMAC'd (no enumeration, no
  LIST). Per-repo size and entry caps; eviction is a sweeper with reported bytes. Cache bytes never carry a
  credential.
- **Residual.** A same-repository PR from someone with write can poison what main reads — that actor already
  has write, so there is no escalation. A cache entry used as a binary is still attacker-influenced code if
  the workflow executes it; documented.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-10):** PR-scoped writes, no PR-to-PR reads,
  discard at merge. A PR run can warm its own namespace and read its base, never feed another PR, so a
  merged cache entry always has a trusted author.

### S7 — Artifacts (same-run and cross-run)

- **Parity behaviour.** Upload/download within a run with v4 inputs; a `workflow_run` job downloads the
  upstream run's artifact; artifact list/delete API.
- **Threat.** An artifact is a file tree another job may *execute* (`download-artifact` then `./script`).
  A cross-run download from an untrusted run into a trusted `workflow_run` is code execution with secrets.
  Artifact contents are not masked, so a careless workflow leaks a secret into one.
- **Control that keeps parity.** Same-run artifacts inherit the run's trust class (structural today).
  Cross-run is **same repository only**, requires `actions: read`, and an artifact produced by an
  **untrusted** run is marked `untrusted`: a consuming run that carries secrets is created in
  `pending_approval` (the fork gate applies to the child), and the page names the parent run and its trust
  class. `merge-multiple` into a path is allowed; execution is the workflow's choice, and the approval is
  the human control. Artifact bytes are never masked — stated in the guide.
- **Residual.** An approved workflow_run that downloads and executes a fork artifact is a deliberate act by
  a maintainer; the approval names the run.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-3):** approval per run for an untrusted
  parent's artifact, never automatic and never for a secrets-bearing job. The human decides with the
  parent run named, so a fork artifact can cross into a trusted job only as a deliberate act.

### S8 — Reusable workflows

- **Parity behaviour.** `on.workflow_call` with typed inputs, secrets and outputs; `jobs.<id>.uses` with
  `with` and `secrets: inherit`; 10-level nesting; permissions downgrade-only.
- **Threat.** `secrets: inherit` to a **cross-repository** callee is secret exfiltration by design; a
  mutable ref changes under you; a callee widens permissions the caller never granted.
- **Control that keeps parity.** `secrets: inherit` is allowed only for a **same-repository** callee
  (a cross-repo callee gets explicitly listed secrets, and its declared inputs are validated). Remote
  callees are pinned to a commit SHA through the S1 resolver and pass the same allowlist. Effective
  permissions are the intersection of caller and callee (downgrade-only). Nesting is capped at 10 and the
  callee's file is fetched at run creation and embedded, never read at execution time.
- **Residual.** The callee still runs with the job token and the secrets explicitly passed; allowlisting an
  organization's workflows is a trust decision the operator makes once.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-6):** `secrets: inherit` never crosses a
  repository boundary, even inside one organization. Same-repository inheritance is a closed set, and a
  cross-repo callee must list its secrets explicitly — the one shape that is exfiltration by default.

### S9 — OIDC claims

- **Parity behaviour.** `iss`, `sub` (including `repo:<o>/<r>:environment:<name>` when a job names an
  environment), `aud` default, `ref_protected`, `ref_type`, `job_workflow_ref`, `runner_environment`;
  `?audience=`; short TTL.
- **Threat.** A subject that names the repository but not the ref lets a PR run impersonate `main` and
  assume a cloud role; a long TTL widens the replay window; a free-form audience lets a token be replayed at
  another service; a fork run minting a token at all.
- **Control that keeps parity.** Existing: trusted runs only, `id-token: write` only, 5-minute TTL, audience
  required, hash-only per-attempt request token, bucket-backed rate window. Add `ref_protected` and
  `ref_type` so a trust policy can require a protected ref, and the `environment` claim/subject so an
  environment-scoped role cannot be assumed from a branch. Every added claim is *more* specific; none
  removes a discriminator.
- **Residual.** A provider that trusts `repo:o/r:*` grants every ref — the provider's policy, and the guide
  says so with the recommended policy.
- **Verdict.** **safe as specified**.

### S10 — Token scopes and permissions

- **Parity behaviour.** `permissions:` per scope; `GITHUB_TOKEN`/`FORGEJO_TOKEN`; Forgejo ignores
  `permissions` and hands the job a write token.
- **Threat.** Forgejo's behaviour means a read-only workflow still holds write — exactly the credential a
  compromised action wants. GitHub's `write-all` default is the same problem with a nicer name.
- **Control that keeps parity.** `contents: read` default; each scope maps to one named capability and
  nothing more; an unknown scope is a **parse error naming it** (today's inert map is the bug); `write-all`
  does not include `id-token`; a fork run is pinned to `contents: read`; the token is bound to one attempt
  and one repository, expires with the job, and a push made with it starts no workflow.
- **Residual.** A workflow that needs write and gets `403` must be edited — a named failure with the missing
  scope, not a silent downgrade.
- **Verdict.** **compatible subset — decided 2026-11-26 (D-11, D-12):** only scopes with a real surface
  exist, every other scope is refused by name; a workflow with no `permissions:` gets `contents: read`,
  and Forgejo's implicit write stays refused. Each scope is one capability, so the token can never be
  wider than the key that asked for it.

### S11 — Runner isolation, capacity and the managed pool

- **Parity behaviour.** GitHub gives one job per fresh ephemeral VM; Threads runs N jobs per runner on one
  daemon (default 1), and can autoscale a pool.
- **Threat.** Capacity > 1 means co-tenant jobs share a daemon (and the daemon API); a managed pool means
  **unattended** execution of untrusted code at scale; an autoscaler bug becomes a fleet-wide incident.
- **Control that keeps parity.** Capacity 1 is the default and the documented untrusted path; a
  capacity-N runner is one trust domain and is excluded from fork runs unless the operator says otherwise.
  Pool workers are single-job, container-isolated, hold a `wrg_` credential revoked on exit, declare the
  labels they serve, and never claim a fork job unless the pool is explicitly opted in. Autoscaling has
  hard bounds (max workers, max queue age) and a drain path.
- **Residual.** The Docker daemon remains the isolation boundary; a kernel escape in one container is the
  node.
- **Verdict.** **safe as specified — decided 2026-11-26 (D-14):** no managed pool; capacity 1 is the
  untrusted path, and a capacity-N runner opts in explicitly. Unattended execution of untrusted code is
  not a default an instance can drift into.

### S12 — `container.options`, volumes and privilege

- **Parity behaviour.** Forgejo exposes runner-level `container.privileged`, `network`, `valid_volumes`,
  `docker_host`; GitHub exposes `container.options`, `container.credentials`, `container.volumes`.
- **Threat.** Each of these, workflow-settable, is node-root or a data-exfiltration path from a file an
  anonymous author wrote.
- **Control that keeps parity.** The allowlist stays (resource and in-container confinement only:
  `--cpus`, `--memory`, `--shm-size`, `--ulimit nofile`, `--init`, `--read-only`, `--user`, `--workdir`).
  `volumes` are refused outright. `credentials` are allowed — registry auth is not a host capability — and
  are delivered as a throwaway docker config scoped to the pull. `--network` is never workflow-settable (the
  runner creates the job network). `privileged` only through the S2 runner capability. Every refusal is a
  failed job naming the option and value.
- **Residual.** `--user`/`--workdir` values can confuse a script; they are not a boundary.
- **Verdict.** **safe as specified**.

### S13 — Fork-run trust

- **Parity behaviour.** Base-branch file at the base tip, no secrets, read-only token, first-run approval,
  the head still built.
- **Threat.** A fork author controls their branch; anything read from it is attacker code. The subtle case
  is a *simulated merge commit* (PAR-024): it contains attacker-controlled content by construction.
- **Control that keeps parity.** The base-tip workflow file is structural (the fork's file is never read);
  `secrets: {}` and `contents: read` are structural; the approval gate covers first-time contributors; fork
  runs need `--allow-fork-runs` **and** the repository allowed; `pull_request_target` is the only path to
  base secrets and is off by default. Merge-ref runs are computed **server-side by git** from the base and
  head trees, and are classified untrusted exactly like the head they contain.
- **Residual.** A maintainer who approves a fork run accepts its code. A job-token push from a fork run
  remains impossible.
- **Verdict.** **safe as specified — decided 2026-11-26 (D-13):** a per-repository `send_write_tokens`
  switch exists, off by default, flippable by `maintain` or above, and audited. A fork run stays read-only
  unless a maintainer writes that decision down.

### S14 — The importer

- **Parity behaviour.** n/a — it is the mechanism that replaces native discovery (§2.3).
- **Threat.** An importer that silently drops a key turns a security-relevant block (`permissions:`,
  `paths:`, a `container.options` refusal) into something weaker **while looking successful**. An importer
  that executes the foreign file, auto-commits to the default branch, or rewrites the source directory
  creates a second source of truth.
- **Control that keeps parity.** Read-only by default; `--write` writes only `.spindle/workflows/<name>.yml`
  plus a report; `--write --apply` commits on a **new branch** (never force, never the default branch);
  never executes the source; never edits the source directory; refuses (named, non-zero exit) every key it
  cannot translate; prints a unified diff; the report lists every dropped, renamed or approximated key.
- **Residual.** A translated file can still differ semantically where a compatible subset was chosen; the
  report names each one and the operator reviews the diff.
- **Verdict.** **safe as specified — decided 2026-11-26 (D-9):** `--apply` may commit the conversion on
  a **new branch only** — never the current branch, never the default branch, never a force push — so the
  importer can produce a reviewable change but can never rewrite what it did not write.

### S15 — Masking, annotations and summaries

- **Parity behaviour.** `::add-mask::`, `::error`/`::warning`/`::notice file=…,line=…`, step summaries as
  markdown, log folding.
- **Threat.** A masked value surfacing in a summary or an annotation; markdown/HTML injection on the run
  page (XSS); `::stop-commands::` token abuse; an annotation forging a file path.
- **Control that keeps parity.** The masker runs on log frames, step results, annotations and summaries
  **before storage**. Annotations are structured data rendered by the server (never HTML). Summaries go
  through the existing server-side markdown sanitizer (`internal/webui/mdsanitize.go`), so the XSS boundary
  does not move. `add-mask` only ever adds. `stop-commands` requires an unguessable token and is per-step.
- **Residual.** Masking stays exact-substring and best effort.
- **Verdict.** **safe as specified**.

### S16 — Step `background`, `wait`, `wait-all`, `cancel` and `parallel`

- **Parity behaviour.** GitHub's tracked docs revision adds step-level background execution: a step can
  start a process, later steps can `wait` for it, and `parallel` groups steps. Enterprise users use it for
  dev servers and sidecar builds.
- **Threat.** A background process outliving its step (and its job) holds the workspace, a port, or a
  credential file into the next job on a capacity-N runner; a `cancel` that does not kill the process group
  leaks it; `parallel` steps sharing one workspace interleave writes.
- **Control that keeps parity.** Every background process is started in its own process group and killed
  (SIGTERM then SIGKILL) at step end unless `wait`ed, at job end unconditionally, and the job's container is
  removed as it is today; `parallel` steps get the documented shared-workspace semantics and are recorded as
  one unit in the log; a leaked process fails the job's cleanup and is reported, never silently inherited.
- **Residual.** Two parallel steps writing one file race — the documented GitHub behaviour, and the same
  warning the docs carry.
- **Verdict.** **decided 2026-11-26 (D-7):** build background/parallel steps with the process-group
  kill above. The parity win is real for dev servers and sidecar builds, and the process-group rule is
  what keeps a background process from outliving the step that started it.

### S17 — Non-goals that are also security positions

- **Parity behaviour.** Forgejo documents LXC and host runner backends; GitHub documents Windows and macOS
  hosted runners and artifact attestations.
- **Threat.** An unisolated host backend is a cross-tenant hole (a job is a process on the operator's
  machine); a Windows/macOS fleet is infrastructure we do not have and cannot isolate the same way;
  attestations require a sigstore trust root spindle does not operate.
- **Control that keeps parity.** Each is a written non-goal (§8), refused **by name** when a workflow asks
  (`runs-on: windows-latest` queues with a reason that names the non-goal), so nobody waits on a job that
  will never run. The corpus expectations are adjusted in the same row.
- **Residual.** A repository that genuinely needs a macOS build keeps that job in its old CI.
- **Verdict.** **decided 2026-11-26 (D-15):** approve all three non-goals, each refused by name. An
  unisolated host backend, a Windows/macOS fleet we cannot isolate, and an attestation trust root we do
  not operate are infrastructure promises, not compatibility gaps.

---

### 7.18 Decision record (operator decisions, 2026-11-26 and 2026-11-27)

The operator decided **D-1…D-21** on **2026-11-26**. Each verdict above carries the chosen option and
its one-line rationale; this table is the index. A decision closes when the operator replies on the
master card; the matrix row then carries `approved_by` and the date, and the item moves. Until the
implementation lands, a row may still read `partial` with `decision: D-n` — the decision is settled,
the code is not. **D-22…D-27 were implemented under the plan on 2026-11-27 and are awaiting
acknowledgement**; §1.13 lists each with what changes if it is declined. Until acknowledged, their
rows keep `decision: D-n` and the code keeps the documented shape.

| ID | Decision | From | Blocks | Chosen / implemented |
|---|---|---|---|---|
| D-1 | Canonical `spindle.*` expression context + `SPINDLE_*` env, with `github.*`/`forgejo.*` as aliases | §2.2 | PAR-004 | Introduce it; the aliasing already exists and parity is unaffected |
| D-2 | `pull_request_target` at all; and head checkout inside it | S5 | PAR-015 | Per-repository opt-in, head checkout behind a second switch, both audited |
| D-3 | Untrusted-parent `workflow_run` / cross-run artifact consumption | S7 | PAR-019, PAR-098 | Approval per run; never automatic, never for a secrets-bearing job |
| D-4 | Job-level `continue-on-error` keeps GitHub semantics where Forgejo documents "ignored" | §9 | PAR-056 | Keep GitHub behaviour, record the divergence |
| D-5 | Remote/marketplace actions: default mode and the initial allowlist | S1 | PAR-070, PAR-071, PAR-074 | `allowlist` mode, SHA required outside it, mirror optional |
| D-6 | `secrets: inherit` across a repository boundary | S8 | PAR-076 | Refuse across repositories, even inside one organization |
| D-7 | Step `background`/`wait`/`wait-all`/`cancel`/`parallel` | S16 | PAR-080 | Build it, with process-group kill on every exit path |
| D-8 | Docker-in-job: daemonless only, or a runner-level daemon/privilege grant | S2 | PAR-081, PAR-179 | Daemonless default; the grant is a runner declaration, never a workflow one |
| D-9 | The importer may commit a converted file on a new branch | S14 | PAR-002 | Yes, `--apply` on a branch, never the default branch, never force |
| D-10 | Cache trust boundaries (fork/PR writes) and the size budget | S6 | PAR-095, PAR-096 | PR-scoped writes, no PR-to-PR reads, discard at merge |
| D-11 | Which token scopes may exist at all | S10 | PAR-112 | Only scopes with a real surface; refuse the rest by name |
| D-12 | The token a workflow gets with no `permissions:` block | S10 | PAR-114 | `contents: read`; Forgejo's implicit write stays refused |
| D-13 | A per-repository `send_write_tokens` switch for fork runs | S13 | PAR-118 | Exists, off by default, maintain+, audited |
| D-14 | Managed runner pool, and capacity > 1 for untrusted jobs | S11 | PAR-174, PAR-178 | Pool off by default; capacity 1 is the untrusted path |
| D-15 | The non-goal block: LXC/host backends, hosted Windows/macOS, artifact attestations | S17 | PAR-182 | Approve all three; each is refused by name |
| D-16 | Which structural field a matrix may vary | §4.4 | PAR-053, PAR-030 | `runs-on` **and `container.image`** substitute per cell (amended by PAR-030: the acceptance line requires `container: image: debian:${{ matrix.variant }}` to pull that tag, and the image gate must see the resolved image, not the authored span); every other structural field keeps the literal rule, and the `{group:, labels:}` form is refused by name |
| D-17 | `defaults.run` and expressions | §4.4 | PAR-054 | Literals only, as GitHub documents; a span there is a parse error, a step key still substitutes and wins |
| D-18 | The job timeout default and its conclusion | §4.4 | PAR-055 | 360-minute default, `1…360`, expression-valued; the job reads `timed_out` while the checks mirror maps it to `failure` (the checks enum has no timed-out state) |
| D-19 | Matrix admission without a hot-path store round trip for ordinary jobs | §4.4 | PAR-052 | A per-run CAS record, read/written only by a matrix that declares `max-parallel`; slots are released on finish, requeue and a lost claim |
| D-20 | `hashFiles` caps and the no-match value | §4.3 | PAR-041 | In-container walk, 1000 files / 100 MiB caps named in the error, no match is the empty string, cached per step and invalidated after a step |
| D-21 | A job `if:` reading secrets | S13 | PAR-058 | Allowed; a trusted run reads decrypted values, an untrusted run reads an empty map (false, not an error), nothing is logged |
| D-22 | Organization and user secret scopes, precedence and repository policy | §4.7 | PAR-115 | Organization secrets exist with a per-secret repository policy (`all`/`selected`), resolved environment > repository > organization, recorded on the job by scope only; the USER scope is declined (a run executes as the repository, not a person) |
| D-23 | A workflow-supplied registry login (`container.credentials`) | S12 | PAR-077 | **Refuse by name**, and keep the pull identity the runner operator's (`--registry-auth-file`): a workflow password would travel in the job payload (the bucket) and in docker create's environment, and every grant that widens isolation already excludes fork runs. A private image still pulls — as the runner, with its own credential. **Needs operator sign-off**: the row's "with workflow credentials" clause is narrowed to "pulls under the runner's credential" |
| D-24 | The default step shell when a workflow authors no `shell:` | §4.5 | PAR-079 (amendment), PAR-200 | **bash**, GitHub/Forgejo's Linux default (`bash --noprofile --norc -eo pipefail -c`), with `sh -e` as the stated fallback on an image without bash. The earlier `sh` default was a documented deviation that made a real corpus workflow (a `<<<` here-string) unexecutable; `shell: sh` remains available. |
| D-25 | The expression-function set for PAR-037 (`case` is not a platform function) | §4.3 | PAR-037 (amendment) | Amend the row's capability/acceptance from "`… hashFiles, case`" to the real GitHub/Forgejo set (success/failure/always/cancelled/canceled, startsWith/endsWith/contains/format/join, toJSON/fromJSON, hashFiles). `case` is not in either platform's language and is not invented here; it stays a named parse/evaluation error. Every real function has a conformance case in `TestExpressionFunctionSetMatchesPlatform`. |
| D-26 | Stock toolchain labels resolve to digest-pinned upstream images | §4.10 | PAR-177 | The catalogue is public upstream images pinned by manifest digest (ubuntu 24.04, node 22, python 3.13, Go 1.25, Rust 1, docker 27-cli); a stock `runs-on` label with no `container.image` resolves to that image at fetch time. A runner's own `label:docker://…` mapping still applies (PAR-171); with no mapping, an explicit workflow image wins over the stock catalogue, and an unknown label is refused by name. spindle does not publish or mirror images; `--image-prefix` covers air-gapped hosts. |
| D-27 | Email as a third failure-notification channel | §9.1a | PAR-197 | Implement a dependency-free SMTP notifier (stdlib `net/smtp` + `crypto/tls`) configured off by default under `threads.smtp.*`. It uses the same failure-only event filter as the inbox, sends only when the inbox record is first created (the record key is the dedupe), and masks the message with the run's job mask values; an unresolvable mask list skips email rather than sending unmasked. Credentials require TLS. |


## 8. What we will not do

Each row is a non-goal with its reason and whether the operator has signed it off. A non-goal is not a gap:
the matrix carries the row, the refusal is named, and the corpus expectation is adjusted.

| # | We will not… | Reason | Sign-off |
|---|---|---|---|
| N1 | …read `.forgejo/workflows/` or `.github/workflows/` at run time, in any mode | Product rule: Threads runs Spindle workflows. A second discovery path splits the product, doubles the parser surface, and makes "which file ran?" ambiguous. The importer is the migration (`PAR-002`) | **given** (the operator's product rule) |
| N2 | …present a Forgejo workflow as a supported format or use "Forgejo workflow" as product language | Same rule. Compatibility is documented as aliases (§2.2), not identity | **given** (product rule) |
| N3 | …trust an arbitrary `uses:` with no resolution record and no policy | A marketplace without pinning, an audit record and a revocation path is a supply-chain hole with a UI (`PAR-070`, S1) | **given** (D-5, 2026-11-26) |
| N4 | …accept a key we do not implement as inert | Silent acceptance is how `permissions` became a false promise. Named parse error or named refusal, always | policy, no sign-off needed |
| N5 | …let a workflow ask for `--privileged`, `--cap-add`, `--device`, `--security-opt`, host mounts, or the docker socket | Each is node-root from an anonymous PR (`PAR-081`, `PAR-179`, S2, S12). A runner may be granted the socket by its operator; a *workflow* never may | **given** (D-8, 2026-11-26 — the refusal is permanent) |
| N6 | …run a job unisolated on the host or in LXC; host Windows/macOS runners; artifact attestations | No isolation story / no fleet / no sigstore trust root (`PAR-182`, S17) | **given** (D-15, 2026-11-26) |
| N7 | …put a secret in argv (`container.entrypoint`, `command`, `options`) or in the plan payload when a name suffices | argv is host-visible; the payload is in the bucket (S3) | policy, no sign-off needed |
| N8 | …pass `secrets: inherit` to a cross-repository reusable workflow | Exfiltration by design (S8) | **given** (D-6, 2026-11-26) |
| N9 | …enable `pull_request_target` or untrusted-parent cross-run artifact consumption by default | The pwn-request and untrusted-artifact vectors (S5, S7) | **given** (D-2/D-3, 2026-11-26 — opt-in and approval, never default) |
| N10 | …implement merge-queue semantics in this plan | Owned by card `01a1fe8a26df000000c260ec351713`; here only the `merge_group` trigger rides on it (`PAR-016`) | out of scope, no sign-off |
| N11 | …give a workflow an implicit write token "because Forgejo does" | The whole point of a scoped token (S10) | **given** (D-12, 2026-11-26 — `contents: read` default) |

---

## 9. Traceability

Three artefacts, created with the first implementation item (`PAR-200`/`PAR-201`/`PAR-202`), not after it:

1. **`docs/features/parity/parity.json`** — the machine source of truth, one object per `PAR-` id:
   `{id, area, capability, provenance, status, wave, track, security[], decision, docs_refs[], tests[]}`.
   `status` is one of `have|partial|missing|non-goal`. A `non-goal` carries `approved_by`.
2. **`docs/features/parity/matrix.md`** — generated from the JSON by `internal/devtools/paritygate`; one
   table per area plus the docs-revision stamp (`forgejo v17.0.2 · github 2026-11-26 · checked <date>`).
   Never edited by hand (the gate fails if it differs from a fresh render).
3. **`internal/devtools/paritygate`** + **`make parity`** — a Go test (no new dependency; it parses the JSON
   with `encoding/json`) that fails when:
   - a row is `missing`, or `partial` with neither a wave nor a decision;
   - a row is `have` and its `tests[]` is empty, or names a `Test…` function that does not exist (grep over
     `internal/**/*_test.go`);
   - a `non-goal` row has no `approved_by`;
   - the master card's eight **track** checklists and the JSON disagree on the `PAR-` id set (read through
     the Weave API when `WEAVE_TOKEN` is present; skipped with a printed notice otherwise — the ids are also
     mirrored in the card body so the gate is never blind in CI). The ninth checklist, *Operator decisions*,
     names the items a decision blocks and is deliberately **not** part of the id-set comparison;
   - `matrix.md` is stale.

**The one rule that makes green mean something:** an item turns green **only** when its acceptance test is
committed *and* passes. A doc row moved to `have` without a test is a red gate, not a review comment. The
`1V` rows are the exception in form only — their test already exists and the row must name it.

Every PR that touches `internal/threads/` or `internal/actions*` updates the JSON row(s) and the card item in
the same change (AGENTS law 12: documents change with the code).

---

## 10. Decisions & deviations

*This section is the doc's own decision log (AGENTS law 12). Security verdicts live in §7, non-goals in §8;
here are the reconciliations this document performed on its sources.*

- **NEW (2026-11-26) — one master plan, not two parity matrices.** The two source cards disagree on keys
  both platforms spell (`continue-on-error`, `permissions`, implicit concurrency). One engine cannot have two
  answers, so `21_parity_master.md` is the work order and the per-row matrix is generated data beside it.
  Neither card's proposed path is used: `16_actions_parity.md` collides with `16_moderation.md`, and
  `21_github_parity.md` is superseded by this document.
- **NEW (2026-11-26) — discovery stays single-path; the importer replaces it.** Both cards' framing assumes
  a repository's existing CI directory can be read. It cannot: the product rule is Spindle workflows only.
  Every affected item became `PAR-002` (importer) plus a banner (`PAR-003`), and the corpus is defined as
  *converted* files with committed goldens.
- **NEW (2026-11-26) — `push.tags` is one P0, described both ways.** The Forgejo card says a tags-only
  trigger fires on every branch; the GitHub card says tags never fire. Both are true of the same code
  (`Fires` ignores `Tags` and `matchAny(nil)` is true). Split into `PAR-010` (fire on tags) and `PAR-011`
  (stop matching branches) because they fail independently and deserve separate tests.
- **NEW (2026-11-26) — job-level `continue-on-error` keeps GitHub semantics; Forgejo's "ignored" is the
  divergence.** The key already works here; removing the behaviour to match Forgejo's docs would break
  GitHub parity and silently change an existing green run red. Recorded as a divergence with D-4 rather than
  a code change.
- **NEW (2026-11-26) — the token default is scoped read, and Forgejo's implicit write is refused.** A
  security decision that deliberately gives up "runs unmodified" for workflows that relied on an unscoped
  token; the failure is a named 403, never a silent downgrade (`PAR-114`, D-12).
- **NEW (2026-11-26) — concurrency is a supersede model, not two features.** Forgejo's implicit cancel and
  GitHub's pending-cancel are the same intent at two levels: `PAR-150` (no `concurrency:` block → a new
  push supersedes the older run) and `PAR-152` (inside a group, the pending run is cancelled by default;
  `cancel-in-progress: false` sequences).
- **NEW (2026-11-26) — one corpus root, `internal/e2e/testdata/parity/`.** The Forgejo card said
  `internal/e2e/testdata/forgejo/`, the GitHub card said `internal/threads/testdata/github-corpus/`. One
  harness, one corpus, two subdirectories, a `SOURCE` stamp per file.
- **NEW (2026-11-26) — READMEs/docs name the compatibility aliases in one place.** WORKFLOWS §12a becomes the
  alias table (`github.*`/`forgejo.*`/`forge.*` + `GITHUB_*`/`FORGEJO_*` beside the canonical spelling), so
  no other page has to explain another platform's namespace.
- **NEW (2026-11-26) — items born in reconciliation.** `PAR-002` and `PAR-003` (the importer and its
  banner) and `PAR-004` (the canonical context) exist because no source item asked for them — they are what
  the product rule requires *instead* of discovery. `PAR-111` (`GITHUB_SERVER_URL`/`GITHUB_API_URL` as env
  vars) is implied by both cards' corpus but listed by neither. `PAR-011` is not new: it is the second half
  of Forgejo item F1.2, split out because it fails independently.
- **NEW (2026-11-27) — Wave 3 closes the GitHub-only trigger surface by emitting what exists and refusing
  the rest by name.** `repository_dispatch`, `workflow_run`, `create`/`delete`, `deployment`/
  `deployment_status` and `pull_request_target` are emitters; `merge_group`, `fork`, `watch`, `status`,
  `check_run`, `check_suite`, `registry_package`, `gollum`, `page_build`, `branch_protection_rule` and
  `public` are named parse errors with the missing surface in the message. Accepting an unemittable
  trigger would make a migrated workflow look healthy while never running, which is the one rule both
  source cards agree on. `PAR-016` therefore stays `partial` until the separate Merge queue card lands the
  queue and its documented ref shape.
- **NEW (2026-11-27) — the PR merge ref is a derived commit, not a published ref.** `pull_request` runs
  the server-computed merge commit (`git merge-tree --write-tree` + `git commit-tree` with a fixed
  identity/date) and reports `refs/pull/<n>/merge`; the commit lives in the serving copy so a checkout by
  sha works, and the same base+head mints the same sha. It is not WAL state and not a durable ref: after a
  serving-copy wipe the next PR event rebuilds it. A conflicting merge falls back to the head and
  `refs/pull/<n>/head` and says so — never a fabricated merge sha.
- **NEW (2026-11-27) — the per-repository trigger switches are a Threads-owned settings record, not
  `RepoSettings`.** `send_write_tokens`, `pull_request_target`, `pr_target_allow_head_checkout` and
  `concurrency_cap` live in `repos/<o>/<r>/threads/settings.json` with their own `…/api/threads/settings`
  endpoint and Threads-page form, because they are Threads engine policy (audit verbs, maintain+ floor,
  capacity release) rather than repository metadata. Every changed switch is audited individually; the
  head-checkout switch cannot be set without `pull_request_target`.

---

## Appendix A — Source item → `PAR-` mapping

Every checklist item from both cards appears exactly once, in card order. Numbers are
`<card><checklist>.<item>` (F = Forgejo card, G = GitHub card). An item mapping to two ids is a genuine
split (two independently testable acceptance lines); an item marked *superseded* is replaced by the product
rule and must not be built.

### A.1 Forgejo card — 69 items

| Item | Short | → |
|---|---|---|
| F1.1 | Discovery of `.forgejo/workflows` (+ `.github/workflows` fallback) | **superseded** → PAR-002, PAR-001, PAR-003 |
| F1.2 | `on.push.tags` fires; tags-only stops firing on branches | PAR-010, PAR-011 |
| F1.3 | `push.paths`/`paths-ignore` | PAR-012 |
| F1.4 | full `pull_request.types` | PAR-014 |
| F1.5 | `pull_request_target` | PAR-015 |
| F1.6 | `issues`, `issue_comment`, `release` | PAR-020, PAR-017 |
| F1.7 | filter glob dialect | PAR-013 |
| F2.1 | `${{ }}` in every documented site | PAR-030 |
| F2.2 | `secrets.*` as an expression | PAR-031 |
| F2.3 | `vars` + org/user/repo variables | PAR-035 |
| F2.4 | `forgejo`/`forge` contexts | PAR-032 |
| F2.5 | event payload + `EVENT_PATH` | PAR-033 |
| F2.6 | `runner` context + `RUNNER_*` | PAR-034 |
| F2.7 | expression functions | PAR-037 |
| F2.8 | operators (`<`, index, loose equality, value-returning logical and/or (the operand, not a boolean)) | PAR-038 |
| F2.9 | per-key availability, fail-closed | PAR-040 |
| F3.1 | job `continue-on-error` (Forgejo ignores) | PAR-056 |
| F3.2 | `defaults.run` | PAR-054 |
| F3.3 | `needs` as a string | PAR-050 |
| F3.4 | matrix object cells + `include`/`exclude` | PAR-051 |
| F3.5 | `strategy.max-parallel` | PAR-052 |
| F3.6 | `runs-on` substitution | PAR-053 |
| F3.7 | job `if` reads `secrets` | PAR-058 |
| F3.8 | `timeout-minutes` parity (job and step) | PAR-055 |
| F4.1 | remote actions | PAR-070 |
| F4.2 | JavaScript actions | PAR-071 |
| F4.3 | Dockerfile actions, `with.args`/`entrypoint` | PAR-072 |
| F4.4 | `actions/checkout` input set | PAR-073 |
| F4.5 | reusable workflows | PAR-075, PAR-076 |
| F4.6 | arbitrary `shell` | PAR-079 |
| F4.7 | `container` keys applied | PAR-077 |
| F4.8 | `services` parity | PAR-078 |
| F4.9 | `docker://` verify-and-close | PAR-082 |
| F5.1 | `GITHUB_ENV` | PAR-090 |
| F5.2 | `GITHUB_PATH` | PAR-091 |
| F5.3 | `actions/cache` end to end | PAR-095 |
| F5.4 | `STEP_SUMMARY`, 1 MiB | PAR-092 |
| F5.5 | upload/download inputs | PAR-097 |
| F5.6 | output heredoc + caps | PAR-094 |
| F5.7 | artifact retention defaults | PAR-158 |
| F6.1 | automatic token aliases | PAR-110 |
| F6.2 | token permission default (Forgejo: write, ignores `permissions`) | PAR-114, PAR-112 |
| F6.3 | org/user secret scopes | PAR-115 |
| F6.4 | secret availability in `env`/`with`/`if`, fork empty | PAR-116 |
| F6.5 | OIDC enable-key | PAR-119 |
| F6.6 | OIDC claim parity | PAR-120 |
| F6.7 | `ACTIONS_ID_TOKEN_*` exchange | PAR-120 |
| F7.1 | `environment` substitution | PAR-130 |
| F7.2 | env reviewers exceed Forgejo — verify | PAR-137 |
| F7.3 | env secret precedence/fork exclusion — verify | PAR-137 |
| F8.1 | implicit concurrency | PAR-150 |
| F8.2 | `cancel-in-progress` expression, `group` reads `vars` | PAR-151 |
| F8.3 | concurrency sequencing | PAR-152 |
| F8.4 | cron parity (`L`/`W`/`#`, macros, timezone) | PAR-022 |
| F8.5 | retention parity | PAR-158 |
| F8.6 | `on.schedule` non-default branch ignored — verify | PAR-022 |
| F9.1 | label→backend+image mapping | PAR-171 |
| F9.2 | registration scopes | PAR-172 |
| F9.3 | ephemeral runners | PAR-173 |
| F9.4 | operator policy parity (`privileged`, `network`, `valid_volumes`, `docker_host`, `timeout`) | PAR-179 |
| F9.5 | offline registration + multi-connection | PAR-180 |
| F9.6 | LXC and host backends | PAR-182 (non-goal) |
| F10.1 | log folding | PAR-190 |
| F10.2 | workflow commands + annotations | PAR-093 |
| F10.3 | re-run failed jobs | PAR-059 |
| F10.4 | run prioritization | PAR-155 |
| F10.5 | `run-name` + failure email | PAR-057, PAR-197 |
| F10.6 | check reporting shape | PAR-192 |
| F10.7 | offline validation / local run | PAR-199 |

### A.2 GitHub card — 90 items

| Item | Short | → |
|---|---|---|
| G1.1 | tag push triggers | PAR-010, PAR-011 |
| G1.2 | filter grammar (`branches-ignore`, `tags-ignore`, `+`, `!`) | PAR-013 |
| G1.3 | `paths`/`paths-ignore` | PAR-012 |
| G1.4 | PR merge-ref semantics | PAR-024 |
| G1.5 | full `pull_request.types` | PAR-014 |
| G1.6 | `pull_request_target` | PAR-015 |
| G1.7 | `merge_group` | PAR-016 |
| G1.8 | `release` trigger | PAR-017 |
| G1.9 | `repository_dispatch` | PAR-018 |
| G1.10 | `workflow_run` | PAR-019 |
| G1.11 | issues/review/label/milestone/discussion triggers | PAR-020 |
| G1.12 | full cron (+ non-default branch) | PAR-022 |
| G1.13 | dispatch input types `environment`/`number` + alias | PAR-023 |
| G1.14 | repo/check/deploy triggers | PAR-021 |
| G2.1 | function library | PAR-037 |
| G2.2 | operator semantics | PAR-038 |
| G2.3 | `github` identity/ref fields | PAR-032 |
| G2.4 | `${{ }}` in every documented site | PAR-030 |
| G2.5 | `secrets`/`github` in `run`/`with` | PAR-031 |
| G2.6 | `vars` context | PAR-035 |
| G2.7 | `runner` + `strategy` contexts | PAR-034, PAR-039 |
| G2.8 | `job`/`jobs` contexts, object filters | PAR-039, PAR-038 |
| G2.9 | `github` server/workflow fields | PAR-111, PAR-032 |
| G3.1 | matrix `include`/`exclude` | PAR-051 |
| G3.2 | `strategy.max-parallel` | PAR-052 |
| G3.3 | job timeout 360 min, `timed_out` | PAR-055 |
| G3.4 | `defaults.run` | PAR-054 |
| G3.5 | `run-name` | PAR-057 |
| G3.6 | concurrency parity (pending-cancel, expression, `queue:`) | PAR-152, PAR-151 |
| G3.7 | `continue-on-error` as an expression | PAR-056 |
| G3.8 | re-run failed jobs only | PAR-059 |
| G4.1 | remote `uses:` + allowlist | PAR-070 |
| G4.2 | JavaScript actions + pre/post | PAR-071 |
| G4.3 | official action parity (checkout/setup/artifact/cache) | PAR-073, PAR-096, PAR-097 |
| G4.4 | reusable workflows | PAR-075, PAR-076 |
| G4.5 | composite parity | PAR-074 |
| G4.6 | `pre`/`post` with pre-if/post-if | PAR-071 |
| G4.7 | Docker actions args/entrypoint | PAR-072 |
| G4.8 | container/service parity (credentials, ports, volumes) | PAR-077, PAR-078 |
| G4.9 | docker-in-job | PAR-081 |
| G4.10 | step `background`/`wait`/… | PAR-080 |
| G4.11 | shell parity | PAR-079 |
| G5.1 | cache service | PAR-095 |
| G5.2 | setup-* cache | PAR-096 |
| G5.3 | artifact v4 inputs | PAR-097 |
| G5.4 | cross-run artifacts | PAR-098 |
| G5.5 | `GITHUB_ENV`/`GITHUB_PATH` | PAR-090, PAR-091 |
| G5.6 | `STEP_SUMMARY` + `GITHUB_STATE` | PAR-092 |
| G5.7 | workflow commands | PAR-093 |
| G6.1 | permissions scope parity | PAR-112 |
| G6.2 | job token as API credential + `GITHUB_TOKEN` | PAR-113, PAR-110 |
| G6.3 | organization secrets with policy | PAR-115 |
| G6.4 | environment `vars` | PAR-117 |
| G6.5 | fork token policy | PAR-118 |
| G6.6 | masking completeness | PAR-116 |
| G6.7 | OIDC subject parity (environment) | PAR-120 |
| G6.8 | OIDC extra claims | PAR-120 |
| G6.9 | OIDC exchange parity | PAR-120 |
| G7.1 | environment protection parity | PAR-131 |
| G7.2 | deployment records + `deployment_status` | PAR-132 |
| G7.3 | `deployments` permission + API | PAR-133 |
| G7.4 | environment UI parity | PAR-134 |
| G7.5 | pending-deployment UX | PAR-135 |
| G7.6 | team-based reviewers | PAR-131 |
| G7.7 | deployment queuing | PAR-136 |
| G8.1 | schedule operations | PAR-154 |
| G8.2 | per-repo/org concurrency caps | PAR-153 |
| G8.3 | retention parity | PAR-158 |
| G8.4 | queue observability | PAR-156 |
| G8.5 | CI pause/resume | PAR-157 |
| G8.6 | GC parity + metrics | PAR-159 |
| G9.1 | hosted-label semantics | PAR-170 |
| G9.2 | managed runner pool | PAR-174 |
| G9.3 | runner groups | PAR-175 |
| G9.4 | larger-runner sizing | PAR-176 |
| G9.5 | stock toolchain images | PAR-177 |
| G9.6 | isolation parity | PAR-178 |
| G9.7 | fleet operations | PAR-181 |
| G9.8 | Windows/macOS support | PAR-182 (non-goal) |
| G10.1 | log fidelity (folding, timing, zip download, streaming) | PAR-190 |
| G10.2 | run graph UI | PAR-191 |
| G10.3 | checks parity + check-run API | PAR-192 |
| G10.4 | workflow/runs API + badges | PAR-193 |
| G10.5 | run-list filtering | PAR-194 |
| G10.6 | cancel/rerun granularity | PAR-195 |
| G10.7 | authoring affordances (validate-on-save, schema) | PAR-196 |
| G10.8 | notifications parity | PAR-197 |
| G10.9 | CLI parity | PAR-198 |
| G10.10 | conformance harness (corpus path) | PAR-200, **path superseded** (§10) |
| G10.11 | living matrix (`21_github_parity.md`) | PAR-202, **path superseded** (§10) |

### A.3 Deliberately dropped or narrowed

| Source item | What happens | Why |
|---|---|---|
| F1.1 / F top-10 gap 4 | Replaced by `PAR-002` (importer) | Product rule; native discovery is a non-goal (N1) |
| G4.9 "docker-in-job: opt-in daemon socket (policy-gated)" | Narrowed to a **runner-level** grant (`PAR-081`, D-8) | A workflow asking for the socket is node-root (S2); capability belongs to the operator's runner declaration |
| G5.4 "`$GITHUB_ARTIFACTS` / `ARTIFACTS_LIST`" | Dropped as a sub-requirement | Neither is a documented GitHub variable or action input; the documented surface is `run-id`/`artifact-ids`/`pattern`. The row (`PAR-098`) is built on those |
| G5.4 "artifact attestations" | Non-goal (`PAR-182`, D-15) | Requires a sigstore trust root spindle does not operate |
| G9.8 Windows/macOS | Non-goal (`PAR-182`, D-15) | No fleet, no isolation story; named refusal so nobody queues forever |
| F9.6 LXC/host backends | Non-goal (`PAR-182`, D-15) | Unisolated execution of anonymous code (S17) |
| F10.6 "check reporting shape" | Kept as a **recorded decision** inside `PAR-192`, not a build item | The current shape (`threads/<workflow>/<job>`) is a product choice; the row records it and names the required-check contexts |
| F10.5 `enable-email-notifications` | Folded into `PAR-197` | It is one surface of notifications parity |
| G1.1 vs F1.2 | Split into `PAR-010` + `PAR-011` | Two independent failures, two tests (see §10) |
| Both cards' prose "runs unmodified" | Narrowed to "**after conversion** to `.spindle/workflows/`" | The product rule; the importer exists precisely so no file needs an engine-side rename |

## Appendix B — Where the two cards contradicted each other, and the resolution

| # | Forgejo card | GitHub card | Resolution |
|---|---|---|---|
| 1 | `push.tags`: accepted, inert, and a tags-only trigger fires on **every branch** (P0) | `push.tags` never fires (P1) | Both true of the same code. One P0 pair: `PAR-010` (fire) + `PAR-011` (stop branch matching) |
| 2 | Job-level `continue-on-error` is implemented here but **ignored** by Forgejo (P0: match or record) | `continue-on-error` is baseline and must be expression-valued | Keep GitHub semantics; record the Forgejo divergence (`PAR-056`, D-4). Removing a working behaviour to match a doc would silently change a green run to red |
| 3 | Forgejo's token has repo **write** and ignores `permissions`; Threads defaults to read (P1: match or record) | `permissions` must drive real capabilities, read default implied | Secure subset wins: scoped token, read default, Forgejo implicit-write is a written divergence (`PAR-112`/`PAR-114`, D-12) |
| 4 | Forgejo cancels the previous run on push/PR-synchronize when `concurrency` is absent (P0) | `concurrency` queues where GitHub cancels the pending run (P2), and pending-cancel is required | One supersede model: `PAR-150` (no block → supersede) + `PAR-152` (in a group, cancel pending by default) |
| 5 | Discovery of `.forgejo/workflows` (+ `.github/` fallback) is P1 and "blocks every file" | No discovery item; corpus under `internal/threads/testdata/github-corpus/` | Product rule: no foreign discovery. Importer (`PAR-002`), corpus converted, one corpus root `internal/e2e/testdata/parity/` |
| 6 | Doc path `docs/features/16_actions_parity.md` | Doc path `docs/features/21_github_parity.md` | `16` is moderation; one document `21_parity_master.md` + generated matrix (preamble) |
| 7 | Corpus at `internal/e2e/testdata/forgejo/` | Corpus at `internal/threads/testdata/github-corpus/` | `internal/e2e/testdata/parity/{forgejo,github}/`, one harness |
| 8 | "Artifacts: have" | Artifact v4 input parity missing, cross-run missing | Not a contradiction (same-run vs v4 surface); union → `PAR-097`, `PAR-098` |
| 9 | "Environments: have (beyond Forgejo); no Forgejo syntax" (P3 verify) | Seven environment/deployment items | Union: the Threads features stay, GitHub's *protection* surface is built (`PAR-131`…`PAR-136`), the Forgejo exceedance is a verify row (`PAR-137`) |
| 10 | "Checks: have (own shape)" | Checks parity with annotations and API | Kept as one recorded naming decision plus real annotation work (`PAR-192`) |

## Appendix C — Wave 1 in one screen

| Batch | IDs | Owner | Closing evidence |
|---|---|---|---|
| 1a syntax core | PAR-010, 011, 030, 031, 032, 037, 050, 051 | T1 | Parser/expr table tests + the converted Weave/Scriobh files parse |
| 1b env + token | PAR-110, 111, 114, 170 | T3 | A job step sees `GITHUB_TOKEN`, `FORGEJO_TOKEN`, `GITHUB_SERVER_URL`; `ubuntu-latest` schedules |
| 1c execution | PAR-073, 090, 091 | T2 | `fetch-tags: true` checkout; `$GITHUB_ENV`/`$GITHUB_PATH` visible to the next step only |
| 1d semantics + proof | PAR-150, 056, 002, 200, 201, 202; verify rows PAR-001, PAR-082, PAR-137 | T3/T1/T7/T8 | Two rapid pushes leave one run; importer goldens; `make parity` green for the Forgejo corpus |

- **NEW (2026-11-26) — the gate ships in two modes, and the corpus tightens itself.** §9 says the
  gate fails on a `missing` row; taken literally, `make parity` could not be green in Wave 1 (the
  later waves are `missing` by design), which would make the merge gate permanently red and the
  rule the first thing anyone disables. So `make parity` (in `make ci`) enforces *integrity* — a
  `have` row needs an existing test, every gap is planned, the plan and the matrix agree on the id
  set, the render is fresh — and `make parity-closed` enforces the §9 *closure* rule (no `missing`
  row). The corpus mirrors it: a file blocked by an open row must fail with its documented refusal,
  and the entry flips to the strict assertion in the change that closes the row. The other
  deviation from this document's text: the four Forgejo corpus files need no dialect translation
  (their syntax is the accepted syntax), so their converted goldens are byte-identical to the
  sources and `SOURCE.json` records that; `TestParityImporterReproducesGoldens` turns the claim into
  the importer's own output the moment PAR-002 lands.
