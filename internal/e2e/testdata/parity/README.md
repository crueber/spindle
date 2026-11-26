# The Threads conformance corpus (PAR-200)

`docs/features/21_parity_master.md` §1 defines this corpus: byte-for-byte copies
of real workflows, a stamp per file, and the converted file the importer must
produce. `internal/e2e/parity_test.go` is the harness; `make parity` runs it.

## Layout

```
forgejo/
  SOURCE.json                 one stamp per file: repository, path, commit, fetch date,
                              source sha256, golden sha256, importer revision, translation
  <slug>.source.yml           the byte copy, exactly as the repository served it
  <slug>.yml                  the CONVERTED file (the importer's output)
spindle/
  parity-smoke.yml            the execution tier's fixture (corpus-shaped, cheap to run)
```

`<slug>.source.yml` is never edited. If the upstream repository changes, add a
new file with a new stamp; the old one is history.

## Tiers

- **parse/plan** (`TestParityCorpusParsesAndPlans`, runs in `-short`, no docker):
  each converted file must parse, expose the documented jobs and step counts, and
  pass through the engine's real run-creation path (`threads.CreateRun`).
  **All four do, with zero `UNTRANSLATED` constructs** — the corpus is a success
  case, not a tolerance. The mechanism for a future file whose row is still open
  remains, declared per entry: `ExpectUntranslated` (the importer marked the
  construct `# UNTRANSLATED: <path>: <reason>`, so the file parses but the
  construct is gone and named) or `Refusal` (the converted file does not parse,
  with the parser's own message). A `Refusal` is only accepted while the entry's
  dependency row is open, and an entry flips to the strict assertion in the
  change that closes the row — "it does not work yet" cannot hide.
- **trigger semantics** (`TestParityCorpusTriggerSemantics`, `-short`): the
  corpus's real trigger shapes through the engine — a tags-only workflow fires on
  a matching tag and on nothing else (not a branch push, not a non-matching tag),
  a workflow listing both fires on each, and a tag run records `refs/tags/…` with
  no branch.
- **execution** (`TestE2E_ParityExecution`, opt-in: `SPINDLE_PARITY_EXEC=1`,
  `make parity-exec`): a real server plus a real runner execute a corpus-shaped
  workflow to success in containers. The PR gate never runs it.
- **importer** (`TestParityImporterReproducesGoldens`, skipped until
  `internal/threads/importer` exists and skipped in `-short`): runs
  `spindle threads import --from forgejo --repo DIR --path FILE --out DIR` on each
  source and requires the committed golden to be its output, byte for byte, and
  the source to be untouched.

## What the conversion changes (and why the goldens are not the sources)

The importer is a translation, not a copy. For these four files it:

- maps `runs-on: docker` (and `ubuntu-latest`) to the runner label set
  `[self-hosted, linux, docker]`;
- rewrites `needs: <job>` to `needs: [<job>]`;
- rewrites `$GITHUB_SERVER_URL`/`${GITHUB_SERVER_URL}` to the canonical
  `$SPINDLE_SERVER`/`${SPINDLE_SERVER}`, and `${{ github.event.inputs.X }}` to
  `${{ inputs.X }}` (the dispatch-input alias);
- drops the `@v4` ref from `uses: actions/checkout@v4` (the built-in takes no
  version);
- translates `strategy.matrix.include`/`exclude` **verbatim** (the node tree is
  carried, not re-rendered), so `scriobh-release`'s `cli` job keeps its four
  cells;
- has **nothing left to refuse** in this corpus (4/4, zero `UNTRANSLATED`): the
  earlier `matrix.include` marker is gone. A future construct the target cannot
  express still becomes `# UNTRANSLATED: …` at its own site, plus a report entry
  and exit 1 — the mechanism is documented here because it is the rule, not
  because this corpus needs it.

So the corpus proves the parser **and** the converter. `SOURCE.json`'s
`translation` field records the per-file change; `TestParityGoldensMatchTheirStamps`
pins both hashes (a drift must come with a new stamp and an explanation), and
`TestParityImporterReproducesGoldens` proves the pin is the importer's own output.

## The GitHub half

§1 also lists 4–6 real public GitHub workflows. They are **not** here yet: they
belong to the GitHub side of PAR-200, which lands with the importer's
`--from github` mode. Adding them without converted goldens would be a corpus
entry the harness cannot drive, which is the silent gap this file exists to
prevent.
