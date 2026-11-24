# 17 — Browser file edit (Feature F)

> **Status: shipped (2026-11-21).** This is the normative contract for `internal/webedit` and its
> pages. It implements `docs/PLAN-V2.md` §4; where this doc and the plan differ, the difference is in
> §9 (decisions) and the plan gets an "as built" line.

A signed-in principal with push rights edits **one text file** from the browser — create, edit or
delete — and the change lands as **one commit authored by them**, published through the same WAL
funnel a `git push` uses. Policy, the events bridge, webhooks, Threads CI and checks therefore observe
it exactly like a pushed commit, because it *is* a push: one PUSH WAL entry with a pack and a ref txn.

## 1. Scope

**In:** create/edit/delete of an existing or new text file at a path on a branch; direct-to-branch or
new-branch + optional immediate PR; a commit message (subject + extended description); a server-side
diff preview; honest conflict/binary/LFS/size refusals.

**Out (deliberate):** multi-file edits in one commit (GitHub's editor is single-file; a
`git/commits`-style multi-file JSON API is a named follow-up), file upload (binary assets belong to
releases/packages), rename as a first-class action (rename = delete + create, two commits), directory
creation as its own action (implicit in a nested path), and editing LFS pointers (refused, not
surgered).

## 2. Data model

**None.** The commit is git state, the branch is a ref, and the optional PR is the shipped `pulls`
state. This feature introduces **zero new bucket families and zero migrations**; the only object it
reads is the repo's existing `repos/<o>/<r>/policy.json`.

## 3. API (both lanes)

Repo-scoped, discovered via `ExposedTemplates` (`/{owner}/{repo}/api/contents`). The `/api-browser`
twin is identical. Errors are plain text and every write is `no-store`.

| Method + path | Auth | Request → response |
|---|---|---|
| `PUT /{o}/{r}/api/contents?ref=&path=` | push floor (§6) + policy | `{message, description?, content:base64, expected_blob_sha?, expected_branch_sha?, new_branch?, open_pr?, pr_title?, pr_body?}` → `201 {commit:{sha}, branch, pr?:{num}}` |
| `POST …/api/contents` | same | create-new-file variant: `expected_blob_sha` **forbidden**, `409` when the path exists |
| `DELETE …/api/contents` | same | `{message, expected_blob_sha (required), expected_branch_sha?, new_branch?, open_pr?}` → `200 {commit:{sha}, branch}`; `404` when the path is absent |

`PUT` is edit-or-create (an absent `expected_blob_sha` on an existing path is a `409`, never a blind
overwrite). A `Location` header names the landed file, the new branch's root for a delete, or the PR
when one opened.

Status matrix: `400` bad path/message/base64/charset; `401` anonymous; `403` role/policy; `404`
unknown ref or absent path; `409` branch or blob moved (both shas in the body), or a create on an
existing path; `413` over `webedit.max_bytes`; `415` binary content or an LFS target; `422` no-op;
`503` backend unavailable.

Path validation is `git.ValidateRefName`-grade: UTF-8, ≤ 512 bytes, no leading/trailing `/`, no
empty/`.`/`..`/`.git` segment, no control bytes, no backslash, no leading `-`. Messages are trimmed to
non-empty and capped at 64 KiB (subject + description). `new_branch` is a `refs/heads/<name>`-grade
name; `open_pr` requires `new_branch` (400 otherwise) and opens `ref` → `new_branch`.

## 4. Guardrails (server-side, never only in the UI)

- **Binary:** a NUL byte in the first 8 000 bytes of the submitted content → `415`; an existing
  binary blob → `415` for edit and delete. (Same sniff the blob page uses.)
- **LFS pointer:** a blob body starting with `version https://git-lfs.github.com/spec/v1` is refused
  for edit/delete, and a path the **root `.gitattributes`** marks `filter=lfs` is refused for
  create/edit. The matcher is a documented minimal subset: the first token is the pattern, and one of
  the remaining tokens must be exactly `filter=lfs`; supported patterns are exact paths, `*.ext`
  basenames, `dir/**`, `*`/`**`. Negations, quoted patterns and macros are ignored.
- **Size:** `webedit.max_bytes` (reflective config, default 1 MiB; setup row) caps the submitted
  content (`413`) and the editor page (the file renders a refusal card linking to raw). Deleting a
  blob too large to sniff is refused rather than skipped — the guard must run.
- **Stale base:** `expected_branch_sha` (the tip the form rendered from) and `expected_blob_sha` (the
  file's current blob) are compared before any write. A mismatch is a `409` whose body names both
  shas (`branch moved under you (page: <a>, disk: <b>)` / `file changed under you (disk: <b>)`). The
  publish CAS is the correctness backstop; the precondition is the honest UX. There is no blind
  overwrite and no force path.
- **No-op:** an unchanged tree is `422 nothing to commit` (plumbing `diff-tree --quiet`).
- **Delete semantics:** deleting the last file in a directory removes the directory entry; deleting
  the repo's only file yields a valid empty tree and is allowed (not refused).

## 5. The write path (the funnel)

One synchronous request (the tags precedent — seconds of pool-bounded plumbing, not a task). Steps:

1. **Gate before any git/store work:** require_read, anonymous → 401, then `identity.CheckPush`
   — the exact push rule (host `write` flag stripped, self-namespace rule applied). No
   `CheckRole(write)` shortcut: a host-wide write flag alone cannot edit someone else's repo.
   Then the repo's existing `policy.json` is evaluated with **all push effects** (`protect` and the
   `required-reviews` push half) for `(principal, target ref, op)`; a deny is `403 rejected by rule
   '<name>'`. An unparseable policy fails closed. Direct edits target `refs/heads/<ref>` (`update`),
   proposed edits target the new branch (`create`).
2. **Resolve:** one serve-level refs sync, then the base commit for `refs/heads/<ref>`.
3. **Build the commit** with plumbing only, a request-scoped temporary index, no worktree and no
   branch movement: `hash-object -w --stdin` → `read-tree` → `update-index --add --cacheinfo` (or
   `--force-remove`) → `write-tree` → `diff-tree --quiet` → `commit-tree`. Author = principal,
   committer = `spindle <spindle@localhost>`, same wall clock. The argv are pinned in
   `docs/go/04_git.md` §15.
4. **Pack and publish:** `pack-objects --revs --stdout` over `<tip>` with every existing ref negated
   → `Layer.Ingest` → one WAL PUSH entry (`RefPublisher.UpdateRefWithPack`), old = the tip for a
   direct edit or zero for a branch create. The publisher wakes the events bridge and runs the **same
   on-push fan-out** as a git push (push mirrors + Threads run creation).
5. **Optional PR:** the new branch is published first, then the shipped `pulls` opener is called
   through the consumer-owned `PROpener` seam (§7). If the PR fails, the response is `503` naming the
   created branch — the commit is real and a retry would 409.

Round trips: the push budget (refs sync + manifest conditional GET + pack PUT + manifest CAS ≤ 5 store
requests), plus one policy GET when a policy exists.

### Concurrency & cost

No locks and no goroutines of its own. The temporary index is unique per request (pid+nanos, removed
on every exit path). Two editors racing one branch are arbitrated by the publish CAS: the loser gets
`409` with both shas. Git spawns are sequential (dependent), pool-bounded and request-cancellable;
a file edit is not a task, creates no new task kind, event name or cursor family, and emits no SSE of
its own — the push event lands on the existing streams.

## 6. Permissions

| Action | anonymous | read | triage | write | maintain | admin |
|---|---|---|---|---|---|---|
| View editor pages / blob + tree | per visibility | ✓ | ✓ | ✓ | ✓ | ✓ |
| Create/edit/delete (commit) | 401 | 403 | 403 | ✓ (+ policy) | ✓ | ✓ |
| New branch + PR | 401 | 403 | 403 | ✓ | ✓ | ✓ |

The floor is the **push** floor. Moderation blocks deliberately do not gate this feature (blocks
govern conversation, not git-equivalent writes).

## 7. The PR opener seam

`internal/webedit` defines the consumer-owned interface; composition wires `*pulls.Service.OpenPR`:

```go
type PROpener interface {
    OpenPR(ctx context.Context, owner, repo string, in OpenInput, actor auth.Principal) (num int, err error)
}
```

`OpenInput` carries title/body defaults (`pr_title`/`pr_body`, else `Edit <path>` and the edit's
description). The PR opens exactly as a human's `POST …/pulls` would, gates included; `webedit` never
imports `pulls`.

## 8. UI

Pages in the repo group (`internal/webui/edit.go`, `templates/pages/repo/editor.html`):

| Route | Page |
|---|---|
| `/{o}/{r}/new/{branch}` (path prefilled by `?path=`) | create |
| `/{o}/{r}/edit/{branch}/{path…}` | edit (prefilled textarea; blob sha + branch shown) |
| `/{o}/{r}/delete/{branch}/{path…}` | delete (typed-path confirm via `data-danger-expected`) |
| `POST /{o}/{r}/contents` | the one form write (`action=preview` renders the diff, writes nothing) |

The blob page gains `Edit` + `Delete` next to `raw`; the tree gains `Add file`. All three are gated
by the same push rule and route anonymous viewers through the login interstitial
(`collabWriteHref`). Binary/LFS/oversize files show a reason instead of an edit link. The form has a
commit message + description, a branch `<select>`, an optional new-branch input, an `open_pr`
checkbox, and `Preview changes` (secondary) beside the one `.primary` submit. A conflict re-renders
the editor with a banner naming both shas and keeps the typed content; guardrail errors map to their
status with the text preserved. Form text is normalized CRLF→LF (git convention); the JSON API carries
exact bytes (base64), so the two lanes differ only in what a browser does to a textarea. Single column
at ~390 px, textarea and selects full width, no page-level horizontal pan.

## 9. Decisions (as built)

- **`commit-tree` via plumbing, not a worktree**; `diff-tree --quiet` is the no-op guard (the plan's
  `diff-index` prose is superseded — see `docs/go/04_git.md` Decisions) and the temporary index gets an
  empty `GIT_WORK_TREE` because `update-index --force-remove` refuses to run without one.
- **`pack-objects --revs --stdout` with caret-negated refs on stdin** (the merge-task precedent);
  `pack-objects` has no `--not/--all` flags.
- **Author email**: the verified `Principal.Email` when present; else the principal name itself when
  it is already an email (static tokens name principals by email); else `<name>@spindle.local`.
- **A failed PR open after a successful branch publish is a `503` naming the branch**, not a silent
  partial success and not a bogus 409 on retry.
- **Create-file (`POST`) on an existing path is `409`**, including the current blob sha — the call
  cannot overwrite. Editing an existing file therefore uses `PUT` with `expected_blob_sha`.
- **Rename** is delete + create (two commits). **Multi-file** editing is out of scope.
- **The edit page requires the push floor to render at all** (a read-only principal gets 403; an
  anonymous viewer in token/oidc mode gets the login interstitial). The service re-gates every write.

## 10. Tests

- `internal/webedit`: validation tables (path/message/branch), binary/NUL sniff, LFS pointer and the
  `.gitattributes` subset, no-op, preconditions (branch/blob/create-exists), policy refusal with a
  bypass arm, the role/push gate (flag-stripped), direct/new-branch/PR modes, both-lane httptest
  matrices (400/401/403/404/409/413/415/422), and real-git plumbing (`commit-tree` identity and
  parent, nested-tree rebuild, delete removes the directory entry, pack shape).
- `internal/webui`: page shape for the three modes, the blob/tree affordances, the gates
  (interstitial/403/503), the conflict re-render, CRLF normalization, and the preview.
- `internal/e2e`: direct edit → webhook ref event + Threads push run; stale base → 409; a
  `required-reviews` direct edit → 403 while the propose-branch path opens a PR; new branch + PR →
  merge; two racing editors → one 201 and one 409. Real git confirms the commit, tree, ref and author.

## 11. SDK and CLI

- **SDK** (`web/sdk/src/contents.js`, attached as `repo.contents`): `put(ref, path, fields)`,
  `create(ref, path, fields)`, `remove(ref, path, fields)` — thin `_call` wrappers over the same
  three verbs, with `?ref=&path=` encoded by `URLSearchParams`. The SDK never retries a 409: the
  caller re-reads and re-submits with fresh `expected_*` values.
- **CLI** (`cmd/spindle/cli_file.go`): `spindle file put|create|delete OWNER/REPO PATH --branch REF
  --message MSG` (`--content`/`--content-file -`, `--expect-blob`, `--expect-branch`, `--new-branch`,
  `--open-pr`, `--json`, `--quiet`). Delete requires `--expect-blob` (the typed-confirm equivalent).
  The verbs are a second client of the same endpoint — never a second writer — so exit codes map
  the API's (409 → exit 1 with the server's message, 401/403 → 4, 404 → 3).
