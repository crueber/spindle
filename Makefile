# spindle — Make targets per 15_testing.md §7 / 16_packaging.md §7.2 (Make is the only task runner).
GO      ?= go
BINARY  ?= spindle
SHA     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.buildSHA=$(SHA)
T5  := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 300" || echo "")
T15 := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 900" || echo "")
# The race/cover tiers use Go's own per-package ceiling (10m, now stated
# explicitly) and keep an outer watchdog for a whole-run hang. The tier once
# raised the package ceiling to 20m because internal/webui -- a render suite
# that owns no shared state -- took 543s under -race; it now takes ~12s
# (docs/go/15_testing.md §6.7), so the default is the ceiling again. The
# slowest package in the full tier is internal/e2e at ~132s on a 10-core host,
# ~4.5x under the ceiling.
T30 := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 1800" || echo "")
# S3 contract target: the rustfs rig defaults (compose.yaml), overridable from
# the environment so CI can point at a service container by hostname.
SPINDLE_TEST_S3_ENDPOINT   ?= http://127.0.0.1:9000
SPINDLE_TEST_S3_ACCESS_KEY ?= walgit-dev
SPINDLE_TEST_S3_SECRET_KEY ?= walgit-dev-secret
SPINDLE_TEST_S3_BUCKET     ?= spindle-test

.DEFAULT_GOAL := help

web: ## build the page assets: vite/Tailwind (app.css) + esbuild (islands, SDK) + vendored htmx
	pnpm --dir web run build
	@# The htmx SSE extension is not our transport and was removed (D-WEB-14). A
	@# stale ignored dist/htmx-ext-sse.js must never ship: build:vendor deletes
	@# it, and this guard fails the build if it somehow reappears.
	@test ! -e web/dist/htmx-ext-sse.js \
	  || { echo "ERROR: stale web/dist/htmx-ext-sse.js — the htmx SSE extension must not ship (rm it and rebuild)." >&2; exit 1; }
	@# Tailwind only emits utilities for what `@source` resolves to (web/src/app.css).
	@# An empty scan still writes a valid stylesheet, so assert the template utilities
	@# actually landed (the Dockerfile repeats this check for the image build).
	@grep -q 'max-w-6xl' web/dist/app.css && grep -q '\.flex{' web/dist/app.css \
	  || { echo "ERROR: web/dist/app.css has no utility classes — Tailwind scanned no templates; check the @source paths in web/src/app.css." >&2; exit 1; }

build: web ## compile everything (the SDK bundle is a build dependency)
	$(GO) build -trimpath -buildvcs=auto -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/spindle

fmt: ## format Go sources
	gofmt -l . | xargs -r gofmt -w

vet: web ## the Go "-D warnings" gate: gofmt clean, go vet, go build
	test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	$(GO) vet ./...
	$(GO) build ./...

test: test-go test-web ## fast tier = Go fast tests + web tests
test-go: web ## fast Go tier: hermetic, < 1 min, watchdog-wrapped
	$(T5) $(GO) test -short -count=1 ./...
test-web: ## headless JS logic tests + fetch smoke (node --test, imports source)
	node --test web/test/unit/*.test.js

race: web ## full fast tier under the race detector (main pushes / on demand)
	$(T30) $(GO) test -race -short -count=1 -timeout 10m ./...

# The PR gate races the packages that own shared state; the full tier above runs
# on every main push (docs/go/15_testing.md §6.1). internal/webui is a large
# render suite that owns no shared state, so racing it on every PR spends gate
# time proving nothing (it is also 12s now, not 543s: §6.7).
RACE_PKGS := ./internal/wal/... ./internal/store/... ./internal/notify/... \
             ./internal/api/... ./internal/events/... ./internal/threads/... \
             ./internal/packages/... ./internal/maintain/... ./internal/identity/... \
             ./internal/server/... ./internal/git/... ./internal/sshd/... ./internal/mergequeue/... \
             ./internal/bundle/... ./internal/sim/... ./internal/staticpages/...

race-pr: web ## PR race tier: the concurrency-owning packages under -race
	$(T30) $(GO) test -race -short -count=1 -timeout 10m $(RACE_PKGS)

cover: ## coverage gate: >= 95% statements, every internal/... package (per-leaf profiles; e2e harness glue excluded)
	@mkdir -p .cover && rm -f .cover/*.out
	@pkgs=$$($(GO) list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./internal/... | grep -v devtools | grep -v "/e2e"); \
	for p in $$pkgs; do \
		name=$$(echo $$p | sed 's|github.com/crueber/spindle/internal/||; s|/|_|g'); \
		$(GO) test -count=1 -timeout 10m -coverprofile=.cover/$$name.out "$$p" || exit 1; \
	done
	$(GO) run ./internal/devtools/covergate -dir .cover -min 95

contract: ## store contract suite: memory + filesystem (always run)
	$(GO) test -count=1 ./internal/store/ -run TestContract

contract-fs: ## store contract suite: filesystem backend only
	$(GO) test -count=1 ./internal/store/ -run TestContract_Filesystem

sim: ## fault-injection consistency proof + round-trip budgets (seeded; skips in -short)
	$(T15) $(GO) test -count=1 -timeout 15m ./internal/sim/...

contract-s3: ## store contract against rustfs (make dev-store first)
	SPINDLE_TEST_S3_ENDPOINT=$(SPINDLE_TEST_S3_ENDPOINT) \
	SPINDLE_TEST_S3_ACCESS_KEY=$(SPINDLE_TEST_S3_ACCESS_KEY) \
	SPINDLE_TEST_S3_SECRET_KEY=$(SPINDLE_TEST_S3_SECRET_KEY) \
	SPINDLE_TEST_S3_BUCKET=$(SPINDLE_TEST_S3_BUCKET) \
	$(GO) test -count=1 ./internal/store/ -run TestContractS3


landing-gifs: ## regenerate the landing-page concept GIFs (MANUAL: run only when scenes change; freshness pinned by the golden test)
	$(GO) run ./internal/devtools/landinggif -out web/public/concepts/

e2e: ## smart-HTTP end-to-end against the real git binary
	$(T15) $(GO) test -count=1 -skip 'TestA11y' ./internal/e2e/...

a11y: web ## rendered accessibility smoke: headless Chromium over CDP (fixed page set; skips without a browser, CI sets SPINDLE_A11Y=1)
	$(T15) $(GO) test -count=1 -timeout 10m -run 'TestA11y' -v ./internal/e2e/

# The parity gate (docs/features/21_parity_master.md §9, PAR-201). Integrity
# mode: the matrix must be structurally sound, every `have` row must name a test
# that exists, the plan's id set must match, and matrix.md must be a fresh
# render. It needs NO docker — the corpus parse/plan tier runs in -short.
parity: ## parity gate + corpus parse/plan tier (no docker; the PR gate)
	$(GO) run ./internal/devtools/paritygate -check
	$(T15) $(GO) test -count=1 -timeout 10m -run 'TestParity' ./internal/e2e/

# The plan's definition of done (§9): no `missing` row at all. Run when a wave
# closes, not on every PR — Wave 1 lands with later waves still planned-missing.
parity-closed: ## parity closure check: fail on any `missing` row (§9 definition of done)
	$(GO) run ./internal/devtools/paritygate -check -strict
	$(T15) $(GO) test -count=1 -timeout 10m -run 'TestParity' ./internal/e2e/

# The execution tier is opt-in: it boots a server and a runner, executes a
# corpus-shaped workflow AND the committed golden of a real Forgejo workflow
# (byte-unmodified) in real containers, and builds/pushes/pulls a daemonless
# image through a local registry:2 — which the PR gate must not require (its
# jobs are containers with no daemon). SPINDLE_PARITY_EXEC=1 makes a missing
# daemon a failure, not a skip.
parity-exec: ## parity execution tier: corpus execution + a daemonless registry build (needs a host docker daemon + docker CLI)
	SPINDLE_PARITY_EXEC=1 $(T15) $(GO) test -count=1 -timeout 15m -run 'TestE2E_ParityExecution' -v ./internal/e2e/

backup-drill: ## restore drill: seed, snapshot, restore into an empty instance, compare every surface (docs/BACKUP.md)
	$(T15) $(GO) test -count=1 -timeout 15m -run TestE2E_BackupRestoreDrill -v ./internal/e2e/

image: ## build the OCI image
	docker build -t spindle .

dev-store: ## start rustfs (S3-compatible) for bucket-contract tests
	docker compose up -d rustfs

dev-store-stop:
	docker compose down

# project website (site/ + docs/demo.webm) staged into one directory, the same way Pages serves it
SITE_DIR ?= dist/site
SITE_PORT ?= 8099
site: ## stage the marketing site into dist/site (exactly what GitHub Pages serves)
	scripts/site-stage.sh $(SITE_DIR)

site-preview: site ## stage and serve the site at http://localhost:8099
	@echo "http://localhost:$(SITE_PORT)/"
	cd $(SITE_DIR) && python3 -m http.server $(SITE_PORT) --bind 127.0.0.1

site-check: ## static site gate: publish strings, referenced assets, one h1, alt, no off-origin assets
	scripts/site-check.sh

clean: ## remove build artifacts
	rm -rf $(BINARY) .cover web/dist dist
	mkdir -p web/dist && touch web/dist/.keep

ci: vet test race cover contract sim e2e parity a11y ## everything that must be green before a merge (09: contract + e2e gate the collab seams too; parity is the conformance gate, no docker; a11y is the rendered floor, skipping only where no browser exists)

help: ## show targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: web build fmt vet test test-go test-web race cover sim contract contract-fs contract-s3 e2e a11y parity parity-closed parity-exec backup-drill image dev-store dev-store-stop site site-preview site-check clean ci help landing-gifs
