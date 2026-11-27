# Command line

> `spindle` is one binary. With no subcommand it **serves**; every other subcommand is either
> **operator-local** (it opens the config, the data dir or the object store itself, so it must run
> next to the bucket) or a **client of the JSON API** (it only speaks HTTP, so it runs anywhere).
> The reference for the bin's own behaviour is [`go/11_config_cli.md`](go/11_config_cli.md);
> what each capability does is [`FEATURES.md`](FEATURES.md).

## 1. The command families

| Kind | Commands | Needs |
|---|---|---|
| **Operator-local** | `serve`, `compact`, `bundle`, `repo`, `access`, `wal`, `synth`, `import`, `mirror`, `config`, `doctor`, `version`, `threads import` | the config file / `SPINDLE__*` env and the data dir (store, cache); `threads import` needs only a repository checkout |
| **JSON-API client** | `auth`, `user`, `org`, `issue`, `pr`, `release`, `checks`, `packages`, `threads` (`list`/`show`/`run`/`cancel`/`rerun`), `file`, `search`, `api` | `--server` and a token; no store access, no data dir |
| **Runner protocol client** | `runner serve` | a runner token and a Docker daemon; no store, no data dir |
| **Shell plumbing** | `completion`, `help` | nothing (the tables are compiled in) |

The split is deliberate: the API client is built for **machine credentials** (static
`[[server.auth.tokens]]` rows never administer), and the CLI exposes no admin commands, so
administration is performed at `/admin` and no CLI command can create or
revoke a runner token. `runner` sits in the client half of the help table, but it speaks the runner
protocol rather than the JSON API, so it needs a runner token instead of a user token. Four surfaces
are still stubs in this build: `synth`, `mirror`, `wal materialize` and `import --direct` answer
`not yet implemented in this build` (exit 1). They stay in the command table, marked `stub` there, and
each answers `-h`/`--help` with its usage and that stub notice — a command that help lists can always
explain itself. `import` is the supported way to bring an existing repository across.

## 2. Global flags and how the target is chosen

Four globals are peeled from **anywhere** in argv (last occurrence wins), so they work before or
after the subcommand:

```
spindle [--config PATH] [--data-dir PATH] [--server URL] [-R OWNER/REPO] <command> [flags] [args]
```

| Flag | Meaning | Environment |
|---|---|---|
| `--config PATH` | the config file to read (an explicitly named file that does not exist is exit 2) | — |
| `--data-dir PATH` | the data dir (store, cache, saved `spindle.toml`); it re-points only the flag-derived paths, so `SPINDLE__*` overrides survive | — |
| `--server URL` | base URL for API commands; a bare host gains `http://`, a path prefix is kept | `SPINDLE_SERVER` |
| `-R OWNER/REPO` | the default repository for API commands that take one | `SPINDLE_REPO` |

**Repository selection** for API commands, in order: the positional `OWNER/REPO` if given, else
`-R OWNER/REPO`, else `SPINDLE_REPO`. A command whose usage line shows `OWNER/REPO` accepts either
form.

**Server selection** for API commands, in order: `--server`, `SPINDLE_SERVER`, the server the last
`auth login` selected, then `http://127.0.0.1:8080`.

**Output.** `--json` prints the endpoint's body unchanged (re-indented; the CLI invents no
envelope), so a field the API grows is a field you can read. `--quiet` suppresses headers and
confirmations; `--limit N` asks for at most N rows on the list commands that page. Everything else
is a human table.

## 3. Authentication

```sh
spindle auth login --server https://spindle.example.com --token-file ~/token
spindle auth login --server https://spindle.example.com --stdin < ~/token
printf '%s' "$SPINDLE_TOKEN" | spindle auth login --server https://spindle.example.com
spindle auth status                                       # server, principal, write/admin — never the token
spindle auth logout [--all]
```

`auth login` verifies the token with `GET /api/v1/me` and stores it in the **OS config dir**:
`~/.config/spindle/auth.json` on Linux (`os.UserConfigDir()` + `/spindle`), overridable with
`SPINDLE_CONFIG_DIR`. The file is written atomically, the directory is `0700`, the file is `0600`,
and the token is **never printed** — `auth status` reports the server, the principal and the
write/admin flags only (and exits 4 when the credential is refused). One entry per server is kept,
keyed by base URL. `--no-verify` stores a token without the round trip; `--token-file F` reads it
from a file, `--stdin` (or just a pipe) from stdin. There is no interactive prompt, so a terminal
with no `--token-file` and no pipe is a usage error (exit 2).

Token resolution for every API call, in order: **`SPINDLE_TOKEN`** (the legacy `WALGIT_TOKEN` is
still accepted as an alias), then the state file entry for the selected server. A 401/403 prints a
hint naming both routes.

Where tokens come from:

- **`wgt_` access tokens** — minted by a signed-in user at **`/_auth/tokens`** (browser session
  surface; the JSON API deliberately exposes no token endpoint, so `auth token list|create|revoke`
  explain that instead of inventing one).
- **Static machine credentials** — `[[server.auth.tokens]]` rows in `spindle.toml`, each with a
  principal, an optional name, an optional `expires_at`, a `write` flag for push, and the secret
  from `token_env` or a literal. They authenticate git, the API and the CLI only; they can never
  administer. The Administration → Access page shows the table and last-use, never the secret.
- **OIDC sessions** for the browser and the UI's own fetches.

## 4. Exit codes (contract)

| Code | Meaning |
|---|---|
| **0** | success |
| **1** | command, config or API error (bad flag value, validation failure, `feature not enabled`, transport failure) |
| **2** | usage error (unknown command, bad argv, a flag missing its value) or a **missing explicitly-named config file** |
| **3** | `config check --strict` observed ignored overrides — and, for API commands, **404** (the named object does not exist) |
| **4** | authentication or authorization failure (401/403) |

The same numbers mean the same thing for every command — scripts branch on them — and the numeric
overlap on 3 is documented, not ambiguous (the two situations belong to different commands).
Validation errors are reported before `--strict`'s exit 3: an invalid config is exit 1 whether or
not `--strict` was passed.

## 5. Command reference

### 5.1 Operator-local

| Command | Usage |
|---|---|
| `serve` | `spindle [--config PATH] [--data-dir PATH]` — run the server (the default with no subcommand) |
| `compact` | `compact [--once] [--base]` — one maintenance compaction pass, or the maintainer loop |
| `bundle run` | `bundle run [--repo ID] [--strategy NAME]` — build bundle slots due now |
| `bundle plan` | `bundle plan REPO [--json]` — print the slot table |
| `bundle compose` | `bundle compose REPO [--strategy NAME]` — build one slot now |
| `bundle rm` | `bundle rm REPO IDS…` — remove bundle entries |
| `repo create` | `repo create REPO [--object-format sha1\|sha256]` |
| `repo list` | `repo list [--json]` (JSONL) |
| `repo info` | `repo info REPO` — manifest stats |
| `repo policy` | `repo policy get REPO` / `repo policy set --file F REPO` / `repo policy clear REPO` — the `policy.json` rule document |
| `repo settings` | `repo settings show [--effective] REPO` / `set --file F [-m MSG] REPO` / `clear REPO` / `history REPO` — the per-repo settings document |
| `access` | `access get REPO` / `access put --file F REPO` — the `access.json` role bindings |
| `wal ls` | `wal ls REPO [--from N] [--to N] [--json]` — log table |
| `wal show` | `wal show REPO SEQ` — one entry, full |
| `wal materialize` | *stub in this build* — `wal materialize REPO --at-seq N --out DIR` would build a standalone repo at a point in time |
| `wal add-pack` | `wal add-pack REPO PACK [--tier N] [--history-of CHECKSUM]` — publish an existing pack as a COMPACT entry |
| `wal annotate-pack` | `wal annotate-pack REPO CHECKSUM [--rev] [--bitmap] [--commit-graph]` |
| `wal rev-index` | `wal rev-index IDX [--out P]` — write a `.rev` from an `.idx` |
| `synth` | *stub in this build* — `synth --out DIR --size s\|m\|l [--seed N]` would generate a deterministic synthetic repo through `git fast-import` |
| `mirror` | *stub in this build* — `mirror --from URL --to URL --dir PATH` would bridge an external repo through a local bare buffer repo |
| `import` | `import [--direct] [--reuse-packs] [--refs GLOB]… --from GITDIR owner/name`, or `import [--ref REF]… [--refs GLOB]… [--default-branch-only] [--include-pull-heads] [--include-notes] [--token-env VAR] [--format sha1\|sha256] [--dangerous] --url URL owner/name` |
| `config check` | `config check [--env-file KEY=VALUE]… [--strict]` — validate file ⊕ env, report ignored overrides |
| `config dump` | `config dump [--env-file KEY=VALUE]` — print the effective config as TOML |
| `doctor` | `doctor [--limit N] [--repos N] [--budget N] [--json]` — read-only upgrade readiness (config + a bounded bucket sample) |
| `version` | `version` — the build identity (linker SHA → VCS revision → `dev`) |

`import` never takes a token on the command line: `--token-env VAR` names the environment variable
holding it. `--dangerous` is the confirmation for a non-allowlisted, non-GitHub source, and
`--direct` (bucket-direct striped import) is not built yet: the flag is parsed and exits 1 with
"not implemented", like the three stub commands above. `spindle import -h` prints its usage; the
stub flag is only reached when it is actually passed.

### 5.2 API client groups

Run `spindle <group> --help` for the group table and `spindle <group> <sub> --help` for that
command's usage line and flags (flags may follow positionals, `gh`-style).

| Group | Subcommands |
|---|---|
| `auth` | `login`, `logout`, `status`, `token list\|create\|revoke` (which explain the browser-only mint) |
| `user` | `whoami`, `list`, `show`, `keys list\|add\|remove` |
| `org` | `list`, `show`, `create`, `member list\|add\|remove`, `team list\|create\|add-repo`, `webhook list\|create\|delete\|ping\|deliveries\|redeliver` |
| `issue` | `list`, `show`, `create`, `comment`, `close`, `reopen` |
| `pr` | `list`, `show`, `diff`, `create`, `close`, `merge`, `review`, `comment`, `checks` |
| `release` | `list`, `show`, `create`, `delete`, `upload` |
| `checks` | `list`, `status REPO [REF]` |
| `packages` | `list`, `show`, `delete` |
| `threads` | `list`, `show`, `jobs`, `run` (`--ref`, repeatable `--input`, `--priority`), `watch`, `cancel`, `rerun`, `schedule list\|enable\|disable\|run`, `pause`, `resume`, `queue`, the operator-local `explain` and `import` (§5.3) |
| `file` | `put`, `create`, `delete` (one text file as one commit — the browser editor's write path) |
| `runner` | `serve` (`--config` offline registration, `--ephemeral`), `drain`, `resume`, `rotate`, `deregister`, `images`, and the named `pool` refusal (see [`threads.md`](threads.md#81-flags-and-environment), §8.3a/§8.3b) |
| `search` | `search QUERY [--owner OWNER \| -R OWNER/REPO] [--ref REF] [--regex] [--case] [--word] [--path PATHSPEC]… [--limit N] [--json]` — code search in one repository (the default branch, or `--ref`) or across an owner's readable repositories. It is a standalone command, not a group: `-R`/`SPINDLE_REPO` selects repository scope, `--owner` owner scope, and exactly one is required (v1 has no instance-wide code search). `--limit` is kept matches per repository (1..200). The command pins `type=code`; issue search has no CLI surface yet. |
| `api` | `api [-X METHOD] [-d DATA] [-H 'K: V'] [-json] PATH` — one authenticated request; `-d` takes a literal, `@FILE` or `-` for stdin |
| `completion` | `completion bash\|zsh\|fish` — generated from the same dispatch table |

### 5.3 `threads import` — the one-way workflow importer

`threads import` is the migration path from a Forgejo or GitHub workflow directory to
`.spindle/workflows/` ([`WORKFLOWS.md` §10](WORKFLOWS.md#10-migrating-from-forgejo)). It lives in the
`threads` group because that is where an operator looks for it, but it is **operator-local**: it
reads a checkout on disk and needs no server, token or data dir — never `--server`, never `-R`.

```
spindle threads import --from forgejo|github --repo DIR
                       [--path PATH] [--out DIR]
                       [--apply [--branch NAME] [--allow-partial]] [--json]
```

| Flag | Meaning |
|---|---|
| `--from forgejo\|github` | the source dialect (required): `.forgejo/workflows` or `.github/workflows` |
| `--repo DIR` | the repository checkout to read (required) |
| `--path PATH` | import one source file or directory instead of the platform's workflow directory; repo-relative or absolute, and it must stay inside the repository |
| `--out DIR` | write the converted files under `DIR` (the same repo-relative layout: `DIR/.spindle/workflows/<name>.yml`) |
| `--apply` | commit the converted files on a **new branch**; mutually exclusive with `--out` |
| `--branch NAME` | the branch name (default `spindle/import-<platform>-<UTC timestamp>`) |
| `--allow-partial` | let `--apply` commit a conversion that has untranslated constructs |
| `--json` | print the machine report instead of the files |

**What it writes.** Nothing in the repository by default: the converted YAML goes to stdout, one
`# ==> .spindle/workflows/<name>.yml (from <source>)` header per file, and the human report goes to
stderr (so a redirect keeps clean YAML). `--out` writes the files under a directory you name.
`--apply` creates the branch (from the current `HEAD`), switches to it, writes
`.spindle/workflows/*.yml`, commits, and stops — leaving you on the new branch. It never pushes,
never forces, refuses a dirty working tree, refuses an existing branch, refuses the current branch,
and refuses to run outside the repository root.

**Output format.** The converted file is a Spindle workflow with a two-line conversion banner at the
top. Source comments are preserved. Every construct the target language cannot express is marked
`# UNTRANSLATED: <path>: <reason>` at its own site, and an untranslatable step or job is replaced by
one that **fails** with the reason — a lossy conversion cannot produce a green run. The file content
carries no timestamp, so converting the same source twice produces the same bytes.

**The `--json` report.**

```json
{
  "platform": "forgejo",
  "repo": ".",
  "applied": false,
  "partial": true,
  "files": [{
    "source": ".forgejo/workflows/release.yml",
    "output": ".spindle/workflows/release.yml",
    "translated": 41,
    "parses": true,
    "untranslated": [{"path": "on.push.paths", "construct": "on.push.paths", "reason": "path filters are not supported; filter inside the job"}],
    "notes": [{"path": "jobs.prepare.runs-on", "construct": "runs-on: docker", "reason": "mapped to the default runner labels [self-hosted, linux, docker]"}]
  }]
}
```

`parses` is always `true` for a file that was produced: every output is validated with the real
parser, and a file that does not parse is an error naming the construct, never a warning. `partial`
is `true` when any file has an untranslated construct.

**Exit codes** (the §4 contract):

| Code | Meaning |
|---|---|
| **0** | every source converted with no untranslated construct (and, with `--apply`, committed) |
| **1** | at least one untranslated construct (the file is still written), or a conversion/git error |
| **2** | usage: a missing `--from`/`--repo`, an unknown platform, `--apply` with `--out`, an unexpected argument |

`--apply` on a partial conversion exits **1** and creates no branch; pass `--allow-partial` when you
have reviewed the named gaps and want the branch anyway.

## 6. Examples

**Clone and push.** HTTP uses the username plus a `wgt_` token as the password (any username works
for a token; the token decides the identity); SSH uses a key registered on **/keys**.

```sh
git clone http://localhost:8080/you/demo.git
git clone ssh://git@localhost:2222/you/demo.git
git push -u origin main
```

**Issues.**

```sh
spindle issue list -R you/demo --state open
spindle issue list you/demo --label bug --limit 20 --json
spindle issue create you/demo --title "Crash on empty repo" \
  --body-file report.md --label bug --assignee ada
spindle issue comment you/demo 42 --body "ssh://… + a token"
```

**Pull requests and checks.**

```sh
spindle pr create you/demo --title "Add the cache layer" --base main --head cache-layer --draft
spindle pr review you/demo 17 --approve --body "LGTM"
spindle pr checks you/demo 17
spindle pr merge you/demo 17 --method squash --delete-head --wait
```

**Releases.**

```sh
spindle release create you/demo v1.2.0 --name "v1.2.0" --notes-file CHANGELOG.md
spindle release upload you/demo v1.2.0 dist/spindle-linux-amd64.tar.gz
spindle release list you/demo --json
```

**Search.** `spindle search` is the code-search client (Feature 20); the human output
prints a per-repository header with its status and match counts, one row per match
(`path:line:column  text`), and always shows the completeness fields; `--json` prints
the API envelope unchanged (`query`, `options`, `scope`,
`code.{complete,repos_considered,repos_searched,results[]}`).

```sh
spindle search "func main" -R you/demo                 # repository scope, default branch
spindle search "TODO" -R you/demo --ref release/1.4 --path internal/ --limit 20
spindle search 'errgroup\..*' --regex --case -R you/demo
spindle search func --owner you                       # every readable repo under you
spindle search func --owner you --json
```

**Packages.** `docker login/push/pull` is the registry; the CLI reads and prunes it.

```sh
docker login spindle.example.com -u <user> -p <wgt_token>
docker push spindle.example.com/you/demo:1.4.2
docker pull spindle.example.com/you/demo@sha256:<digest>

spindle packages list you/demo
spindle packages show you/demo sha256:<digest>
spindle packages delete you/demo --tag 1.4.2
spindle packages delete you/demo sha256:<digest>
```

**Trigger CI.**

```sh
spindle threads list you/demo
spindle threads run you/demo ci --ref main --input mode=fast --priority 5
spindle threads watch you/demo 17          # exit 0 success, 1 failure, 130 cancelled
spindle threads jobs you/demo 17
spindle threads show you/demo 17 --json
spindle threads cancel you/demo 17
spindle threads rerun you/demo 17
spindle threads queue you/demo             # claim order: priority, then FIFO
spindle threads pause you/demo
spindle threads resume you/demo
spindle threads schedule list you/demo
spindle threads schedule disable you/demo --path .spindle/workflows/ci.yml --cron "0 3 * * *"
spindle threads schedule run you/demo --path .spindle/workflows/ci.yml --cron "0 3 * * *"
spindle threads explain .spindle/workflows/ci.yml
```

**Fleet operations.** These use the runner's own credential, so they need no admin session. The
host-admin verbs (mint/list/revoke/policy/group) stay in the browser and the JSON admin lane.

```sh
spindle runner drain --token-file /etc/spindle/runner.token
spindle runner resume --token-file /etc/spindle/runner.token
spindle runner rotate --token-file /etc/spindle/runner.token   # overlap window, no downtime
spindle runner deregister --token-file /etc/spindle/runner.token
spindle runner images                                          # stock toolchain catalogue
```

**Run a CI runner.** The registration token is minted in the browser at `/admin/runners`; the
runner consumes it once, writes the token file, then serves.

```sh
spindle runner serve --server https://spindle.example.com --register wrg_… \
  --token-file /srv/spindle/runner.token --labels self-hosted,linux,docker \
  --capacity 1 --allow-host-docker

spindle runner serve --server https://spindle.example.com \
  --token-file /srv/spindle/runner.token --capacity 1 --allow-host-docker
```

`--allow-host-docker` acknowledges that the daemon at `DOCKER_HOST` exposes the host root
filesystem and is required for the default socket; `--allow-fork-runs` is the separate opt-in for
fork code and belongs only on a disposable host. `SPINDLE_RUNNER_TOKEN` wins over `--token-file`,
and every flag has a `SPINDLE_RUNNER_*` environment twin.

**Config and readiness.**

```sh
spindle config dump                                   # effective config as TOML
spindle config check                                  # validate file ⊕ env (warnings + errors)
spindle config check --env-file SPINDLE__SERVER__LISTEN=:8080 \
                     --env-file SPINDLE__SERVER__LISTEN=:9090 --strict   # exit 3: the first is ignored
spindle doctor --json                                 # upgrade readiness, read-only
```

`--env-file` takes literal `KEY=VALUE` pairs (repeatable), layered under the real environment; a
repeated key keeps the last value and the earlier one counts as **ignored**, which is what
`config check --strict` reports with exit **3**. Overlay keys are **exact paths**
(`SPINDLE__STORE__S3__ENDPOINT`, never `SPINDLE__STORE__ENDPOINT`), and a mistyped path is ignored
silently — confirm the effective value with `spindle config dump` before blaming the server.

**Raw API.**

```sh
spindle api /api/v1/me --json
spindle api -X POST -d @issue.json /you/demo/api/issues
spindle api -H 'Accept: text/event-stream' /you/demo/api/tasks/<id>
```

## 7. Help and completion

```sh
spindle help                    # the command table
spindle help repo               # a family's subcommand table (same as `spindle repo -h`)
spindle <command> --help        # that command's usage line
spindle issue --help            # a group's subcommand table
spindle issue list --help       # that command's usage line and flags
spindle completion bash         # also: zsh, fish
```

A leading `-h`/`--help` (before any command token) prints the global table. After a command token,
help wins over execution: `serve` prints its usage instead of binding a port, `version` prints usage
instead of the build identity, and a stub prints usage instead of the "not yet implemented" error.
Every command and family in the table answers `-h`/`--help`, and `spindle help <topic>` answers the
same way (an unknown topic exits 2 naming it). Completion (bash, zsh, fish) is generated from the
same dispatch table, so a new subcommand completes as soon as it exists.
