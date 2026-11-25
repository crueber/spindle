# 20 — Code search (and issue/PR text search)

> Status: **shipped** (2026-11-25). **Card:** `01a1fe8a26d100000012bc64fbbb1b`
> (Spindle board). Phases: **P0** this doc + the frozen interfaces; **P1** the
> `internal/git` grep primitive (`docs/EVIDENCE.md` E21); **P2** the service
> (`internal/search`: scopes, query grammar, permission-aware fan-out,
> completeness), `wal.RepoHandle.PacksReady`, the `[search]` config keys and the
> composition; **P3** the JSON API lanes, the `spindle search` CLI and the SDK
> methods; **P4** the issue/PR searcher (`internal/search/issue.go` over the
> additive `issues.SearchProjection` read); **P5** the repository and owner
> pages plus the repository-header box; **P6** the end-to-end scenario
> (`internal/e2e/search_test.go`) and this as-built record.
>
> Normative git argv: [`docs/go/04_git.md`](../go/04_git.md) §17. Wire shapes:
> [`docs/go/07_api.md`](../go/07_api.md) §9.13. Pages/chrome:
> [`docs/go/12_web_ui.md`](../go/12_web_ui.md) §2.2.1/§2.3.1. Cost:
> [`docs/EVIDENCE.md`](../EVIDENCE.md) E21.

Search answers "where is this text?" over things spindle already holds: the
repository's trees at a resolved commit, and the issue/PR threads in the
collaboration object family. It introduces **no new object family, no index, no
database and no dependency**: every code answer is one `git grep` subprocess on
the serving copy, and every issue answer is the existing index/card/event reads
with a bounded text scan.

## 1. Goals and non-goals

**Goals (v1)**

- Exact-at-the-sha code search in **one repository** (`scope: repo`), with the
  ref resolved first and the answer pinned to the resolved object id.
- **Bounded owner-scope** code search across the repositories the caller can
  read (≤ 50 candidates, warm-only, honest `complete:false` when capped).
- Issue **and** pull-request search in the same card: title, labels, author,
  state and kind from the existing shared index, plus body/comment text under
  bounded scans with moderation redaction. This is the operator's decision
  (Decisions §12.4), not a follow-up.
- Every cap, timeout and cold state is a **field**, never a silently short list
  (AGENTS law 7).
- Surfaces: `GET /{o}/{r}/api/search`, `GET /api/v1/search`, the repo page
  `/{o}/{r}/search`, the public page `/search?owner=`, the repo-header search
  box, `spindle search`, and the SDK's `repo.search`/`client.search`.

**Non-goals (v1, each with a named reason)**

- **An index.** No inverted index, no shards, no posting lists, no new bucket
  keys (§2, §12.1). The serving copy already exists for browsing/blame/compare
  and is the natural corpus.
- **Instance-wide code search.** Without an owner the cost is every repository
  on the instance and cannot be bounded honestly. `/search` without `owner`
  says so and links to the repository picker.
- **Ranking, symbol/definition search, match counts over the whole tree.**
  Results are git tree order (path) and ascending line: deterministic and
  cheap. Relevance is a separate engine and a separate decision.
- **Searching remote-served repositories.** A repository whose pack set exceeds
  the cache budget has no local tree; see §9 (`not_warm`/503).
- **Multi-branch or unreachable-object search, fork search, saved searches,
  typeahead, an SSE stream.** A grep finishes inside one request.
- **Client-side search/sanitization.** The server renderer is the XSS boundary.

## 2. Engine: `git grep` on the serving copy, no index in v1

`internal/git` gains the grep primitive (`Layer.Grep`, `04_git.md` §17). It runs

```
git grep --no-color -n --column -I -z --no-textconv --no-recurse-submodules \
  [-i] [-w] [-F|-E] -m <perFile+1> -e <pattern> <resolved-sha> -- [<pathspec>…]
```

against the **synced serving copy** and parses the NUL-delimited record stream
(`<sha>:<path>\0<line>\0<column>\0<text>\n`; `<text>` excludes the terminator,
so a CRLF line's CR is dropped) incrementally. Properties that
matter for the design:

- **Exact at any commit whose objects are present** — including tags, orphan
  branches and shas that are not a branch tip. `Resolve` produces the sha; the
  primitive never sees a ref name.
- **No worktree, no index file, no write, no lock.** The serving copy is read
  only; search writes nothing to the bucket and takes no push-path round trip
  (law 6).
- **Binary blobs skipped** (`-I`, git's NUL-in-first-8 KiB probe), matching the
  blob page's policy. `--no-textconv` + explicit `-F`/`-E` stop repository
  config from changing what is searched.
- **`LC_ALL=C`** makes case folding deterministic and ASCII-only across glibc
  and musl (§5.3, §12.2).

### 2.1 Measured cost and the deferral triggers

E21 (`internal/devtools/searchbench`, real bare fixtures built by
`git fast-import`, 40 lines/file, host `linux/amd64`, 10 CPUs, git 2.53.0,
page cache warm):

| files | fixed narrow (full tree walk) p50 / p95 | regex narrow p50 / p95 | fixed broad (capped) p50 / p95 |
|---|---|---|---|
| 1,000 | 15.9 ms / 19 ms | 21 ms / 22.2 ms | 8 ms / 8.4 ms |
| 10,000 | 137.9 ms / 141.7 ms | 171 ms / 186 ms | 12.4 ms / 16.5 ms |
| 50,000 | 921.3 ms / 998.9 ms | 936.5 ms / 1.0322 s | 11.7 ms / 12.3 ms |

The shape claims, which are the durable part: **a full-tree query is linear in
file count** (≈10× files ≈ 10× time) and **a capped broad query is flat**
(every cap — matches, bytes, time — bounds the work). Absolute numbers are
this workstation's; a production host with a cold page cache is slower.

**An index starts a new planning doc — not a patch — when any of these holds:**

1. measured p95 query latency exceeds **1.5 s** on the operator's median
   repository (re-measure with E21's harness, not from a suspected case);
2. a repository that must be searchable is **remote-served** (its pack set
   exceeds `cache.max_bytes`, so no local tree exists);
3. **instance-wide code search** becomes a product requirement;
4. a requirement appears for **ranking, symbol/definition search or exact
   match counts over the whole tree**.

An index is not wrong; it is a different consistency model. It must be built
per push (breaking the law-6 push budget) or asynchronously (introducing lag,
which law 4's "no eventually" forbids), it adds a bucket family and a
frozen-contract amendment, and it changes what "the result at sha X" means.
Until a trigger fires, grep is the correct engine.

## 3. Scopes and enumeration

| Scope | v1 | Candidate source | Cost |
|---|---|---|---|
| `repo` | yes | the repo id in the path | refs sync + 1 grep (plus the existing serve materialization when cold) |
| `owner` | yes, bounded | `api.Env.VisibleRepos(ctx, owner, principal)` — the only source | 1 registry listing (LIST + ≤ N manifest HEADs) + 1 `CheckRead` per candidate + ≤ 50 greps |
| `instance` | deferred for code | would be `VisibleOwners` → `VisibleReposByOwner` | N listings; forbidden in v1 |

**Gating is one primitive.** Every candidate passes the existing read gate
(`identity.Service.CheckRead` / `api.Env.VisibleRepos`); a denied repository
contributes **no name, no count, no total and no error text**. There is no
second candidate list, no hand-rolled visibility test and no per-page filter.

### 3.1 Owner scope never materializes a repository

Owner-scope **code** search calls `Sync(LevelRefs)` (cheap) and greps only
repositories whose local pack set is current (`PacksReady`, the read-only
export P2 adds on `wal.RepoHandle`). A repository that is not warm is returned as
`{"status":"not_warm","reason":"not cached on this instance"}`. Rationale: a
50-repo query must never start 50 pack downloads, and the user learns why a
repository is missing instead of seeing a silent short list.

Single-repository search **may** materialize (that is what the user asked for)
under the engine's existing serve-sync bound; its cold/remote states are in §9.

### 3.2 Ordering and deduplication

Results are rendered in candidate order (owner: repository name ascending) and
files in git tree order (path byte order); matches ascend by line. No ranking,
no interleaving, no deduplication across repositories (different paths in
different repositories are different facts).

## 4. Ref selection

- `ref` is optional. Default: the repository's default branch from the summary
  (`RepoView.Head`), then `Resolve` to a sha.
- Malformed ref (leading `-`, control chars/NUL, > 256 bytes) → **400**;
  well-formed but unknown, or an unborn repository → **404** with the existing
  `unknown revision` / `empty repository: …` body.
- Annotated tags peel to the commit sha (`Resolve` already does this).
- The response always carries both `ref` (what was asked; `""` = default) and
  `sha` (what was searched). Every result link uses the **sha**, so a page
  stays internally consistent while the branch moves.
- Multi-branch, fork and unreachable-object search are deferred.

## 5. Query language and caps

### 5.1 Parameters

| Knob | Default | Meaning |
|---|---|---|
| `q` | required | the text or pattern; ≤ 256 bytes; no NUL/CR/LF |
| `regex` | `0` | `1` = POSIX ERE (`-E`), pre-validated with Go `regexp.CompilePOSIX`; `0` = fixed string (`-F`) |
| `case` | `0` | `0` = ASCII case-insensitive (`-i`); `1` = exact bytes |
| `word` | `0` | `1` = whole word (`-w`) |
| `path` | none | repeatable git-default pathspec, ≤ 8 × 256 B; a leading `:`/`!`/`^` is **400** (pathspec magic silently changes the query) |
| `type` | `code` (repo) / `issues` (global) | `code`, `issues`, `all` |
| `ref` | default branch | repository scope only |
| `limit` | 100 | clamped 1..200 (kept matches per repository) |
| `owner` | — | global scope only; **required** in v1 |

PCRE, backreferences and multiline patterns are rejected. The pattern rides
`-e`, so a leading `-` is data, never an option.

### 5.2 Caps (package constants; only two are config)

| Cap | Default | Where |
|---|---|---|
| pattern bytes | 256 | constant |
| path patterns | 8 × 256 B | constant |
| matches per file | 5 (ask 6; the extra record makes truncation observable) | constant |
| matches per repository | 200 | constant (`limit` may lower it) |
| stdout bytes per repository | 2 MiB | constant |
| text per match | 4 KiB, middle-ellipsized around the match column | constant |
| grep timeout | 10 s | constant |
| multi-repo request deadline | 5 s | constant |
| repositories per owner query | 50 | constant |
| concurrent searches / instance | 2 | `[search] max_concurrent` |
| concurrent greps / repository | 1 | package serialization |
| owner fan-out workers | 4 | constant |
| issue threads scanned (body mode) | 50 | constant |
| issue events scanned (body mode) | 500 | constant |

Two host config keys, both operator-only:

```toml
[search]
enabled = true        # *bool; absent/nil = enabled
max_concurrent = 2    # int; 0 = 2
```

`internal/config` rejects unknown keys, so the struct must exist even though no
`internal/setup` schema row is added: these are operator knobs, documented here,
not admin-page fields. (The P2 change that adds the config struct also adds the
row to `11_config_cli.md`, law 12.) Every other cap is a package constant with a
comment; promote one only when an operator actually hits it.

### 5.3 Honest limitations (documented and tested)

- Case-insensitive matching is **ASCII-only** because the child runs with
  `LC_ALL=C` (deliberate: identical behavior on glibc and musl). Non-ASCII case
  variants need `case=1`.
- POSIX character classes (`[[:alpha:]]`) are ASCII under `LC_ALL=C`.
- No ranking; files are path-ordered, matches line-ordered.
- No context lines and no pagination: caps plus a refined query.
- Binary files, submodule gitlinks and `.gitattributes` textconv are
  skipped/ignored.
- Huge trees are scanned until the deadline; the answer says
  `timed_out: true`, never silently partial.
- A pathspec cannot name a file whose name contains a NUL/CR/LF (the query
  grammar rejects it), although such a file can still appear in results.

## 6. JSON API (frozen)

### 6.1 Routes

```
GET /{owner}/{repo}/api/search          GET /{owner}/{repo}/api-browser/search
GET /api/v1/search                      GET /api-browser/v1/search
```

Both lanes share handlers; the canonical route rows live in `internal/api`'s
table with `Expose` (`/{owner}/{repo}/api/search`, `/api/v1/search`), so the
table-derived discovery lists them and the api-browser twins add no entry
(07_api.md §9.13). Every response is `Cache-Control: no-store` (the shared
`cachepolicy.NoStore` value — no-store is unshareable by definition) with no
`ETag`: the result is principal-, ref- and query-dependent, so it must never be
revalidated or shared across principals.

### 6.2 Envelope

Unrequested sections are **omitted**, never `null`; arrays are `[]`, never
`null`. `options` echoes the normalised query so a client can render it.

```json
{
  "query": "func main",
  "type": "code",
  "options": {"regex": false, "case_sensitive": false, "word": false,
               "paths": [], "limit": 100},
  "scope": {"kind": "repo", "owner": "acme", "repo": "app"},
  "code": {
    "complete": true,
    "repos_considered": 1,
    "repos_searched": 1,
    "results": [
      {
        "repo": "acme/app",
        "ref": "main",
        "sha": "4f1c9d2e7a5b3c8d1e0f2a4b6c8d0e2f4a6b8c0d",
        "status": "ok",
        "complete": true,
        "truncated": false,
        "timed_out": false,
        "match_count": 2,
        "file_count": 2,
        "files": [
          {"path": "cmd/app/main.go",
           "matches": [{"line": 12, "column": 6, "text": "func main() {"}]}
        ]
      }
    ]
  }
}
```

Owner scope is the same envelope with `"scope":{"kind":"owner","owner":"acme"}`
and up to 50 `results` entries (`"repo"` distinguishes them; `"ref"` is the
default branch because `ref` is repo-scope-only).

### 6.3 Issue/PR section

```json
"issues": {
  "complete": true,
  "scanned": {"threads": 18, "events": 0, "index_complete": true},
  "hits": [
    {"repo": "acme/app", "num": 42, "kind": "issue", "title": "Crash on empty ref",
     "state": "open", "author": "jane", "labels": ["bug"],
     "updated_at": "2026-11-20T10:11:12Z", "matched": "title",
     "url": "/acme/app/issues/42"}
  ]
}
```

`matched` ∈ `title | body | comment | label | author` and says **where** the hit
was found, so the UI can show a body excerpt only when the body matched. `url`
is derived server-side from `kind` (`/issues/N` vs `/pulls/N`) — clients never
guess. `scanned.index_complete` is false when the shared index was incomplete
and the header scan was used; `scanned.events` is zero for metadata-only
searches.

P4 adds the additive fields decision 14 records: `matched` may also be `state`
or `kind`; a `body`/`comment` hit may carry `excerpt` (clipped by the service,
escaped by the transport); and an incomplete section carries `reason`, while
owner scope carries `repos[]` with the code section's `ok`/`not_searched`/
`failed` vocabulary (`not_warm` is code-only: issue reads need no packs). Every
new field is `omitempty`, so this example is still the complete
single-repository answer.

### 6.4 Completeness fields

| Field | Where | Meaning |
|---|---|---|
| `complete` (section) | code, issues | every requested unit answered fully: `code.complete` = all considered repos are `status:"ok"` and not truncated/timed out; `issues.complete` = the scan covered every candidate (index complete, caps not hit) |
| `complete` (repo result) | each code result | this repository's answer is not capped, timed out or degraded |
| `truncated` | repo result | a match cap or the byte cap fired (per-file truncation folds into this repo-level flag; the wire does not need a per-file flag) |
| `timed_out` | repo result | the grep deadline fired; `files` is partial |
| `status` | repo result | `ok` — searched; `not_warm` — owner scope only, no local pack set; `not_searched` — the request deadline stopped the fan-out before this repo started; `failed` — the grep itself failed (a per-repo reason, never a whole-request 500) |
| `reason` | repo result, non-`ok` | short, neutral text; never contains a path, an error from another principal's repository, or a count of denied repositories |
| `repos_considered` / `repos_searched` | code | candidates after visibility filtering / candidates actually grepped |

There is **no** "N private repositories skipped" message anywhere: a denied
repository is absent from every field, including counts.

## 7. Pages

### 7.1 `GET /{owner}/{repo}/search` (repo page)

Repository chrome + exactly one `h1` ("Search acme/app"), a native
`role="search"` GET form (`q`; type tabs Code/Issues/All; a `<details>`
"Options" holding ref, case, regex, word, path). Results are grouped by file,
each match links to `/{o}/{r}/blob/{sha}/{path}#L{line}`, the matched span is
wrapped in `<mark>` by the server (plain text, escaped), and a `role="status"`
line states the count and every cap ("showing the first 200 matches; refine the
query"). Every non-`ok` repository status renders its own notice. No JavaScript
is required and htmx is not used (a GET form is native). The submit is the
page's one `.btn.primary`.

The gate is the Code page's (`repoGate`/`repoVisible`): an invisible repository
is a 404 page, not a 403 page — the API keeps its 401/403.

### 7.2 `GET /search?q=&owner=` (public page)

Non-repository surface, so `gated()` (`anonymous_read`) applies. It is the
entry point for "search all of @owner". Without `owner` the page does not
guess and does not search instance-wide code: it says v1 needs an owner and
links to the repository picker. Owner-scope results render the same
file-grouped anatomy with the repository name per row.

### 7.3 Repository header box

`partials/repo_header.html` gains a compact search form (GET →
`/{o}/{r}/search`) beside the repository title. Its submit is a plain `.btn`
(the page's own CTA stays the one primary). At ~390 px it wraps below the
title and never pans (`scrollWidth == clientWidth`).

There is **no** masthead/global search box in v1: a header box with no owner
context implies instance-wide code search, which implies the index (§12.1,
§12.6).

## 8. Gating order (normative)

**Single repository**

1. parse the repo id (bad shape → 404, existing behaviour);
2. authenticate (server chain; an invalid credential → 401);
3. `Access.CheckRead(owner, repo, principal)` → 401 anonymous / 403 insufficient
   / 503 identity unavailable — **before** any existence, ref or query work,
   mirroring every `AuthRead` route;
4. validate the query parameters (400) — reachable only by a caller who passed
   the gate;
5. resolve the ref (400 malformed / 404 unknown / 404 `empty repository:`);
6. sync/serve (503 cold or remote, §9);
7. grep (caps, §5);
8. render.

**Owner scope**

1. authenticate;
2. validate the parameters (400) — a bad query never pays a LIST;
3. `VisibleRepos` — the **only** candidate source;
4. per candidate: resolve → refs sync → if `PacksReady`, grep; a failure is a
   per-repository `status`, never a whole-request error (identity failures
   cannot vary inside one principal and remain whole-request).

**Leak rules (each is a test).**

- Denied repositories are absent from results and from every count.
- No 404 where the API convention is 401/403 (the *page* is the 404, as
  everywhere else).
- No `ETag`, no shared cache: `Cache-Control: no-store`.
- Issue bodies/comments are folded through `moderationView` **before** matching,
  so a hidden body cannot be matched, counted or excerpted.
- Match text is rendered through `html/template` escaping; `template.HTML` is
  never used for user bytes.

## 9. Cold and remote-served repositories

- **Single repo:** `Sync(LevelServe)` runs under the engine's serve bound.
  `WalErrTimeout` while warming → **503** + `Retry-After: 5` and the body
  `search is warming this repository; retry in a few seconds`. A serving copy
  that is remote-served → **503**
  `code search needs a local serving copy; this repository's pack set exceeds the cache budget`.
- **Owner scope:** never materializes. A not-warm repository is
  `{"status":"not_warm","reason":"not cached on this instance"}` and the section
  is `complete:false`.
- The page renders those sentences as notices; the search box stays visible
  (the reason is discovered on submit, not guessed).

## 10. Failure mapping

Every row is an HTTP test in P2/P3. Never 500; never a silent empty result for a
failure — `status` is always set.

| Case | Status | Body / field |
|---|---|---|
| `q` missing/empty/too long/NUL/newline | 400 | plain text naming the rule |
| invalid regex (pre-flight `CompilePOSIX`) | 400 | `invalid regular expression: …` |
| regex git rejects at run time (exit ≥ 2 + regcomp stderr) | 400 | `invalid regular expression` |
| pathspec magic / > 8 / > 256 B | 400 | plain text |
| unknown `type` | 400 | plain text |
| malformed ref | 400 | plain text |
| bad repo id | 404 | existing `not found` |
| anonymous, private repo | 401 + `WWW-Authenticate: Bearer` | `authentication required` |
| signed-in, no read role, private | 403 | `read access required` |
| identity store down | 503 | `access check unavailable` |
| unknown repository (well formed, visible) | 404 | existing |
| unknown ref / unborn repo | 404 | `unknown revision` / `empty repository: …` |
| single repo cold, serve bound fired | 503 + `Retry-After: 5` | `search is warming…` |
| remote-served repo | 503 | `code search needs a local serving copy…` |
| grep exit ≥ 2, unexpected | 503 | `search is temporarily unavailable` |
| request deadline fired | 200 | `timed_out:true`, `complete:false`, partial matches |
| repository not warm in owner scope | 200 | `results[].status:"not_warm"` |

The primitive's exit-code contract (`04_git.md` §17.1): `0` = matches, `1` = no
matches (**success**, empty result), `≥ 2` = error. The measured git 2.53
returns `128` for a bad `-e` pattern; the contract is `≥ 2`, not the number the
plan guessed.

## 11. Concurrency and pools

- **Two pools, never one.** `internal/git`'s `Layer.ScanPool` (2 slots) is the
  only pool a grep takes; the control-plane `Layer.Pool` is never used for a
  search. A broad search must not starve clone/upload-pack, and a clone must
  not starve search (`04_git.md` §17.2).
- **Request pool.** `[search] max_concurrent` (2) bounds concurrent searches per
  instance, so a public repository cannot turn a stranger into a CPU monopsony.
- **Per-repository serialization.** Two searches of one repository queue
  (a 1-slot gate keyed by repo id) instead of competing for page cache. A
  scan-pool slot is held **only** while the subprocess runs — never while
  syncing, resolving or reading the store (no lock across I/O).
- **Owner fan-out.** 4 workers over ≤ 50 candidates under a 5 s request
  deadline; each worker takes a scan slot only around its grep. Candidates not
  started are `{"status":"not_searched"}` and the section is `complete:false`.
- **Cancellation.** `exec.CommandContext` SIGKILLs the child on client
  disconnect. The byte cap cancels the same way (the parser asks the runner to
  cancel at 2 MiB), so a runaway match count dies promptly. A caller
  cancellation is an error; the deadline and the caps are fields.
- No cross-caller result sharing in v1: a shared context would let one client's
  disconnect kill another's search. A short-lived result cache keyed by
  `(repo, sha, query, options)` is a follow-up only if E21 shows repeat queries
  matter.

## 12. Decisions & deviations from the Rust design

The Rust specification has no search surface; every decision below is new and
freezes a contract for P2–P6.

1. **No index in v1.** `git grep` on the serving copy is exact at the resolved
   sha, adds zero push-path round trips and zero bucket families, and costs CPU
   only. An index would need either inline push writes (breaking the ≤ 5-request
   budget) or an async builder (introducing lag, against "no eventually"), plus
   a shard format, GC and a frozen-contract amendment. Revisit triggers are
   §2.1; each starts a new planning doc, not a patch.
2. **ASCII case-insensitive default** (`case=0` → `-i` under `LC_ALL=C`).
   Rationale: GitHub-shaped default, deterministic on glibc and musl, no locale
   dependency. The limitation (non-ASCII case variants need `case=1`) is stated
   in the UI options and tested.
3. **POSIX ERE is opt-in** (`regex=1` → `-E`), pre-validated with
   `regexp.CompilePOSIX` (RE2, linear time) and re-classified from git's exit
   code. Rationale: a fixed-string default cannot be a regex-injection or
   catastrophic-backtracking surface; POSIX ERE is the dialect git's own
   `regcomp` implements. PCRE/backreferences stay unavailable.
4. **Issue/PR bodies and comments ship in v1** (operator decision, 2026-11-25,
   widening the card comment). Metadata (title, labels, author, state, kind)
   comes from the existing shared `issues/index.json`; body/comment text is a
   bounded scan (≤ 50 threads / 500 events) folded through `moderationView`
   **before** matching, with `complete:false` beyond the cap. A hidden body can
   never be matched, counted or excerpted.
5. **Two config keys; every cap a constant.** `[search] enabled` (`*bool`, nil =
   enabled) and `[search] max_concurrent` (`int`, 0 → 2; the planning brief
   spelled it `max_concurrency` — `max_concurrent` matches the existing
   `server.max_concurrent_per_repo` vocabulary). No `internal/setup` schema row:
   the values are operator-only and documented. The remaining caps in §5.2 are
   package constants with comments; an operator who hits one gets a new key in a
   later change, not a configuration surface nobody validated.
6. **Repo-header box plus `/search?owner=`; no masthead box.** A global box
   without an owner implies instance-wide code search and therefore the index;
   v1 deliberately has no such promise. The repo page and the owner page are
   the two entry points.
7. **`status:"failed"` joins the frozen vocabulary.** The plan named
   `ok`/`not_warm`/`not_searched`; a per-repository grep failure in owner scope
   also needs a value, and a whole-request 503 would let one broken repository
   hide 49 good answers. `failed` carries a neutral `reason`.
8. **Per-file truncation folds into the repository-level `truncated` flag.**
   Every cap is reported, but the wire does not need a per-file boolean: the
   UI says "some files have more matches" at the repository level. The
   primitive still reports it (`GrepFile.Truncated`) for the service.
9. **Pathspec magic is rejected, and a NUL/CR/LF path cannot be targeted.**
   Leading `:`/`!`/`^` changes the query's meaning silently; a path with a
   control character cannot ride a query parameter safely. Both are 400s; a
   repository may still *report* such paths in results.
10. **A dedicated scan pool is added to `internal/git`** rather than reusing
    the blocking pool: search is long-running CPU, clone/upload-pack is I/O and
    latency-sensitive, and the 2026-08 incidents are what happens when bulk and
    control-plane work share a semaphore.
11. **Request-wide `no-store`.** Search results are principal-, ref- and
    query-dependent; an ETag or a shared cache would leak one principal's
    result to another. `cachepolicy.NoStore` is the only cache class.
12. **The frozen P4 seam** (implemented by `internal/issues`, consumed by
    `internal/search`; field names and semantics are frozen here so P4 can
    start independently):

```go
// IssueSearcher is the P4 seam (the concrete P4 implementation lives in
// internal/search over issues.SearchProjection). The caller has already
// validated the query and resolved the scope; the implementation enforces
// moderation redaction BEFORE matching.
type IssueSearcher interface {
    SearchIssues(ctx context.Context, scope IssueScope, q IssueQuery) (IssueSection, error)
}

type IssueScope struct {
    Owner string // required
    Repo  string // set by the P4 service's visible-repo fan-out; "" is refused
}

type IssueQuery struct {
    Text      string // validated: non-empty, ≤ 256 bytes, no NUL/CR/LF
    CaseExact bool   // false = case-insensitive
    Body      bool   // true = include body/comment text (bounded scan)
    Limit     int    // ≤ 200
}

type IssueSection struct {
    Complete bool
    Scanned  IssueScan
    Hits     []IssueHit // never nil
}

type IssueScan struct {
    Threads       int
    Events        int
    IndexComplete bool
}

type IssueHit struct {
    Repo      string   // "owner/name"
    Num       int
    Kind      string   // "issue" | "pr"
    Title     string
    State     string
    Author    string
    Labels    []string // never nil
    UpdatedAt string   // RFC 3339 UTC
    Matched   string   // "title" | "body" | "comment" | "label" | "author"
    URL       string   // server-derived: /{owner}/{repo}/issues|pulls/{num}
}
```

13. **P2 implementation notes (the frozen design above is unchanged).**
   - `internal/search` owns the seams: `Git` is `git.Layer.Grep` verbatim, so the
     grep's caps and the derived `ScanPool` are the primitive's; `RepoSource`
     lists an owner, resolves a repository ref at serve level, and reports
     refs-only warmth (`Target`, `ready`); `Access` is
     `identity.Service.CheckRead` returning `*auth.AuthError`, so a typed nil can
     never read as "allowed" and the gate's 401/403/503 distinction survives into
     the service's `ErrUnauthorized`/`ErrForbidden`/`ErrUnavailable`. A nil gate
     is fail-closed (503), never "unfiltered".
   - **The 50-candidate cap bounds the candidate SET, not the result list.** The
     owner listing is sorted and truncated to 50 before the per-candidate read
     gates, so the probe count is bounded too; repositories beyond it are not
     considered and appear nowhere (their entries would make the response
     unbounded), and `complete:false` says the set was capped (§1's "honest
     complete:false"). Candidates inside the bound that the deadline never
     started keep `status:"not_searched"`, so the wire is never short within the
     bound.
   - **An unborn repository in owner scope is `not_warm`** with the neutral reason
     "empty repository": the frozen vocabulary (§6.4) has no separate value, and
     `not_warm` is the true statement ("nothing cached here to search"). The
     `ref` field carries the default branch name; `sha` stays empty.
   - **The owner-scope request deadline is observable in two fields**, both
     frozen: a grep the deadline cancelled keeps its partial files with
     `timed_out:true` and `complete:false` (the §10 row), and a candidate the
     deadline never started is `not_searched`.
   - **`PacksReady` required one bookkeeping fix in `internal/wal`.** A fresh
     repository never applies a log delta (HeadSeq 0), so its state `revision`
     stayed 0 while a serve-level materialize set `packs_revision` to the
     manifest revision (1) — `PacksReady` then reported a fully materialized
     repository cold forever, and the publish path's "keep packs_ready" rule
     inherited it. The materialize now sets `revision` alongside
     `packs_revision`, both **forward-only** (`max(state, snapshot)`, because a
     detached timeout-continued body may carry an older manifest than a
     concurrent publish), and the publish keep-ready snapshot is taken under
     `stateMu` with the state write. `PacksReady` itself remains a read-only
     snapshot that never syncs (05_wal_engine.md §5.1.4/§5.2).
   - **§5.2's "no `internal/setup` schema row" and the reflective schema.** The
     schema is generated from the `[search]` struct, so the card appears
     automatically with `enabled` (as a switch, rendered from the effective
     `*bool`) and `max_concurrent`; the page gains a `sectionWhy` line and two
     display notes, and the save path learned to coerce/render/compare pointer
     scalars by value. No per-key validation row was added in `internal/setup`;
     `checkSearch` in `internal/config` is the authority. Both keys are
     restart-required and are listed as such in `11_config_cli.md`.
   - **Issue search is wired as the frozen seam but not yet available.** Until
     P4 binds `internal/issues`, a `type=issues|all` request is
     `ErrIssuesUnavailable` (503-class), never an empty section that would read as
     "nothing matched". The code section is fully functional.

14. **P4 implementation notes (2026-11-25; the frozen field names and their
    meanings above are unchanged).** The concrete `IssueSearcher` lives in
    `internal/search/issue.go` over one additive read-only projection,
    `issues.Service.SearchProjection`. The projection owns the bucket reads
    (the existing `issues/index.json` cards with the existing header-scan
    fallback, the immutable events, `moderationView`'s Snapshot fold) so search
    adds **no object family, no index and no second moderation rule**.
    - **Metadata vs body.** Titles, labels, author, state and kind come from the
      index cards; body/comment text is the bounded scan (§5.2: newest 50
      threads, 500 events per repository). `matched` gains `state` and `kind`
      (the frozen five cannot name where a state/kind match happened), and
      `body`/`comment` are the only values that carry the new `excerpt`.
    - **Redaction is unconditional.** The frozen seam carries no principal, so
      the search fold never takes the moderator's reveal arm: a hidden body is
      omitted before matching for every caller and can never be matched,
      counted or excerpted. A moderation read failure fails the repository's
      section closed (`failed` in owner scope, unavailable in single-repo
      scope), never an empty success.
    - **Owner scope is the code path.** The service enumerates
      `visibleCandidates` (registry listing → 50-name bound → one `CheckRead`
      per candidate) and calls the searcher once per visible repository. The
      concrete searcher refuses `IssueScope.Repo == ""` rather than scanning an
      owner with no gate. Each visible repository has an `IssueRepo`
      status/complete entry (the issue twin of a code `Result`); one failed
      repository is a `failed` status and never hides the others.
    - **Additive wire fields.** `IssueQuery` gains `Regex` and `Word` (so
      `type=all` searches the same grammar in both sections), `IssueHit` gains
      `Excerpt` (deterministically clipped here, escaped by the transport) and
      `IssueSection` gains `Reason` (a typed cap/deadline cause) and `Repos`
      (owner scope). All are `omitempty`, so a complete single-repository answer
      is byte-compatible with the §6.3 example. The concrete searcher and the
      projection are covered by real-store fixtures in both packages.

15. **P5 page implementation notes (the frozen anatomy above is unchanged).**
    - **The masthead is the repository page's one `h1`.** §7.1's "exactly one
      `h1` ("Search acme/app")" would double-count: every repo-scoped page's
      single `h1` is the shared masthead (the repo name), and its body heading is
      an `h2` (a blob's filename, a PR's title). The search page follows that
      convention: `h1` = the masthead, `h2` = "Search owner/repo". The a11y floor
      (exactly one `h1`, no skipped levels) is the rule that wins.
    - **Both page forms default to `type=code` and send the choice explicitly.**
      §5.1's owner-scope default is `issues`; the page picks code because code
      search is what ships in v1 and an unbound issue searcher would make the
      owner page's first search a 503. The tabs still offer Issues/Both, and they
      work the moment P4 binds the seam (until then the rendered notice says so).
    - **Status mapping:** a rejected query renders the page at **400** with the
      form and a `role="alert"` sentence (the form is the recovery path); cold,
      remote-served and disabled repositories render the service's sentence with
      the box visible at **503** (warming carries `Retry-After: 5`); unknown
      repository/ref and the auth classes stay the ordinary error page
      (404/401/403) like every other repo page. No search ever answers 500.
    - **`no-store` implies no ETag.** Both pages set
      `Cache-Control: no-store`; the renderer skips the strong ETag (and
      conditional 304 handling) for that class, because a revalidating 304 would
      be a cross-principal confirmation for a principal-dependent body (§8,
      §12.11). This is a renderer rule, not a search-only branch: the audit
      viewer (also `no-store`) inherits it.
    - **The path filter is one `<textarea>`, one pathspec per line** (the honest
      no-JS way to accept up to eight); repeated `?path=` still decodes, so a
      shared API URL pastes into the page. The owner form carries no `ref`
      control and drops the parameter (ref is repo-scope-only, §5.1).
    - **The matched span is three plain strings, never `template.HTML`.** The
      Go side splits the line into before/mark/after using the match column as a
      hint and falling back to the first occurrence (the text may be
      middle-ellipsized); the template wraps only the middle piece in `<mark>`.
      A span that cannot be located renders the line whole and unmarked — the
      page never fabricates a highlight.
    - **The page set was added to the rendered a11y smoke**
      (`internal/e2e/a11y_test.go`): repo search and owner search join the fixed
      page set at 390 px and desktop.

16. **P6 implementation notes (2026-11-25; the frozen contracts above are
    unchanged).**
    - **The shared `issues/index.json` has two writers, and the pull-side
      mirror is now structurally lossless.** `internal/pulls` rewrites the whole
      document on every PR mutation; its mirror first lacked P4's `card_version`
      (Forgejo #564) and the pin set, so the first PR write stripped the stamp
      and `issues.SearchProjection` reported `index_complete:false` (and
      therefore `issues.complete:false`) forever, while `pinned` would have
      vanished. Adding those two fields alone would only fix the known loss —
      the next additive field, envelope or card, would be stripped again.
      `pulls.Card` and `pulls.Index` therefore implement
      `UnmarshalJSON`/`MarshalJSON` that capture every key the struct does not
      name and re-emit it (known fields stay in struct order, and a document
      without extras is byte-identical), and a re-projected card inherits the
      unknown keys of the row it replaces.
      `internal/pulls/index_roundtrip_test.go` pins an unknown envelope key, an
      unknown issue-card key, an unknown PR-card key through its own write,
      `card_version`/`pinned`, and the no-extras byte shape. Law 5 applies to
      every writer of a shared JSON document, not only its owner.
    - **An issue/PR body is the opened event, seq 0.** The existing moderation
      target spelling `thread:%06x:event:%012x` with seq 0 hides the
      description; the search projection omits it before matching, so the body
      stops matching and no `excerpt` is produced (the e2e proves both on the
      wire). No search-specific moderation verb was needed.
    - **Owner scope is warm immediately after a real push.** The publish path
      carries `packs_ready` to the new manifest revision (decision 13's
      bookkeeping fix), so a `WarmRefs` query answers `ok` without a serve-level
      materialize; the e2e relies on this and would catch a regression.
    - **The shipped no-store header is `no-store`, not `private, no-store`.**
      The one shared class is `cachepolicy.NoStore` (§12.11); no-store already
      forbids every cache, so the intent is unchanged and §6.1/§8 now spell the
      header exactly as the transport sets it.

## 13. Tests (the definition of done, per phase)

**P1 — `internal/git` (this change).** Real bare fixtures: matches, non-matches
(exit 1 = empty success), a pattern git rejects at run time (distinct
`ErrGrepPattern`, never "no matches"), unicode plus the pinned ASCII-folding
limitation, a binary file skipped, a 60 KiB single line (text cap), pathspec
filtering, ref selection, symlink content, a submodule gitlink skipped, a path
containing a colon and a newline, CRLF, no trailing newline, cancellation
(mid-flight and while queued), a deadline against a stub that blocks, the match
cap, the per-file cap, the byte cap, the text cap, a missing binary, and the
argv pinned literally. The scan-pool isolation test saturates the control-plane
pool and still answers. `grep.go` is at 100 % statement coverage; the package
stays ≥ 95 %.

**P2 — service.** Gating matrix (anonymous/public, anonymous/private → 401,
signed-in/no-role/private → 403, read/private → 200, admin → 200); owner scope
omits denied names and counts; ref matrix (default, branch, peeled tag, sha,
unknown → 404, malformed → 400, unborn); cold (warming + `Retry-After`, remote
→ unavailable, owner `not_warm`); completeness for every cap; `-race` with
identical and different concurrent queries; per-repo serialization; a
saturated-git-pool test proving search still answers (separate pools).

**P3 — HTTP/API.** Every §10 row, both lanes, `[]` not null, `no-store`,
discovery lists both templates, plain-text errors, no repository name in a
denial body, `cacheclass_test.go` pinning every cacheable GET.

**P4 — issue/PR search.** Title/label/author/state, PR kind, index-first fast
path, incomplete index → `complete:false`, body/comment mode with a
moderation-hidden body never matched for a non-triage caller, scan caps, `-race`.

**P5 — UI.** Render tests (sha links, `<script>` in a match stays escaped,
`<mark>`, non-`ok` notices), `make a11y` with the search page added to the fixed
set, keyboard pass, no page-level pan at 390 px. *Landed
(`internal/webui/search_test.go`): both pages, the sha+anchor links, `<mark>`,
the query/match path escaping, every completeness flag and per-repository
status, the 401/404 gate parity, `no-store` without an ETag, the repo-header
box, and the form echoing the normalised options; the two pages joined
`internal/e2e/a11y_test.go`'s fixed set (390 px + desktop), which is the
keyboard/pan/one-`h1` floor.*

**P6 — e2e.** Push a repository containing a needle → anonymous search hits with
path/line → flip `access.json` to private → anonymous 401, owner scope omits the
repository, a signed-in reader still hits; assert the push round-trip budget is
unchanged. *Landed (`internal/e2e/search_test.go`, ~10 s, skipped in `-short`):
one real push (source file, README and a NUL-bearing binary carrying the same
needle) then the repository-scope answer (path/line/column, the frozen envelope,
`no-store` without an ETag, the binary skipped, a no-match `complete:true` with
`files:[]`, a 400 bad regex, a 404 unknown ref); an issue body marker and a PR
title marker found by `type=issues` with the right `matched`/`url`; the same
body hidden through the existing moderation PUT (opened event seq 0) and then
**not matched and carrying no excerpt**; a second, private repository absent
from a non-reader's owner answer in name and in `repos_considered`, a real 401
(anonymous) and 403 (signed-in, no role) on its API, the documented 404 page,
and its appearance after the visibility flip; both pages (sha `#L` link,
`<mark>`, the match line's markup escaped, the owner-less entry point); and a
push after the searches that still lands. The push **budget** itself stays the
sim's assertion (`internal/sim/budgets_test.go`): search adds no push-path
write, and the scenario's extra push is the end-to-end half of that claim.*
