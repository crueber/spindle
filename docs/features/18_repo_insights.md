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
most 365 days, **one** bounded `git ls-tree` for the language mix, plus bounded
reads of existing index/card families. The
maintainer sweeper refreshes stale projections on its cadence; a repository
writer can start the same task from the page and reload when it finishes. No
request path runs git, and no request path LISTs.

```
page GET ──► insights/pulse.json (1 GET) ──► render
                    ▲
maintainer due gate ─┤  insights-refresh task: 1 history walk + 1 tree listing
page POST (writer) ──┘  + bounded family reads + 1 CAS PUT
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
              "last_release_at": "2026-08-25T12:00:00Z",
              "packages": 2, "packages_known": true, "packages_capped": false,
              "threads_runs": 9, "threads_runs_known": true, "threads_runs_capped": false },
  "languages": [{"name":"Go","bytes":84000,"pct":84},
                {"name":"JavaScript","bytes":9000,"pct":9},
                {"name":"Other","bytes":7000,"pct":7}],
  "languages_capped": false
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
| last release stamp | `releases/latest.json` | 1 GET |
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

### 3.7 Language mix (committed bytes, not lines)

The refresh's second git call is ONE `git ls-tree -r -l -z <resolved-head>` on
the serving copy it already materialized (`04_git.md` §16.5): the byte size of
every regular blob in the served tree. Rows are grouped by a pinned
extension→language table (`internal/git/languages.go`) — Go, JavaScript,
TypeScript, CSS, HTML, Shell, Python, Rust, Java, C, C++, Ruby, PHP, JSON, YAML,
Markdown, SQL, Dockerfile, Makefile, everything else `Other`. This is an
**estimate from committed bytes, not a line counter**: a vendored tree, a
lockfile or a generated file counts by its stored size. The projection says so
in the card's method, not merely in this doc.

Only regular blobs count: trees are not emitted under `-r`, symlinks
(`120000`) and gitlinks (`160000`) are skipped, and an unparseable record is
ignored. Zero-byte languages are dropped (an empty file is not a language
signal), so an all-empty tree carries no rows and the card hides the row.
Percentages are whole numbers and the rows sum to exactly 100: the largest
entry absorbs the rounding residual (largest-remainder correction). Ordering is
bytes desc, name asc — deterministic independent of directory order.

The tally caps at **50,000 regular blobs** (`git.InsightsLanguageScanMax`); the
listing streams through a counting writer, so the cap bounds the counting work
without buffering the whole output. Past the cap the projection carries
`languages_capped: true` and both the card and the Insights page say the mix is
bounded. A subprocess failure fails the refresh (no half-truth projection).

The fields are **additive and optional** (`languages`, `languages_capped`,
`facts.last_release_at`): a projection written before them decodes with no rows
and the card hides only the rows it cannot fill. That is why the schema
`Version` stays 1 — a bump would invalidate every projection (and force a git
scan on every repository) to express a change that old and new readers already
tolerate. The next head move or 24 h TTL fills the new fields.

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

### 4.1 The Code page's "Code pulse" card

The repository **Code** page (`pages/repo/tree.html`, handler `repoTree`) carries
a second card in the About rail, beneath the About box: **Code pulse**. It is
the same projection, read with the same ONE exact-key GET, and it renders only
when the projection exists — an absent projection, an unwired seam or a read
error renders **no card at all** (the page is unchanged). It never runs git,
never LISTs and adds no JavaScript.

Four facts, each with its text reading:

1. **Momentum** — the 90-day commit total as text (`"41 commits · 90d"`) and an
   inline-SVG sparkline of the 30-day daily buckets (`Windows["30"].Buckets`,
   UTC). The `<svg role="img">` carries an `aria-label` that states the range,
   bucket count, the 30-day total, the peak day and the 90-day total; the text
   beside it repeats the fact. The sparkline is `polyline`, server-rendered, no
   chart library and no island.
2. **Top contributor share** — two lines from the 90-day window:
   `"Top contributor 78%"` (`Contributors[0].Commits / Commits`, its own `.kv`
   row) and `"3 contributors"` as a left-aligned caption beneath the row
   (`ContributorsTotal`), never a `.kv` value (it would read as a suffix to the
   percentage). A capped contributor list renders the true total with a `+`
   (`"60+ contributors"`). A zero-commit window omits both lines rather than
   printing `0%`.
3. **Cadence** — `"Last commit 2d ago"` from the Code page's own head-commit
   strip (`Latest.When`, the same `rel` treatment as every other stamp), and
   `"Last release 3mo ago"` from `Facts.LastReleaseAt` (`releases/latest.json`,
   §3.5) or `"No release yet"` when `ReleasesKnown && Releases == 0`. When
   `ReleasesKnown` is false, or a count exists without a stamp, the release row
   is omitted — never a guessed date.
4. **Language mix** — a thin stacked bar plus the text list (top 4 + `Other`,
   `"Go 84% · JavaScript 9% · CSS 7%"`). The bar is `aria-hidden` decoration on
   the `--pd-text-2` token with descending opacity; the percentages are in the
   text. A projection without `Languages` hides only this row.

The footer is `computed 12m ago` (relative from `ComputedAt`) linking to the
repository's Insights tab. `Scan.Truncated` says the commit counts cover the
most recent 20,000 commits; `LanguagesCapped` says the mix covers the first
50,000 files.

The card introduces no CSS class: it composes `.kv`, `.eyebrow`, `.link`, the
card surface (`--pd-surface` + `--pd-line`) and Tailwind utilities. The
composition is the shared pattern named in `docs/style-guideline.md` §7
("sidebar insight card"), with this page as the canonical reference. Rendered
proof: 1280 and 390 px, one `<h1>`, clean console, no page-level horizontal pan
(`scrollWidth === clientWidth`).

## 5. Cost

Evidence E20 (harness `internal/devtools/insightsbench`, `-tags evidence`):

| fixture | raw git scan (median of 3) | full refresh (median) | git calls / refresh | store ops / refresh |
|---|---|---|---|---|
| 1,000 commits | 17.6 ms | 20.7 ms | 2 | 58 |
| 10,000 commits | 175.5 ms | 175.5 ms | 2 | 58 |

The measurement host is a workstation VM; the durable shape is that cost is
dominated by the single history walk and linear in commits (not in repository
count or object count). The second invocation is the language-mix tree listing
(§3.7): constant and small at these fixtures. The 58 store ops are: projection
GET/HEAD/PUT, the shared index GET, 50 PR sidecar GETs, social GET,
`releases/latest.json`, three capped LISTs and their per-key metadata (the
memory backend counts each listed object).

The page read is one GET; no refresh work ever rides a request. The Code pulse
card adds the same one GET to the tree page (measured 2026-11-25, filesystem
store, 7 KB projection, 300 warm requests each: 11.1 ms without a projection
vs 12.0 ms with the card, **+0.9 ms median**). That is below the materiality
bar (5–10 ms, or a second sequential round trip), so the card reads the
projection directly and there is no memo to go stale.

## 6. Tests

- `internal/git/insights_test.go`: parser shapes (merge/no-stats, NUL fields,
  names with newlines, bad records), singular/plural shortstat, `--since`,
  cap/truncation, merge zero-lines, invalid inputs.
- `internal/git/languages_test.go`: the `ls-tree` tally against a real fixture
  (Go/JS/CSS/unknown/binary/empty/symlink/gitlink), byte sizes and file counts,
  the extension table, the entry cap's truncation flag, and the streaming
  parser's chunk boundaries.
- `internal/insights/insights_test.go`: UTC day/week/month bucket edges, window
  inclusion/exclusion, timezone normalization, future dates, contributor
  identity by email (same email, different name), ordering, cap, empty windows,
  collab counts, the language mix (sum-to-100 rounding, zero-byte drop,
  determinism).
- `internal/insights/refresh_test.go`: Load shapes (absent/corrupt/unknown
  version), staleness (head/TTL/version), refresh happy path, empty repo,
  `RefreshIfStale` skip, CAS retry, merge-sidecar cap, bounded facts, LIST
  failures as unknown, context cancellation, every error arm, the language
  scan's population/cap/error arms, the last-release pointer, and an old
  projection without the additive fields.
- `internal/webui/insights_test.go`: rendered page (one h1, tab, period links,
  chart bars/captions, facts, traffic copy, no ZgotmplZ), window
  selection/default, stale banner, empty states (no figure, no bars), chart
  scale (peak 100 %, zero buckets 0 %), the primary/detail split (non-zero rows
  only outside `<details>`, all N rows inside), uncomputed/unavailable,
  permission matrix (anonymous/private/public), refresh matrix (reader 403,
  writer 303, seam failure 503, cross-origin 403, anonymous interstitial), no
  cross-repo leak.
- `internal/webui/pulse_test.go`: the Code pulse card renders all four rows
  with exact text, the sparkline's `role="img"` label (range, totals, peak) and
  server-rendered `<polyline>`, the card sits inside the About `<aside>` with
  one `<h1>` and no script, capped contributors, `ReleasesKnown=false`,
  "No release yet", absent projection (no card), old projection (no language
  row), top-4 + `Other` folding, `Scan.Truncated`/`LanguagesCapped` wording, and
  the zero-commit window.
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
- **The Code pulse card is the same projection, not a second one (2026-11-25).**
  The Code page reads `insights/pulse.json` with one exact-key GET through the
  existing `RepoDeps.Insights` seam. It adds no key, no task and no git: the
  card is a render projection of the cached object, and an absent projection
  renders no card. Hotspots (most-changed files) and test-signal insights are
  deliberately NOT on the card — they remain Insights-page work, because a
  sidebar has room for four facts and must never become a second page.
- **Languages are an additive projection field, and the schema version stays 1
  (2026-11-25).** `languages` / `languages_capped` / `facts.last_release_at`
  are optional: an old body decodes with them absent and the card hides the
  rows it cannot fill, while an old reader ignores them. A `Version` bump would
  refuse every existing projection and force a git scan per repository to
  express a change both sides already tolerate; the next head move or the 24 h
  TTL fills the fields. Staleness stays head equality + TTL, unchanged.
- **The language mix is a committed-bytes estimate, not a line counter
  (2026-11-25).** One `ls-tree -r -l -z` at the resolved head, grouped by a
  pinned extension table, is deterministic and costs one spawn — versus a
  blame/line walk that would be linear in the whole tree and still wrong for
  one-line minified files. Vendored trees and generated files count by size by
  design; the card's method line says "committed bytes", and the cap (50,000
  files) is stated when it bites. Symlinks and gitlinks are excluded so a
  linked object cannot be double-counted.
- **The extra read is measured, not memaized by reflex (2026-11-25).** See §5:
  a warm Code page on the filesystem store costs single-digit microseconds more
  with the card (one `GetBytes` + JSON decode of an already-tiny projection),
  below the materiality bar (5–10 ms, or a second sequential round trip on a
  hot path), so the card reads the projection directly and there is no memo to
  go stale or to evict. If the projection grows or a remote backend changes the
  measurement, revisit with a bounded 30–60 s memo keyed owner/repo and
  invalidated by `computed_at`.
