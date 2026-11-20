# spindle

A git host in Go: **git over smart HTTP (v0/v2), LFS, bundle-uri, a JSON API with SSE, and a web UI — where the object store is the only database.**

It serves repositories whose entire state — refs, packs, config, policy, events, web UI — lives as objects in a bucket (filesystem or S3). Instances are disposable; wipe one and you lose nothing but warmth.

## Inspired by walgit

spindle exists because of **Tobi Lütke's fantastic [walgit](https://github.com/tobi/walgit)**. It proved that a git host can put everything on an object store, and it is the direct inspiration for this project. Thank you, Tobi.

We aim to stay **object-protocol compliant** with walgit: the bucket key layout, the protobuf wire encoding, and git wire behavior all follow walgit's formats. The visual direction is our own.

## Quick start

```sh
make build          # stylesheet + islands + SDK, then the binary — web/dist is embedded
./spindle            # zero-config: 0.0.0.0:8080, filesystem store, auth "none" (loud warning)
```

Then open **http://localhost:8080** — pages are rendered by the server (no client app shell to boot); `/setup` configures everything for real use. No config file exists yet, so the first run is deliberately friction-free; save one from `/setup` and restart. An invalid config file puts the server in **setup-only mode** (everything but setup/health answers 503) until you fix it through the same UI.

Push something — repositories auto-create on push by default:

```sh
mkdir demo && cd demo && git init -b main
echo "# demo" > README.md && git add -A && git commit -m "hi"
git remote add origin http://localhost:8080/you/demo.git
git push -u origin main          # browse it at http://localhost:8080/you/demo
```

Auth: `none` (everyone anonymous — dev only), `token` (static bearer/basic tokens), or `oidc` (any OpenID Connect issuer, plus walgit-issued `wgt_` access tokens minted in the UI).

## Run with docker compose (prebuilt image)

GitHub Actions publishes the image on every push to [`ghcr.io/crueber/spindle`](https://github.com/crueber/spindle/pkgs/container/spindle) — pull it instead of building:


```yaml
# docker-compose.yml — spindle alone: filesystem store on a named volume
services:
  spindle:
    image: ghcr.io/crueber/spindle:latest   # :main tracks main; vX.Y.Z releases; sha-<sha> per commit
    ports:
      - "8080:8080"
      - "2222:2222"                        # git over SSH (17_ssh.md)
    environment:
      SPINDLE__SERVER__SSH__LISTEN: 0.0.0.0:2222
    volumes:
      - spindle-data:/var/lib/spindle
    restart: unless-stopped

volumes:
  spindle-data:
```

```sh
docker compose up -d        # update later with: docker compose pull && docker compose up -d
```

Then open **http://localhost:8080/setup** — the same zero-config first boot as the binary: save a config from the setup page and restart the container. Repositories auto-create on push under `http://localhost:8080/<owner>/<repo>.git`.

Git also works over **SSH**: the stack publishes port 2222 and enables the SSH transport (`SPINDLE__SERVER__SSH__LISTEN`). Add your public key on the **/keys** page, then:

```sh
git clone ssh://git@localhost:2222/<owner>/<repo>.git
```

The host key auto-generates into the data volume, so it is stable across container restarts.

For an **S3-backed store** (rustfs/MinIO), see [`compose.yaml`](compose.yaml) — the shipped stack builds from source; to run it from the published image instead, replace the `spindle` service's `build: .` with `image: ghcr.io/crueber/spindle:latest` (the rustfs service and the `SPINDLE__STORE__*` env stay as they are). [`compose.standalone.yml`](compose.standalone.yml) is the same standalone shape as above, built from source instead of pulled.

## v1 release requirements

Status of every item, the GitHub-parity bar and the closing work: [`docs/PLAN-V1.md`](docs/PLAN-V1.md).

- [x] [git storage](internal/store)
- [x] [issues](internal/issues)
- [x] [pull requests](internal/pulls)
- [x] [tags](internal/tags)
- [x] [releases](internal/releases)
- [ ] packages — [specified](docs/features/14_packages.md), not yet implemented
- [ ] [cli](cmd/spindle) — every operator action, machine-readable
- [ ] [threads](docs/features/15_threads.md) — CI: workflow files, container jobs, checks results
- [x] [fork](internal/social)
- [x] [webhooks](internal/notify)
- [x] [oidc](internal/server)

## Backlog

- [ ] file edit
- [ ] pr/merge protection rules
- [ ] projects
- [ ] wiki
- [ ] insights
- [ ] moderation
- [ ] sponsorships
- [ ] forum
- [ ] ownership transfer

## Development

```sh
make build     # stylesheet + islands + SDK, then the static binary
make web       # just the UI assets (app.css, ui.js, vendored htmx, repos.js)
make ci        # vet + fast tests + race + ≥95% coverage per package — the merge gate
make contract  # store contract suite (memory + filesystem; S3 via `make dev-store`)
make e2e       # real-git end-to-end flows
make test-web  # node --test over the island and SDK logic modules
make image     # OCI image
```

Backend: Go 1.27 (module `github.com/crueber/spindle`), six direct third-party modules (chi, BurntSushi/toml, x/net, x/crypto for SSH, dicebear-go + dicebear/styles for avatars — the budget is AGENTS.md law 1). Frontend: **server-rendered Go `html/template` pages** enhanced with **htmx (vendored)** and small vanilla-ES islands (`web/src/islands/`); no UI framework and no client data layer. Markdown, diffs, commit graphs and syntax highlighting are rendered server-side in Go, so user text is escaped and sanitized on the server. pnpm 11 builds `web/dist/app.css` (Tailwind v4), `web/dist/ui.js` (esbuild, islands) and the dependency-free `web/dist/repos.js` SDK; any Node runs the headless JS tests. The design language is **Precision Dark** — graphite panes, one amber action accent, monospace identifiers, dense 34 px rows — specified normatively in `docs/style-guideline.md` §1. The JSON API and the SDK remain the programmatic surface (browse it at `/api`). The container build is `Dockerfile`, with compose examples in `compose.standalone.yml` (filesystem store) and `compose.yaml` (S3-backed via rustfs).

## CI and publishing

CI runs on GitHub Actions (`.github/workflows/ci.yml`) on every pull request and every push to `main`: web build + headless JS tests, `go vet`, `-race`, the ≥95 % per-package coverage gate, e2e against the real git binary, the filesystem contract, and the S3 contract against a rustfs service, then the image build and the container contract. Pushes to `main` and release tags publish the container image to `ghcr.io/crueber/spindle:latest` (plus the commit SHA, and `vX.Y.Z` on release tags).

## License

[MIT](LICENSE) © 2026 Christopher Rueber

> No politics allowed — see our [Code of Conduct](CODE_OF_CONDUCT.md).
