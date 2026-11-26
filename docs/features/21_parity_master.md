# 21 — Threads parity master plan (Forgejo-first, security-reviewed)

**Status:** work order. This document is the single source of truth for Actions parity work in Threads.
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
| PAR-002 | One-way importer `spindle threads import` (converted file + report + golden diff) | F | missing | 1 | T1 | Converting the 4 Forgejo corpus files produces files that parse and a committed golden per file; an untranslatable key is named in the report and exits non-zero | S14 |
| PAR-003 | Migration banner: a foreign workflow directory is *shown* with the importer command, never run | — | missing | 2 | T6 | The Threads page lists the foreign files and the exact `spindle threads import` invocation; no run is created by visiting it | S14 |
| PAR-004 | Canonical `spindle.*` context / `SPINDLE_*` env with `github.*`/`forgejo.*` as aliases (D-1) | — | partial | 2 | T1 | `spindle.ref`, `spindle.event_name` and `github.ref`, `forgejo.ref` resolve to the identical value in one table test | — |

### 4.2 Triggers and events

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-010 | `on.push.tags` fires on tag pushes (`refs/tags/*`) with tag globs and `GITHUB_REF=refs/tags/X` | B | missing | 1 | T1 | A `v1.2.3` tag matching `tags: [v*]` starts exactly one run; a non-matching tag starts none | — |
| PAR-011 | A tags-only trigger stops matching every branch push | F | missing | 1 | T1 | `on: push: {tags: ["v*"]}` on a branch push creates no run (the current `matchAny(nil)` behaviour is pinned as a failing-first test) | — |
| PAR-012 | `on.push.paths` / `paths-ignore` (and `pull_request.paths`) with `**`, ANDed with `branches` | B | missing | 2 | T1+T3 | A docs-only push against `paths: ['src/**']` creates no run; a mixed commit does; a `paths`-only trigger uses the pushed commit's diff | — |
| PAR-013 | Filter glob dialect: `*`, `**`, `?`, `+`, ordered `!` negation, `branches-ignore`/`tags-ignore` | B | partial | 2 | T1 | The documented filter cheat-sheet is a passing table test, including ordered include/exclude | — |
| PAR-014 | Full `pull_request.types` vocabulary and its emitters (closed, labeled, ready_for_review, review_requested, …) | B | partial | 2 | T3 | Each documented type fires from the real event through the pulls seam; the default stays opened/synchronize/reopened | — |
| PAR-015 | `pull_request_target`: base-tip file, base secrets, write token, head fetchable | B | missing | 2 | T3 | A fork PR run executes the base file and never the head's, with secrets resolving and the badge set | S5 D-2 |
| PAR-016 | `merge_group` trigger wired to the merge-queue card (no queue semantics here) | G | missing | 3 | T3 | A queue entry starts a `merge_group` run whose checks gate the merge | — |
| PAR-017 | `release` trigger (created/published/edited/deleted/prereleased/released) with `event.release.*` | B | missing | 2 | T3 | Publishing a release runs a workflow that reads its tag and asset list | — |
| PAR-018 | `repository_dispatch` with `types` + `client_payload`, reachable from API/CLI/scoped token | G | missing | 3 | T3 | A dispatch exposes `github.event.client_payload.*`; a token without `contents: write` is refused | S10 |
| PAR-019 | `workflow_run` (workflows/types/branches filters, upstream run + artifacts) with the untrusted-trigger rule | G | missing | 3 | T3 | A follow-up workflow downloads the first run's artifact and is itself gated when the parent was untrusted | S7 D-3 |
| PAR-020 | `issues`, `issue_comment`, `pull_request_review`(+`_comment`), `label`, `milestone`, `discussion`(+`_comment`) | B | missing | 2/3 | T3 | Each parses, is emitted by its owning package, and carries the documented payload; an `issue_comment` run uses the default-branch file | — |
| PAR-021 | Repository/check/deploy triggers: create/delete, fork, watch, status, check_run/check_suite, deployment(+_status), registry_package, gollum, page_build, branch_protection_rule, public | G | missing | 3 | T3 | Each parses and fires from a synthesized event; a trigger for a surface spindle lacks is refused by name, not inert | — |
| PAR-022 | Cron parity: names, `@daily`-family macros, `L`/`W`/`#`, per-entry `timezone:` with the DST rule; non-default-branch schedules ignored | B | partial | 2 | T1 | GitHub's and Forgejo's cron examples all parse and fire at the documented minute; the Berlin DST entry behaves as documented | — |
| PAR-023 | Dispatch input types `environment` and `number` plus the `github.event.inputs.*` alias | G | partial | 2 | T1+T6 | An `environment` input renders a picker and both `inputs.x` and `event.inputs.x` agree | — |
| PAR-024 | PR merge-ref semantics: the simulated merge commit (`refs/pull/N/merge`) is what runs | G | missing | 3 | T3 | A merge-only failure goes red; `GITHUB_REF`/`GITHUB_SHA` name the merge commit; the head is still what a fork run cannot control | S13 |

### 4.3 Expressions, contexts and functions

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-030 | `${{ }}` in every documented site: run, with, env, name, if, shell, working-directory, runs-on, container.*, services.*, environment, defaults, outputs, concurrency | B | partial | 1/2 | T1 | `container: image: debian:${{ matrix.variant }}` pulls that tag; `runs-on: ${{ matrix.os }}` selects; no supported span stays literal | S3 |
| PAR-031 | `secrets.*` as an expression, empty (never literal) on an untrusted run | B | missing | 1 | T1 | `env: TOKEN: ${{ secrets.TOKEN }}` yields the value, a fork run yields empty, and the log masks it | S3 S13 |
| PAR-032 | `github`/`forgejo`/`forge` contexts: full documented field set, identical values; identity/ref fields (actor, actor_id, triggering_actor, ref_type, ref_protected, head_ref, base_ref, run_number, run_attempt, workflow_ref, workflow_sha, action_path, action_ref, repository_owner(+_id)) | B | partial | 1/2 | T1 | One table test per field, resolved in a step and in `if:`; `${{ forgejo.actor }} == ${{ github.actor }}` | — |
| PAR-033 | `github.event` payload + `GITHUB_EVENT_PATH`/`FORGEJO_EVENT_PATH` JSON | B | missing | 2 | T1 | `jq -r .pull_request.number "$GITHUB_EVENT_PATH"` returns the PR number; `github.event.action` agrees | — |
| PAR-034 | `runner` context and `RUNNER_OS/ARCH/NAME/TEMP/TOOL_CACHE/ENVIRONMENT/DEBUG` | B | missing | 2 | T2 | `${{ runner.os }}` is `Linux`; `${{ runner.temp }}` is a writable directory inside the job | — |
| PAR-035 | `vars` context with repository/organization/user/environment variables, documented precedence, reserved-name refusal | B | missing | 2 | T3 | `${{ vars.X }}` resolves per scope; a `GITHUB_`/`SPINDLE_`-prefixed name is refused at save | — |
| PAR-036 | `env` context in expressions (workflow → job → container → step layering, visible to `if:`) | B | partial | 2 | T1 | `if: env.FLAG == '1'` follows the documented layer order in one table test | — |
| PAR-037 | Expression functions: contains, startsWith, endsWith, format, join, toJSON, fromJSON, hashFiles, case | B | missing | 1/2 | T1 | One conformance case per function with the platform-expected value; `hashFiles('**/go.sum')` returns a hash inside a job | — |
| PAR-038 | Operators: loose cross-type equality, `<`/`<=`/`>`/`>=`, `[ ]` index, property access, `*`/object filters, value-returning logical and/or (the operand, not a boolean) | B | partial | 2 | T1 | `1 == '1'`, `true == 'true'`, `'a' < 'b'`, `github['sha']` and a filter over a list all match | — |
| PAR-039 | `job`/`jobs`/`strategy` contexts (`job.status`, `strategy.job-index`, `job-total`, `max-parallel`) | G | missing | 3 | T1 | Each resolves in a step; `job.status` reflects a tolerated failure correctly | — |
| PAR-040 | Per-key context availability, fail-closed with a named error (never a literal, never an empty string) | B | partial | 2 | T1 | A context outside the documented set for that key is a named parse/eval error; the availability table is a test table | — |
| PAR-041 | `hashFiles` workspace walk in the job container (patterns, cache, caps) | B | missing | 2 | T2 | Two calls in one job hash the same bytes; a >N-file walk is refused with the cap named | — |

### 4.4 Jobs, matrix and flow control

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-050 | `needs` as a bare string parse (`needs: test` ≡ `needs: [test]`) | B | missing | 1 | T1 | `needs: test` in both Weave files parses and orders the job; a matrix dependency still waits for every cell | — |
| PAR-051 | `strategy.matrix.include`/`exclude` with the documented expansion rules, object cells | B | missing | 1 | T1 | The three documented `include` examples produce exactly the documented cell sets; Scriobh's `include:` list runs unmodified | — |
| PAR-052 | `strategy.max-parallel` and its effect on cell admission | B | missing | 2 | T3 | 6 cells with `max-parallel: 2` never have three running | — |
| PAR-053 | `runs-on` matrix/inputs substitution and the `runs-on: {group:, labels:}` form | B | missing | 2 | T1 | `runs-on: [docker, "${{ matrix.arch }}"]` selects the matching runner; the group form is honoured or refused by name | — |
| PAR-054 | `defaults.run.{shell, working-directory}` at workflow and job level | B | missing | 2 | T1 | The documented `defaults` example runs unchanged; a step-level key still wins | — |
| PAR-055 | Timeout parity: 360-minute job default, expression-valued, `timed_out` conclusion; step-level shape | B | partial | 2 | T3 | An idle job is cancelled at the documented limit and reads `timed_out`; a step `sleep` past its own limit kills the container | — |
| PAR-056 | `continue-on-error` as an expression, and the Forgejo divergence recorded (D-4) | B | partial | 1/2 | T1 | `continue-on-error: ${{ matrix.experimental }}` tolerates one cell; the matrix row states the Forgejo difference and names the approval | — |
| PAR-057 | `run-name` (expression-valued run title) | G | missing | 4 | T6 | The run list and API show the rendered value | — |
| PAR-058 | Job `if:` may read `secrets` and `vars`; an untrusted run reads an empty secrets context | B | missing | 2 | T1 | `if: secrets.DEPLOY_KEY != ''` evaluates on a trusted run and is false (not an error) on a fork | S13 |
| PAR-059 | Re-run failed jobs only, preserving successful jobs and their checks | B | missing | 2 | T3 | A partially failed run reruns only the failed jobs; the attempt increments; green checks are untouched | — |

### 4.5 Steps, actions and execution

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-070 | Remote `uses: owner/repo[/path]@ref` and full URLs: resolve, pin to a SHA, cache, honour allowlist/mirror policy | B | missing | 2 | T4 | `uses: https://code.forgejo.org/actions/setup-go@v5` runs unmodified; the resolved SHA is in the run header and the log; a non-allowlisted repo is a named refusal | S1 D-5 |
| PAR-071 | JavaScript actions (`runs.using: node20` / `node24`): `INPUT_*`, `GITHUB_OUTPUT`/`GITHUB_STATE`, pre/post hooks with pre-if/post-if | B | missing | 2 | T4 | A published node action passes its own test suite; a post hook runs after a failed main in the documented order | S1 |
| PAR-072 | Docker/Dockerfile actions: no `action.yml`, `runs.args`, `with.args`/`with.entrypoint`; `docker://` args parity | B | partial | 2 | T4 | A Dockerfile-only action builds and runs; `args` overrides the image CMD; `docker://` verify-and-close lands as a matrix row with its existing tests | S12 |
| PAR-073 | `actions/checkout` full input set: ref, repository, path, fetch-depth, fetch-tags, submodules, token, persist-credentials, clean, sparse-checkout | B | partial | 1 | T2 | Weave's `fetch-tags: true` dispatch checkout runs unmodified; every documented input is honoured or refused by name | S10 |
| PAR-074 | Composite parity: `${{ inputs.* }}`, `github.action_path`, nested remote `uses:`, per-step env layering, output threading | B | partial | 2 | T4 | `sigstore/cosign-installer` runs unmodified and its composite reads `action_path` | — |
| PAR-075 | Reusable workflows, local: `on.workflow_call` (typed inputs, secrets, outputs) + `jobs.<id>.uses`/`with`/`secrets:` | B | missing | 2 | T3 | The documented chain runs, each callee is visible on the run page, its output is consumable, permissions are downgrade-only | S8 |
| PAR-076 | Reusable workflows, cross-repo/remote: SHA-pinned resolution, explicit secrets only, 10-level cap | G | missing | 3 | T3 | `secrets: inherit` to a cross-repo callee is refused by name; a pinned remote callee runs; nesting 11 is refused | S8 D-6 |
| PAR-077 | `container` keys applied, not inert: credentials, ports, init, tty, entrypoint, command | B | partial | 3 | T2 | A private-registry image pulls with workflow credentials; the `sleep infinity` entrypoint example runs; a refused key is named | S12 |
| PAR-078 | `services.<id>` parity: Forgejo's `cmd` spelling, credentials, `--health-*` options and readiness gating | B | partial | 2 | T2 | The documented postgres service with `--health-cmd pg_isready` gates the job; a 60 s miss fails the job naming the service | — |
| PAR-079 | `steps[*].shell` as an arbitrary command with `{0}`; `node`/`python` aliases; bash→sh fallback | B | partial | 2 | T2 | `shell: dash -e {0}` runs; `python` maps to `python {0}`; a missing bash falls back with a log line | — |
| PAR-080 | Step `background`/`wait`/`wait-all`/`cancel`/`parallel` (new keys) — or an approved non-goal (D-7) | G | missing | 3 | T2 | Either each behaves as documented (process group killed on exit) or the matrix row is an approved non-goal | — |
| PAR-081 | Docker-in-job: daemonless builders documented as the parity path; optional runner-level daemon/privilege grant | B | partial | 2 | T2 | A buildah corpus job builds and pushes; a job asking for the socket on a non-granting runner is refused by name | S2 D-8 |
| PAR-082 | Built-in action surface verify-and-close (`docker://` never receives secrets or the job token; digest policy) | F | have | 1V | T7 | The existing tests are named in the matrix row and re-run in the parity job | S13 |

### 4.6 Command files, artifacts and cache

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-090 | `$GITHUB_ENV`/`$FORGEJO_ENV`: later steps only, heredoc, caps, protected-name refusal | B | missing | 1 | T2 | `echo "TAG=$TAG" >> "$GITHUB_ENV"` is visible to later steps and not before; `LD_PRELOAD` is refused by name | S4 |
| PAR-091 | `$GITHUB_PATH`/`$FORGEJO_PATH` | B | missing | 1 | T2 | A directory appended in one step is on `PATH` in the next, prepended, and never replaces it | S4 |
| PAR-092 | `$GITHUB_STEP_SUMMARY` (1 MiB/step, sanitized markdown) and `$GITHUB_STATE` for pre/post | B | missing | 2 | T2 | The documented Mermaid example renders safely on the job page; state survives into a post hook | S4 S15 |
| PAR-093 | Workflow commands: `::error`/`::warning`/`::notice` with file/line/col/title, `::group::`/`::endgroup::`, `::add-mask::`, `::stop-commands::`, `::set-output` if honoured | B | missing | 2 | T2 | `::error file=a.go,line=3::msg` is a line-anchored annotation on job and commit; a masked value never prints later | S15 |
| PAR-094 | `$GITHUB_OUTPUT` heredoc, caps and collision rules (last wins; malformed line is a named failure) | B | partial | 2 | T2 | The documented multi-line heredoc publishes an output readable via `steps.<id>.outputs`; a malformed line names the line | — |
| PAR-095 | Cache service: key/restore-keys, branch and default-branch scope, `cache-hit`, eviction, size budget | B | missing | 2 | T4 | Run 2 restores run 1's cache; a miss is not a failure; lookup order is a test table; a fork run cannot write a base-readable entry | S6 D-10 |
| PAR-096 | `setup-*` cache integration (`cache: npm` / `pip` / `go`) on the cache service | G | missing | 2 | T4 | `setup-node` with `cache: npm` restores and saves with no separate cache step | S6 |
| PAR-097 | Artifact v4 inputs: overwrite, compression-level, include-hidden-files, pattern, merge-multiple, artifact-ids, 255-char names, no-name download-all | B | partial | 2 | T5 | `pattern: dist-*` with `merge-multiple: true` yields one merged tree; a no-name download fetches all | S7 |
| PAR-098 | Cross-run artifacts (download from another run id, same repository only) | G | missing | 3 | T5 | A `workflow_run` job downloads the upstream artifact; a cross-repository request is refused by name | S7 D-3 |
| PAR-099 | Artifact list/delete API, page list and a sweep report | G | partial | 3 | T5 | Bytes freed are reported per family; a delete removes blob and manifest | S7 |

### 4.7 Secrets, tokens and OIDC

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-110 | Automatic token aliases: `FORGEJO_TOKEN`/`GITHUB_TOKEN` in env, `secrets.GITHUB_TOKEN`, `forgejo.token` | B | missing | 1 | T3 | Weave's `FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}` checkout works unmodified; the value never appears in argv or the plan payload | S10 |
| PAR-111 | Server/API addresses: `GITHUB_SERVER_URL`, `GITHUB_API_URL`, `FORGEJO_SERVER_URL` env and `github.server_url`/`api_url`/`graphql_url`/`retention_days` | — | missing | 1 | T3 | Scriobh's `$GITHUB_SERVER_URL/api/v1/...` curl succeeds; `${{ github.server_url }}` equals the env var | — |
| PAR-112 | Permission scopes: every documented scope drives a real capability or is refused by name | B | partial | 2 | T3 | `issues: write` lets a job comment; an unimplemented scope is a parse error naming it (never inert) | S10 D-12 |
| PAR-113 | Job token as a scoped platform API credential (comment, label, release, package) | G | partial | 2 | T3 | A workflow comments on its own PR over HTTP with `$GITHUB_TOKEN`; the same token is refused another repository | S10 |
| PAR-114 | Token default recorded: scoped + `contents: read`; Forgejo's implicit-write token is an approved divergence | F | partial | 1 | T3 | The matrix row states both behaviours and the approval; a workflow relying on implicit write fails with a 403 that names the missing scope | S10 D-12 |
| PAR-115 | Organization and user secret scopes with documented precedence and repository policy | B | missing | 2 | T3 | An org secret resolves in every org repo; a repo secret of the same name wins; an unselected repo gets nothing | — |
| PAR-116 | Secret availability in `env`/`with`/`if`; fork runs empty; masking completeness (`::add-mask::`, annotations, summaries) | B | partial | 2 | T3 | A fork run's `${{ secrets.X }}` is empty and its env var unset; a masked value never appears in a later log line, annotation or summary | S13 S15 |
| PAR-117 | Non-secret `env` variables (repository/organization/environment) alongside environment secrets | G | missing | 2 | T3 | `${{ vars.X }}` resolves differently per environment; `env` and `vars` never collide | — |
| PAR-118 | Fork token policy: per-repo send-write-tokens setting, `pull_request_target` trust split, first-run approval | G | partial | 3 | T3 | Flipping the setting makes a fork run write-capable, the change is audited, and the run page says so | S13 D-13 |
| PAR-119 | OIDC enable-key parity: `enable-openid-connect: true` beside `permissions: id-token: write` | F | missing | 2 | T1 | A Forgejo workflow with both spellings parses and mints a token with zero edits | S9 |
| PAR-120 | OIDC claim and exchange parity: `iss`, PR `sub`, environment `sub`, `aud` default, `ref_protected`, `ref_type`, `job_workflow_ref`, `runner_environment`, `?audience=`, TTL, error shapes | B | partial | 2 | T3 | A provider trust policy written for either platform validates a Threads token; the `environment` claim appears only when a job names one | S9 |

### 4.8 Environments, deployments and approvals

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-130 | `environment`/`environment.url` expression substitution (or a named parse error, never a literal bucket key) | B | missing | 2 | T1 | `environment: ${{ matrix.env }}` resolves; an unresolvable reference is the file's named error | — |
| PAR-131 | Protection parity: wait timer, ≤6 required reviewers, prevent-self-review, deployment branch/tag policy, team reviewers | B | partial | 2 | T3 | All five are enforced and shown on the deployment; a team member's approval counts | — |
| PAR-132 | Deployment records: per-environment history, `deployment_status`, environment URL on the repo page | G | partial | 3 | T3+T6 | Each deployment has a state history and a working URL link | — |
| PAR-133 | `deployments` permission and deployment API (`POST /deployments`, statuses) | G | missing | 3 | T3 | A workflow-created deployment appears in the UI with its statuses; a token without `deployments: write` is refused | S10 |
| PAR-134 | Environment UI parity: deployments, secrets, variables and protection-rule tabs | G | partial | 3 | T6 | All four surfaces exist and write; mobile and desktop checks pass | — |
| PAR-135 | Pending-deployment UX: run-list badge, reviewer notification, approval audit entry | G | partial | 2 | T6 | A reviewer is notified, the badge names the environment, the audit names who approved what | — |
| PAR-136 | Deployment queuing: a second deployment to a busy environment serializes instead of failing | G | missing | 3 | T3 | Two concurrent deploys to one environment run in trigger order, the second visibly queued | — |
| PAR-137 | Environment verify-and-close: secret precedence, fork exclusion, and "Threads exceeds Forgejo (no environment syntax)" | F | have | 1V | T7 | Existing tests are named in the matrix rows; the row states the exceedance so nobody builds Forgejo parity work here | — |

### 4.9 Concurrency, scheduling, retention and GC

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-150 | Implicit supersede: a new push / PR `synchronize` cancels the superseded in-flight run of that workflow+ref when no `concurrency` block exists | F | missing | 1 | T3 | Two rapid pushes leave one in-flight run; the superseded run reads `cancelled` with a reason naming the superseding run | — |
| PAR-151 | `cancel-in-progress` as an expression; `group` reading `vars`/`inputs`/`matrix`/`github` | B | partial | 2 | T1 | `cancel-in-progress: ${{ github.ref_name != 'main' }}` behaves as documented; an unresolvable group still fails closed | — |
| PAR-152 | Group semantics: cancel the *pending* run by default, `cancel-in-progress: false` sequences, `queue: single` / `max` | G | partial | 2 | T3 | Three rapid pushes to one group leave one running and one pending; sequencing runs in trigger order to completion | — |
| PAR-153 | Per-repository and per-organization concurrency caps | G | missing | 3 | T3 | A cap of 2 queues a third run with exactly one named reason | — |
| PAR-154 | Schedule operations: next-fire in UI/API, enable/disable without a commit, operator-visible watermark | G | partial | 4 | T6 | The page shows the next fire per cron and a disable takes effect immediately | — |
| PAR-155 | Run prioritization | F | missing | 4 | T3 | A prioritized run's jobs are claimed before older queued work; the ordering is documented | — |
| PAR-156 | Queue observability: exactly one named reason per queued job (no runner, labels unmatched, group held, approval, capacity) | G | partial | 3 | T6 | Every queued job shows exactly one named reason, and the reason changes when the cause clears | — |
| PAR-157 | Per-repository CI pause/resume with in-flight jobs finishing | G | missing | 4 | T3 | A paused repo claims nothing and resumes cleanly | — |
| PAR-158 | Retention parity: configurable ceiling (≥90 days), keep-forever, separate run/log and artifact windows with artifact expiry defaults | B | partial | 2 | T5 | A 90-day setting is honoured and reported; an artifact outlives its run only when its own window allows; keep-forever works | — |
| PAR-159 | GC parity: expired artifacts, unreferenced blobs, cache eviction, reclaimed bytes per family | G | partial | 2 | T5 | GC reclaims and reports the bytes it freed per family; a dry run reports the same numbers without deleting | — |

### 4.10 Runners and infrastructure

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-170 | Hosted-label semantics: `ubuntu-latest` (and friends) via a documented operator mapping; `runs-on: {group:, labels:}` | B | missing | 1 | T3 | A `ubuntu-latest` corpus job runs with zero edits on an instance whose operator mapped the label; an unmapped label queues with a named reason | S11 |
| PAR-171 | Label→backend+image mapping (`name:docker://image`, `name:lxc://template`, `name:host`) | F | partial | 2 | T3 | A runner declaring `ubuntu-latest:docker://node:lts` runs that job in that image | S11 S17 |
| PAR-172 | Registration scopes: instance, organization, user, repository | F | partial | 3 | T3 | An org-scoped runner serves only that org and appears in its repositories' lists | — |
| PAR-173 | Ephemeral runners: one job, then the record and credential are gone | F | missing | 3 | T3 | The record is gone after one job and its credential is rejected | — |
| PAR-174 | Managed runner pool (ephemeral, container-backed, autoscaled) so a fresh instance runs queued jobs unattended | G | missing | 3 | T3 | A fresh spindle boots and a queued job runs unattended inside the documented bounds | S11 D-14 |
| PAR-175 | Runner groups with org/repo visibility and inherited labels | G | missing | 3 | T3 | A group's runner is claimable only by allowed repositories, with inherited labels matched | — |
| PAR-176 | Larger-runner sizing (per-job cpu/memory) or a named queue reason | G | missing | 3 | T3 | A job asking 4cpu/16GB lands on a matching runner or is queued with that exact reason | — |
| PAR-177 | Stock toolchain images (node/python/go/rust/docker CLI + buildx) published and documented | G | missing | 3 | T3+T8 | The JS matrix, Go build and docker build corpus rows run on stock images | S2 |
| PAR-178 | Isolation parity: concurrent jobs share no namespace, and the capacity model is documented | G | partial | 3 | T3 | Two concurrent jobs cannot see each other's processes or volumes; the doc states which setting buys which guarantee | S11 D-14 |
| PAR-179 | Operator policy knobs: `container.privileged`, `network`, `valid_volumes`, `docker_host`, `runner.timeout` — each with a safe default | F | partial | 2 | T3 | Each knob exists with a documented default; `privileged` is refused unless the runner opted in | S2 S12 D-8 |
| PAR-180 | Offline registration (config file, no browser) and multi-scope from one process | F | missing | 4 | T3 | A runner registers from a config file and serves two scopes from one process | — |
| PAR-181 | Fleet operations: list/drain/revoke, zero-downtime rotation | G | partial | 4 | T3 | Draining finishes in-flight jobs and claims nothing new; revocation is a real 401 | — |
| PAR-182 | **Non-goal:** LXC and unisolated host backends; hosted Windows/macOS runners (D-15) | B | missing | 4 | T8 | The matrix rows carry the non-goal, its reason and the named signatory; a workflow asking for one is a named refusal | S17 |

### 4.11 Observability, API, CLI and UI

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-190 | Log fidelity: `##[group]` folding, per-step timing, whole-run log download (txt/zip), streaming that survives session expiry | B | partial | 2 | T2+T6 | Grouped logs collapse; a zip download contains every job's log; an expired session reconnects instead of dying | S15 |
| PAR-191 | Run graph UI: job DAG with live status, matrix cells grouped under their job, per-step drill-down | G | partial | 2 | T6 | A matrix run renders one row per cell with its check and duration; the DAG shows blocked dependents | — |
| PAR-192 | Checks parity: per-check annotations, re-run from the PR panel, per-branch required-check config, check-run API accepting annotations; check-context naming decision recorded | B | partial | 3 | T6 | An external CI posts annotated checks that gate the merge; the naming decision names the contexts required checks use | — |
| PAR-193 | Workflow/runs API and badges (`…/badge.svg`) | G | partial | 3 | T6 | A README badge reflects the branch's latest conclusion; the API lists/filters runs | — |
| PAR-194 | Run-list filtering parity: branch, event, status, workflow, actor, with pagination | G | partial | 3 | T6 | The documented filter set narrows the list correctly, and the page stays usable at 390 px | — |
| PAR-195 | Cancel/rerun granularity: whole run, single job, force-cancel, and a "cancelled by X" record | G | partial | 3 | T3+T6 | Cancelling one job leaves its siblings running; the record names the actor | — |
| PAR-196 | Authoring affordances: validate-on-save with key-level errors, schema-assisted editor | G | missing | 4 | T6 | An invalid key is reported in the editor, not only after a push; the message names key and file | — |
| PAR-197 | Notifications parity: failure notifications (UI/webhook/email) and per-user workflow subscriptions | B | partial | 4 | T3+T6 | A failed watched workflow notifies its subscribers once, with the run link | — |
| PAR-198 | CLI parity: `threads run --ref` with inputs, `threads watch`, run/job listing | G | partial | 4 | T6 | The CLI dispatches with inputs and follows the run to completion; exit codes match the API | — |
| PAR-199 | `spindle threads validate` offline validation with the same errors the server reports | F | missing | 2 | T1+T6 | A file validates offline with byte-identical error text to the server for a table of bad files | — |

### 4.12 Conformance and traceability

| ID | Capability | Prov | Status | W | Track | Acceptance | Sec |
|---|---|---|---|---|---|---|---|
| PAR-200 | Conformance corpus + harness (`internal/e2e/parity_test.go`, `testdata/parity/{forgejo,github}`) driven through discovery → run → checks → artifacts | B | missing | 1 | T7 | `make parity` runs the corpus; a deliberately broken row makes it fail; a Forgejo corpus entry runs unmodified after conversion | — |
| PAR-201 | `make parity` gate: no `missing` row, no `partial` without a wave or decision, no `have` row without an existing, passing named test | B | missing | 1 | T7 | Removing a named test from the tree fails the gate with the row id; adding a row without a test fails | — |
| PAR-202 | Living matrix (`docs/features/parity/parity.json` + generated `matrix.md`) with the docs-revision stamp; docs drift corrected | B | missing | 1 | T8 | Every `PAR-` id appears once; the stamp names both docs revisions; `docs/threads.md` §11 matches §5.5/§5.6 and `15_threads.md` §2.6/§2.7 | — |

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

### 7.18 Decision record (operator decisions, 2026-11-26)

The operator decided all fifteen on **2026-11-26**. Each verdict above now carries the chosen option and
its one-line rationale; this table is the index. A decision closes when the operator replies on the master
card; the matrix row then carries `approved_by` and the date, and the item moves. Until the implementation
lands, a row may still read `partial` with `decision: D-n` — the decision is settled, the code is not.

| ID | Decision | From | Blocks | Chosen (2026-11-26) |
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
