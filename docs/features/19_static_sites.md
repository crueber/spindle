# 19 — Static sites (a dedicated static host)

**Status:** in progress (2026-11-24). **Card:** `01a20debaba100000050d8b0c78436` (Spindle board).

## 1. What it is

A repository can be published as a static site, GitHub-Pages-like but served from one
**dedicated static hostname** instead of per-owner subdomains:

```
https://<static-host>/<owner>/<repo>/              → that repo's default-ref index.html
https://<static-host>/<owner>/<repo>/docs/         → docs/index.html
https://<static-host>/<owner>/<repo>/assets/app.css→ text/css
```

Read-only bytes already in the repository. There is **no build step**, no template engine,
no per-site configuration file, and no server-side redirects: what is committed is what is
served. A future CI job can produce a static tree; spindle will not learn Jekyll.

The site is served by a **second HTTP listener** bound to its own address and answering only
for its configured host. It never mounts the application's session middleware, CORS, page
provider or API routes.

## 2. Why a separate listener and a separate host (the security decision)

Serving repository HTML on the application origin is account takeover: same-origin script can
`fetch('/…/api/…', {credentials:'include'})` with the visitor's session, read the app's pages
for CSRF tokens, and read non-HttpOnly storage. `SameSite` does not help — the request is
same-site *and* same-origin, the exact case those attributes allow.

**Ports do not isolate cookies** (RFC 6265 cookies are host-scoped). The isolation therefore
comes from the **hostname**, not from the port: the app and the sites are different origins and
different cookie jars. The port is merely where the second listener binds.

Consequences, all deliberate:

- The static listener is **cookie-less**: it sets no cookies and reads none. It therefore serves
  **public repositories only**; a repo whose visibility is not `public` is a plain **404**
  (existence is not leaked, and there is no login redirect loop).
- Residual risk: user pages on a sibling subdomain can still *set* a `Domain=.parent` cookie that
  the app host receives (cookie tossing). Session values are signed and verified, so the impact is
  bounded to a forced logout / cookie confusion, not forgery. Recorded here so nobody believes the
  host split is total. The durable hardening is a `__Host-` cookie prefix app-side; that is a
  separate change.
- The static listener answers **only** when `Host` equals `static_sites.host` (port-insensitive,
  case-insensitive). Any other Host is refused with 404 and no body, for every method — a
  non-GET under a foreign Host is that same bodiless 404, not a 405. This stops the listener from
  being reached under the app's hostname through a misconfigured proxy. The comparison is an exact
  string match after lowercasing and port removal: a bracketed IPv6 literal keeps its address
  (`[::1]:8081` matches a configured `[::1]`), a trailing-dot FQDN (`static.localhost.`) does **not**
  match `static.localhost`, and neither does a different spelling of the same IPv6 address
  (`[0:0:0:0:0:0:0:1]`) — there is no DNS or address canonicalisation.

## 3. URL → file resolution

Per request, after the host check, the path is split as `/{owner}/{repo}/{rest}`. `owner` and
`repo` are validated with the repository-id rules; `rest` is normalised (percent-decoded,
duplicate slashes collapsed, `.` segments dropped) and **rejected** if it contains `..`, a
backslash, NUL, a control byte, or an empty segment after normalisation. Traversal is refused
before any ref or blob read.

Resolution order (mirrors GitHub Pages, measured 2026-11-24):

| Request | Result |
|---|---|
| `/{o}/{r}` (no trailing slash) | `301` to `/{o}/{r}/` (query preserved) |
| `/{o}/{r}/` | `rest` = `""` → index resolution at the site root |
| `…/x/` where `x` is a tree | index resolution inside `x` |
| `…/x` where `x` is a tree | `301` to `…/x/` |
| `…/x` where `x` is a blob | serve its bytes |
| `…/x` where `x` is missing but `x.html` is a blob | serve `x.html` with **200** (GitHub's undocumented fallback; generators rely on it) |
| `…/x/` where `x` is a blob | `404` |
| anything else | the site's `404.html`, status **404** |

Details:

- **Index resolution** tries `static_sites.index_files` (`index.html`, `index.htm` by default) in
  order inside the target directory. A directory without an index is a 404 (never a listing).
- **`404.html`** is looked up at the site root (after the per-repo `root`, §5), served with status
  **404** and `Content-Type: text/html; charset=utf-8`. Absent, a one-line plain-text 404 is
  returned. There is no SPA rewrite: `/x/y` never falls back to `index.html`.
- **Dotfiles never serve.** Any path segment starting with `.` is a 404 (GitHub's artifact default).
  `.well-known/` is not special-cased today; if ACME/`security.txt` is wanted it becomes a
  documented exception, not an accident.
- **Paths are case-sensitive**, as git is.
- Symlinks (mode `120000`) and gitlinks (mode `160000`) are **never** served — a symlink's blob
  content is the link target text, which would be a trap. Both are 404 (with the site's `404.html`
  body, since the entry was already looked up).
- The site root is the repository root; `static_site.root` narrows it to a subdirectory for
  sites published from, e.g., `public/` or `docs/`.

## 4. Response semantics

- `Content-Type` comes from a **pinned** extension table owed by this package (`text/html`,
  `text/css`, `text/javascript`, `application/json`, `image/svg+xml`, `font/woff2`, `image/*`,
  `application/wasm`, …), with `; charset=utf-8` on `text/*` and `application/json`. Unknown
  extensions are `application/octet-stream`. Never sniff.
- `X-Content-Type-Options: nosniff` on every response.
- `ETag` is the blob's git object id, strong and quoted. `If-None-Match` → **304** with the same
  ETag and no body. There is no `Last-Modified` in v1 (a commit timestamp would cost an extra
  subprocess on every request; the strong ETag is sufficient for revalidation).
- `Cache-Control: public, max-age=600` on 200s — GitHub's value, short enough that a fixed 404 or a
  stale asset is visible within ten minutes. **404s and 301s carry no cache headers.**
- `Accept-Ranges: bytes`; a single `Range` → `206` with `Content-Range`; past EOF → `416`; `HEAD`
  mirrors `GET` without a body. `http.ServeContent` provides these; the handler only supplies the
  bytes, the ETag and a zero modtime.
- No CORS headers, no app CSP, no `Set-Cookie`, no `X-Frame-Options` on user content (a user site is
  allowed to frame itself; the app origin is a different host and keeps its own policy).

## 5. Configuration

Global (`spindle.toml`):

```toml
[static_sites]
enabled        = false            # off by default; the listener is not started when false
address        = "127.0.0.1:8081" # its own listener; must differ from server.listen
host           = "static.localhost" # the ONLY Host this listener answers for (port-insensitive)
max_file_bytes = "8MiB"           # per-file cap (buffered read path); larger files 404 in v1
index_files    = ["index.html", "index.htm"]
```

Per repo (the WAL settings document, `[static_site]`):

```toml
[static_site]
enabled = true       # *bool: absent/false = not published
ref     = ""         # "" = the repository's default branch tip
root    = ""         # "" = repository root
```

Validation (fail closed), with `enabled` on:

- `host` non-empty, and **not the application's own hostname** — neither `server.public_url`'s
  hostname nor a real (non-wildcard) `server.listen` host. Serving user HTML on the app origin is
  the account-takeover case §2 forbids, so a config that would allow it is refused by name.
- `address` parseable, different from `server.listen`, and different from that listener's IPv6
  loopback twin (`[::1]:<port>`), which the server also binds for `*.localhost`.
- `max_file_bytes > 0`, with a **64 MiB ceiling**: each request buffers one file, bounded by the
  listener's pool, so an unbounded value is an OOM knob.
- `index_files` entries are clean relative names (no `/`, no `..`, no dot-leading file).
- Per repo: `ref` is a valid git ref name (same rule as the settings validator), and `root` matches
  the handler exactly — relative, no trailing slash, no backslash or control bytes, no empty
  segment, no `.`/`..`, no dot-leading segment, at most 512 bytes.

## 6. Interfaces (package `internal/staticpages`)

The package imports `git`, `auth`, `config` and stdlib only — never `internal/server` or
`internal/webui` (law 8). Composition supplies the seams:

```go
type RepoDirs interface {
    Dir(ctx context.Context, repo string) (string, error) // synced serving copy, guard released
}

type Entry struct {
    Mode string // "100644", "100755", "120000", "040000", "160000"
    Type string // "blob", "tree", "commit"
    OID  string
    Size int64
}

type GitRunner interface {
    Resolve(ctx context.Context, dir, ref string) (commitOID string, err error) // ref "" = HEAD
    Lookup(ctx context.Context, dir, treeish, path string) (Entry, bool, error) // ls-tree -z -l -- <treeish> <path>
    ReadBlob(ctx context.Context, dir, oid string, w io.Writer) error          // cat-file blob <oid>
}

type SiteSettings struct {
    Enabled bool
    Ref     string
    Root    string
}

type Settings interface {
    Site(ctx context.Context, owner, repo string) (SiteSettings, error) // fail-open: error = not published
}

type Visible interface {
    CanRead(ctx context.Context, owner, repo string) (bool, error) // anonymous CheckRead; false ⇒ 404
}

type Options struct {
    Host        string
    MaxFileBytes int64
    IndexFiles  []string
    Dirs        RepoDirs
    Git         GitRunner
    Settings    Settings
    Visible     Visible
    Logger      *slog.Logger // nil = slog.Default(); one record per 503 (see §7)
}

func New(opts Options) *Handler
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

`GitRunner` is implemented in `internal/staticpages/git.go` over `os/exec` with its own bounded
pool (4 × `GOMAXPROCS`) and per-command timeout, modelled on `internal/archive` so bulk reads never
share a semaphore with control-plane work (13 §7). `exec.CommandContext` is bound to the request
context so a client disconnect kills the child.

## 7. Request flow (subprocess budget)

```
1. Host check (static_sites.host)                     — no store/subprocess; 404 for every method
2. method GET/HEAD only (405 + Allow otherwise)
3. parse + validate {owner}/{repo}/{rest}             — no store/subprocess
4. repo visibility gate (anonymous)                   — warm: 0–1 store read
5. per-repo [static_site] settings                    — warm: 0–1 store read
6. RepoDirs.Dir → Sync(LevelServe), guard released    — warm: 0 store round trips
7. Git.Resolve(ref or HEAD) → commit oid              — 1 subprocess (0 with a warm ref cache)
8. Git.Lookup(commit, path) → Entry                   — 1 subprocess
   miss → Lookup(path+".html") → 1 subprocess (failure path only)
9. ETag = entry.OID; If-None-Match match → 304        — no content read
10. cap → ReadBlob(oid) into a buffer ≤ max_file_bytes — 1 subprocess
11. ServeContent(bytes, ETag, zero modtime)           — Range/HEAD/304 handled by stdlib
```

The bare `/{owner}/{repo}` → `/{owner}/{repo}/` 301 is answered **before** step 6: after the
visibility gate and the per-repo settings, it needs no serving copy and no ref, so a redirect costs
zero subprocesses and zero store reads. The slashed form carries the honest 404/503 for an
unresolvable ref.

Happy path: **3 git subprocesses** (resolve, lookup, blob) and no store round trips when warm;
a 304 costs **2** (resolve, lookup). No LIST: every step is an exact-key probe.

Two deliberate details:

- `Lookup` runs `git ls-tree -z -l -- <treeish> <path>` and requires the returned name to equal the
  request exactly. `<treeish>:<path>` cannot express "this one entry": for a tree it lists that
  tree's children, and for a blob it fails with `fatal: not a tree object`. The separate `<path>`
  argument also neutralises pathspec glob substitution, and the exact-name check makes a glob match
  impossible even if git expands one.
- A path rejected for traversal or a dotfile segment gets a **plain 404**, never the site's
  `404.html` — those rejections must cost zero git calls, and a `404.html` lookup is a git call. An
  entry that *exists* but is not servable (symlink, gitlink, oversized) has already cost the lookup,
  so it renders the site's `404.html` like any other miss. An unreadable or oversized `404.html`
  degrades to the plain-text 404 rather than a 503.
- Infrastructure failures (sync, subprocess spawn) are **503**; missing refs, paths and objects are
  **404**. The 404 rules above are the only user-visible contract. Only "this name does not resolve"
  is a 404 from `rev-parse`: git's unknown-revision diagnostics are a miss, while a corrupt object, a
  broken repository or a missing binary is a 503 (`Lookup`/`ReadBlob` failures are always 503).
- Every 503 logs **one** record — repository, operation (`dir`/`resolve`/`lookup`/`read`) and the
  error, which carries the git argv and stderr — through `Options.Logger` (default `slog.Default()`).
  A 4xx and the happy path log nothing; the log line is the only place the subprocess detail surfaces.

## 8. Concurrency

Hazard: a slow client plus a spawned `cat-file` pins a goroutine and a pool slot; a disconnect
leaks the child; a shared pool with control-plane traffic starves git-over-HTTP.
Avoidance: the package owns its pool; every command is `exec.CommandContext(r.Context(), …)` with
a timeout; the `rw` read guard from `Sync` is released before any git subprocess and before
streaming; file bodies are bounded by `max_file_bytes`. The body buffer starts **nil** and grows as
the reader writes, so a request queued for a pool slot holds no body memory and only the reads
actually running (at most one per slot) allocate — a blob's declared size is never trusted enough
to preallocate from it.

## 9. Tests

- Table-driven `httptest` over a fake `GitRunner`/`Settings`/`Visible`: the full resolution table in
  §3 (including the `.html` fallback and the trailing-slash-on-a-file case), 304 on both the
  matching and non-matching ETag, `Range` 206/416, `HEAD`, `405`, host mismatch (in both orders:
  a non-GET under a foreign Host is the bodiless 404, never a 405 with `Allow`), private repo → 404,
  dotfile and traversal tables, symlink/gitlink refusal, MIME table.
- Isolation assertions on the rendered responses (§2/§4): no `Set-Cookie`, no `Access-Control-*`, no
  `X-Frame-Options` and no CSP on a 200, 304, 404 or 301. IPv6-literal and trailing-dot Host
  spellings (accepted and refused respectively), `HEAD` + `If-None-Match` → 304 with no body and no
  `Content-Length`, an oversized `404.html` degrading to plain text, the lazy body buffer, and the
  one-log-record-per-503 rule (with 4xx and 200 staying silent).
- One real-git fixture test (temp repo + `index.html`, nested dirs, `404.html`) exercising
  `internal/staticpages/git.go` argv exactly, plus real-repository failure shapes: an unknown ref is
  a 404, while a non-repository, a missing serving copy and a corrupt loose object are 503s.
- A cache-class contract test pinning `public, max-age=600` on 200s and no cache headers on 404/301.
- Config tests: defaults, `enabled` without host, `address == server.listen`, bad `ref`/`root`.
- e2e (`internal/e2e`): push a repo with `index.html` + `assets/app.css`; `GET` the index, the asset,
  a directory, a 404, a 304, a 206; a private repo 404s; `POST` 405s. Drive the static host (the
  app's canonical-host redirect does not apply on this listener).
- `make cover`: the new package must clear ≥ 95 %.

## 10. Non-goals (v1)

Subdomain-per-site, custom domains/`CNAME`, per-site build pipelines (Jekyll/Actions), `_headers` /
`_redirects`, HTTPS automation, analytics, authenticated/private sites, directory listings, and
compression negotiation (the bytes are served as committed).
