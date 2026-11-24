# 18 — Repository Insights (the pulse card)

> Status: **shipped (2026-11-21)**. Package `internal/insights`; page
> `internal/webui/insights.go` + `templates/pages/repo/insights.html`;
> composition `cmd/spindle/insights.go`. Git argv: `docs/go/04_git.md` §16.4.
> Bucket family: `docs/go/02_storage_protobuf.md` §2.1. Cost: `docs/EVIDENCE.md` E20.

The Insights page answers "how is this repository doing?" from facts the
bucket and git actually hold: commit activity per period, distinct authors,
code frequency, issue/PR states, and the counters that exist (stars, watches,
forks, releases, package tags, Threads runs).

It deliberately does **not** invent traffic. spindle has no git access log, so
clone/fetch/view counts do not exist; the page says so in a dedicated card
instead of drawing a fake chart.

## 1. Architecture in one paragraph

Every read is ONE exact-key GET of a cached projection
(`repos/<o>/<r>/insights/pulse.json`). The projection is computed by a narrated
task (`insights-refresh`, Seam 5) that runs **one** bounded `git log` over at
most 365 days plus bounded reads of existing index/card families. The
maintainer sweeper refreshes stale projections on its cadence; a repository
writer can start the same task from the page and reload when it finishes. No
request path runs git, and no request path LISTs.

```
page GET ──► insights/pulse.json (1 GET) ──► render
                    ▲
maintainer due gate ─┤  insights-refresh task: 1 git scan + bounded family reads + 1 CAS PUT
page POST (writer) ──┘
```

## 2. Data model

### 2.1 Key and shape

`repos/<owner>/<repo>/insights/pulse.json` — JSON, version 1, CAS'd
(create-if-absent then update-if-version, bounded retry). The whole object is
derived and rebuildable; losing it loses warmth, never truth.

```json
{
  "version": 1,
  "computed_at": "2026-11-21T15:04:05Z",
  "head_sha": "<40/64 hex or empty for an unborn repo>",
  "windows": {
    "30":  { "days": 30, "bucket": "day",   "start": "...", "end": "...",
             "commits": 12, "additions": 300, "deletions": 40,
             "buckets": [{"start":"...","label":"Nov 1","commits":2,"additions":10,"deletions":1}],
             "contributors": [{"name":"Alice","email":"alice@example.com","commits":7,"last_active":"..."}],
             "contributors_total": 3, "contributors_capped": false,
             "prs_opened": 2, "prs_merged": 1, "issues_opened": 1, "issues_closed": 0 },
    "90":  { "...": "weekly buckets" },
    "365": { "...": "monthly buckets" }
  },
  "scan":   { "commits": 1234, "max_commits": 20000, "truncated": false,
              "future_dated": 0, "since": "..." },
  "collab": { "known": true, "index_bounded": false,
              "merge_scan_capped": false, "pr_sidecar_reads": 12 },
  "facts":  { "stars": 5, "watchers": 2, "forks": 1, "social_known": true,
              "releases": 3, "releases_known": true, "releases_capped": false,
              "packages": 2, "packages_known": true, "packages_capped": false,
              "threads_runs": 9, "threads_runs_known": true, "threads_runs_capped": false }
}
```

All timestamps are RFC 3339 **UTC**. The projection carries every selectable
window, so switching windows is a render choice, never a second computation.

### 2.2 Staleness

`staleness = (version != 1) OR (head_sha != current head) OR (age > 24 h)`.
The page compares the stored head with the chrome's head and shows a stale
banner (never a silent recompute). The sweeper's `RefreshIfStale` does the same
check before touching git: an unchanged repo costs one refs-level head read and
one exact-key GET.

### 2.3 Refresh triggers

- **Maintainer sweeper** (`insightsSweeper`, the packages-gc shape): per-repo
  in-memory due gate of 30 minutes, then `RefreshIfStale` through the
  `(repo, kind)` task table. A restart repeats a pass; it never skips one.
- **On demand** (`POST /{o}/{r}/insights/refresh`, write role): starts/joins
  the same task and returns immediately (303). The task table and the repo
  progress stream narrate it; the next page load reads the result.
- **Manual op** (`POST {lane}/ops/insights-refresh`): the same task via the
  Seam 5 op registry (`GET …/ops` lists it).

The refresh is single-flight per `(repo, kind)`; cross-instance duplication is
harmless (idempotent, CAS'd output).

## 3. Computation

### 3.1 The one git invocation

`git log --no-renames --no-ext-diff --no-textconv --no-show-signature
--no-decorate --max-count=<cap+1> --since=<UTC floor> --format=<record>
--shortstat <head>` (`04_git.md` §16.4). One spawn yields identity, committer
date and line totals for every commit in the widest window. `cap` defaults to
20,000; asking for one extra makes truncation observable without a second read.
`--since` is the earliest bucket start of the 365-day window; commits are then
filtered exactly per window in Go.

- **Date semantics:** committer date (`%cI`), the same preference as the
  repository activity stamp (Forgejo #142: commit date first, author date
  fallback). A rebase/cherry-pick therefore lands where it landed, not where it
  was originally written.
- **Merge commits** count as commits but contribute zero line changes (git log
  does not diff a merge without `-m`; walking both parents would double-count).
  Binary changes contribute zero.
- **Future-dated commits** (clock skew) are outside every window and counted in
  `scan.future_dated` rather than scattered into today's bar.

### 3.2 Bucketing (UTC, no local time)

| Window | Bucket | Rule |
|---|---|---|
| 30 days | day | today and the 29 UTC days before it; each bucket is `[00:00, next 00:00)` |
| 90 days | week | ISO weeks starting Monday; the first bucket starts on the Monday on/before the 89-day-old day, so the 90-day span shows 13 weekly buckets (12 full weeks + the current partial week) |
| 365 days | month | calendar months; the first month can be partial |

Window membership is `start <= committer_date <= computed_at` (inclusive both
ends). A commit exactly at a bucket boundary belongs to the later bucket. Bucket
labels are English and render the UTC date.

### 3.3 Contributors

Identity is the **lowercased email** (one person may commit under several
names). The display name is the newest name seen for that email. Each row
carries commits and the last-active committer date. Rows are sorted by commits
desc, last-active desc, email asc; the list is capped at 50 with
`contributors_total` carrying the true distinct count and `contributors_capped`
telling the UI to say "top 50".

### 3.4 Issues and PRs (existing families, bounded)

- One exact-key GET of the shared `issues/index.json` (02 owns the shape).
- **Opened** = cards (issue or PR) whose `created_at` is in the window.
- **Issues closed** = closed issue cards whose `updated_at` is in the window.
  Closed threads refuse conversation writes (02 §6.1), so the card's
  last-activity stamp is the close stamp; a reopen + re-close honestly moves it
  to the later close.
- **PRs merged** = closed PR cards in the window, enriched from at most
  `DefaultMergeScanCap` (200) `pulls/<num>/pr.json` sidecars — only a PR sidecar
  carries `merged`/`merged_at`. Past the cap the page states that merged counts
  cover the most recent 200 closed PRs.
- `index_bounded` is set when the shared index carries a compaction watermark;
  threads older than it are not counted, and the page says so.
- A read failure yields `known:false` ("unavailable"), never zero.

### 3.5 Facts (only what exists)

| Fact | Source | Cost |
|---|---|---|
| stars / watchers / forks | `meta/social.json` | 1 GET |
| releases | `releases/<tag>.json` headers | 1 capped LIST (100) |
| package tags | `packages/registry/tags/<tag>` | 1 capped LIST (100) |
| Threads runs | `threads/runs/<run>.json` | 1 capped LIST (100) |

Each count records `known` (read succeeded; absent = verified zero) and
`capped` (stopped at 100; the page renders "100+"). The LISTs run only in the
refresh task, never on a request; three LISTs for a repo whose head moved, zero
for an unchanged repo (the staleness gate returns first).

**Refused to invent: traffic.** Clone/fetch/view counts are not recorded
anywhere (no git access log), so the page has a "Traffic is not measured" card
that states the absence. No proxy metric is substituted.

### 3.6 Concurrency

- **Hazard:** two refreshes for one repository race the projection object; a
  page could read a body written from a different head.
- **Avoidance:** the `(repo, kind)` task table single-flights a refresh in the
  process; across instances both may compute, but the write is a bounded CAS
  (create-if-absent, then update-if-version) and the body is self-consistent
  (one head, one computed-at). A loser retries against the winner; no lock is
  held across any store or git call, and the page never takes a lock at all.
- **Hazard:** the maintainer's due gate is shared by concurrent passes.
- **Avoidance:** `insightsSweeper.mu` guards only its `map[string]time.Time`
  (a lookup/insert under a mutex, never held across I/O). The service's
  staleness check runs before git; an unchanged repo never spawns a process.
- **Channels/goroutines:** the on-demand start owns its goroutine via the task
  table's ctx (`Tasks().Run`); drain cancels it like every other task. No
  detached goroutine outlives the task table.

## 4. Page

- Route `GET /{owner}/{repo}/insights`, tab **Insights** (unconditional, before
  Settings). The tab strip scrolls internally at narrow widths (#274).
- Gate: `repoGate` + `repoOpen` — exactly the Code page. A private repository is
  a 404; the cache key contains owner/repo, so counts cannot cross repositories.
- No JS: period links are server-rendered anchors (`?window=30|90|365`).
  **The chart is the primary surface; per-bucket zeros are detail on demand.**
  Pulse and Code frequency render as server-side bar charts (`height: N%`
  computed in Go, 96 px tall, one flex column per bucket, `aria-hidden`
  decoration). Each chart is a `<figure>` whose `<figcaption>` states the
  totals, the active periods and the peak period in words; the existing
  six-stat `<dl>` stays as the numeric reading. A primary table lists only
  **non-zero** periods; a native `<details><summary>All N periods (including
  empty ones)</summary>` holds the complete table. An all-zero window renders
  the sentence, never an empty chart. The axis is one line (first → last bucket
  plus the active count), because 30 x-axis labels do not fit; the table
  carries precision. No chart library, no island, no new CSS class.
- One `<h1>` (the repository masthead); the page's title is a `<h2>`.
- Empty states are sentences: "No commits in this window", "No pull requests or
  issues in this window", "No commit authors in this window", "No code frequency
  data in this window", and the clone guide for an empty repository.
- Honesty notes: stale banner, truncated scan, bounded index, merged-PR cap,
  future-dated commits, and "a dash is a failed read, never a fabricated zero".
- 390 px: tables scroll inside `overflow-x-auto` wrappers; the page itself never
  pans. The chart is 30 flex-1 columns with a 2 px gap, so it fits without a
  wrapper.

Rendered proof (2026-11-22, 30 UTC-day buckets with one commit on Nov 20,
headless Chromium over CDP, dark/light/ocean): the page height at 1440×900 went
**3443 → 1772 px** and at 390×844 **4037 → 2407 px**; one `<h1>`; no page-level
horizontal pan (`scrollWidth == clientWidth`, including with both `<details>`
open); the disclosure opens to the full 30-row table; the zero buckets are a
flat baseline and the single bucket is a full-height bar; console clean.

The refresh form is rendered only for a viewer with the write floor, and the
POST re-checks it server-side (plus same-origin and the anonymous login
interstitial). It is a secondary `.btn` — the page has no primary CTA, so the
masthead Clone stays the one amber action.

## 5. Cost

Evidence E20 (harness `internal/devtools/insightsbench`, `-tags evidence`):

| fixture | raw git scan (median of 3) | full refresh (median) | git calls / refresh | store ops / refresh |
|---|---|---|---|---|
| 1,000 commits | 16.6 ms | 16.5 ms | 1 | 57 |
| 10,000 commits | 156.2 ms | 157.5 ms | 1 | 57 |

The measurement host is a workstation VM; the durable shape is that cost is
dominated by the single git spawn and linear in commits (not in repository
count or object count). The 57 store ops are: projection GET/HEAD/PUT, the
shared index GET, 50 PR sidecar GETs, social GET, three capped LISTs and their
per-key metadata (the memory backend counts each listed object).

The page read is one GET; no refresh work ever rides a request.

## 6. Tests

- `internal/git/insights_test.go`: parser shapes (merge/no-stats, NUL fields,
  names with newlines, bad records), singular/plural shortstat, `--since`,
  cap/truncation, merge zero-lines, invalid inputs.
- `internal/insights/insights_test.go`: UTC day/week/month bucket edges, window
  inclusion/exclusion, timezone normalization, future dates, contributor
  identity by email (same email, different name), ordering, cap, empty windows,
  collab counts.
- `internal/insights/refresh_test.go`: Load shapes (absent/corrupt/unknown
  version), staleness (head/TTL/version), refresh happy path, empty repo,
  `RefreshIfStale` skip, CAS retry, merge-sidecar cap, bounded facts, LIST
  failures as unknown, context cancellation, every error arm.
- `internal/webui/insights_test.go`: rendered page (one h1, tab, period links,
  chart bars/captions, facts, traffic copy, no ZgotmplZ), window
  selection/default, stale banner, empty states (no figure, no bars), chart
  scale (peak 100 %, zero buckets 0 %), the primary/detail split (non-zero rows
  only outside `<details>`, all N rows inside), uncomputed/unavailable,
  permission matrix (anonymous/private/public), refresh matrix (reader 403,
  writer 303, seam failure 503, cross-origin 403, anonymous interstitial), no
  cross-repo leak.
- `cmd/spindle/insights_test.go`: sweeper kind/due gate, maintainer
  registration, op listing and `opFn` mapping.

## Decisions & deviations from the Rust design

- **New family `repos/<o>/<r>/insights/pulse.json` (CAS'd, versioned,
  rebuildable).** The card asks for a cached projection and law 4 forbids
  request-path LIST/git. One object per repo (not one per window) makes the page
  read one key and keeps the windows consistent by construction; the family is
  added to the frozen overwritable list in `14_extensibility.md` §14.11 rule 2
  in this same change.
- **One git scan, three windows.** Windows are nested; scanning 365 days once
  and slicing in Go avoids the 3× spawn cost and the risk of the windows
  disagreeing about the head. The scan is capped (20k) and truncation is shown.
- **Committer date, not author date.** Matches the repository activity
  convention (#142) and keeps a rebase/import from scattering fresh work across
  old years. Documented on the page ("UTC" and the merge rule).
- **Contributor identity is email.** A name is display data; two people sharing
  a name must not merge, and one person renaming must not split.
- **Issue/PR counts read the shared index, not every thread.** The index is the
  frozen P4 projection; reading sidecars for every thread would be a LIST-scale
  scan. The trade-offs (compaction watermark, 200-PR merge classification) are
  surfaced in the UI instead of hidden.
- **No traffic metric.** There is no git access log; the page states this
  rather than inventing clones/views. GitHub's traffic section is explicitly
  out of scope for the same reason.
- **No new config keys and no new dependency.** The cadence (30 min due gate,
  24 h TTL) and caps are constants; the page adds no JS, no chart library and no
  CSS rule (it composes the existing tokens/classes).
- **Refresh is writer-gated, not admin-gated.** Computing a projection is
  derived work, not a mutation of repository content; the write floor matches
  the create-tag/star precedent while keeping an anonymous or read-only viewer
  from filling the task table.
