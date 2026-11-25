// serve.go — `spindle serve` (the default): the §10.4 startup order of
// 06_server_http.md — bootstrap decision tree, store by backend, registry,
// engine, api env, chi server, loops gated by server.roles, prewarm,
// watchdog, plain-HTTP/h2c listener, and the two-phase drain on SIGTERM/SIGINT.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/events"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
	"github.com/crueber/spindle/internal/webui"
)

// runServe boots the server (§10.4). It returns the process exit code.
func runServe(ctx context.Context, c *cli, args []string) int {
	dataDir := dataDirFor(c)
	if err := config.EnsureDataDir(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "spindle: data dir: %v\n", err)
		return exitErr
	}

	cfg, fileState, err := resolveConfig(c)
	if err != nil {
		switch fileState {
		case stateAbsent:
			// Missing explicitly-named file (§6.1: exit 2).
			fmt.Fprintf(os.Stderr, "spindle: %v\n", err)
			return exitArg
		case stateUnconfigured:
			// NO config file and no complete OIDC set anywhere: the zero-config
			// first run is RESTRICTED (PLAN-OIDC-ONLY §2.1). The only reachable
			// surfaces are the Administration area and health; /admin carries
			// the OIDC onboarding card. The validation findings are logged for
			// the operator, not rendered as a wall of boot errors.
			fmt.Fprintf(os.Stderr, "spindle: first run: OIDC is not configured — serving only /admin until a config is saved and the server restarted (%v)\n", err)
			cfg, _ = setupOnlyBootConfig(c, dataDir, err)
			boot := server.BootState{Mode: "setup_only", FileState: "absent", ConfigPaths: configCandidates(c), ConfigExplicit: configPathExplicit(c)}
			return serveHTTP(ctx, cfg, boot, dataDir, nil)
		default:
			// INVALID config → RESTRICTED MODE (§3.4): only /admin, /healthz,
			// /readyz, /services/public answer; the setup UI displays the
			// errors. F5: boot on the file's BEST-EFFORT values (not
			// FirstRunDefaults), so [server].listen is honoured and the page
			// pre-fills from the file.
			fmt.Fprintf(os.Stderr, "spindle: config invalid — entering restricted mode (Administration area only): %v\n", err)
			cfg, msgs := setupOnlyBootConfig(c, dataDir, err)
			boot := server.BootState{Mode: "setup_only", FileState: "invalid", Errors: msgs, ConfigPaths: configCandidates(c), ConfigExplicit: configPathExplicit(c)}
			return serveHTTP(ctx, cfg, boot, dataDir, nil)
		}
	}
	// The --data-dir flag is authoritative over the XDG/env default (§3.1.1):
	// re-point the flag-derived PATHS so first-run store/cache and setup saves
	// live in the directory the operator named. The loaded config keeps every
	// file/env value (backend, auth, …) — re-deriving FirstRunDefaults here
	// would silently discard the SPINDLE__* overlay.
	// resolveConfig already applied this same sync for every subcommand
	// (Forgejo #611: syncDataDirFlag in config.go); this pass is kept as an
	// idempotent no-op (post-sync values no longer equal the env-derived
	// paths, so neither branch fires twice).
	envDataDir := config.ResolveDataDir(os.Getenv)
	cfg.DataDir = dataDir
	if cfg.Store.Root == filepath.Join(envDataDir, "store") {
		cfg.Store.Root = filepath.Join(dataDir, "store")
	}
	if cfg.Cache.Dir == filepath.Join(envDataDir, "cache") {
		cfg.Cache.Dir = filepath.Join(dataDir, "cache")
	}
	bootFileState := "valid"
	if fileState == stateAbsent {
		// The env overlay completed a valid OIDC set with no config file.
		bootFileState = "absent"
		logSetupBanner(cfg)
	}
	for _, w := range mustWarnings(cfg) {
		slog.Warn(w)
	}

	st, err := openStore(cfg, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spindle: store: %v\n", err)
		return exitErr
	}
	return serveHTTP(ctx, cfg, server.BootState{Mode: "normal", FileState: bootFileState, ConfigPaths: configCandidates(c), ConfigExplicit: configPathExplicit(c)}, dataDir, st)
}

// setupOnlyBootConfig builds the F5 setup-only boot config (the file's
// best-effort values ⊕ the env overlay) plus the banner/readyz error list: the
// original load error when nothing more specific is available, else every
// parse and validation finding separately. The returned Config.Server.Listen is
// exactly the address serveHTTP binds.
func setupOnlyBootConfig(c *cli, dataDir string, loadErr error) (*config.Config, []string) {
	cfg, bootErrs := config.LoadSetupOnly(dataDir, configCandidates(c), os.Getenv)
	_, verrs := config.Validate(cfg)
	for _, e := range verrs {
		bootErrs = append(bootErrs, e)
	}
	msgs := make([]string, 0, len(bootErrs))
	for _, e := range bootErrs {
		msgs = append(msgs, e.Error())
	}
	if len(msgs) == 0 && loadErr != nil {
		msgs = []string{loadErr.Error()}
	}
	return cfg, msgs
}

// serveHTTP assembles AppState and runs the listener until drain completes.
// st is nil in setup-only mode (no store, engine, api, or loops).
func serveHTTP(ctx context.Context, cfg *config.Config, boot server.BootState, dataDir string, st store.ObjectStore) int {
	log := newLogger(cfg)
	setupOnly := boot.InSetupOnly()

	// The live [import] SSRF policy (config.LiveImport): constructed once here
	// from the effective config and injected into every request-time reader
	// (the Import page, the import service, mirrors, fork sync, the setup
	// save). The save publishes to it after the file is written, so the import
	// gates take the new policy on the next request — no restart
	// (11_config_cli.md §3.7 read-live keys).
	liveImport := config.NewLiveImport(cfg)

	// Cache-space safety (BUG-6): a cache.dir on a filesystem smaller than the
	// default budget can never fit it. The loader keeps provenance, so a
	// configured value is reported but never changed; the compiled default is
	// lowered to the eviction watermark with this log line.
	if !setupOnly {
		if note := config.AdaptCacheBudget(cfg); note != "" {
			log.Warn(note, "cache_dir", cfg.Cache.Dir, "cache_max_bytes", int64(cfg.Cache.MaxBytes))
		}
	}

	var reg *wal.Registry
	var engine *server.WalEngine
	var apiEnv *api.Env
	var collab *collabWiring
	if !setupOnly {
		wal.SetWarnLogger(func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })
		reg = wal.NewRegistry(ctx, st, cfg)
		engine = server.NewWalEngine(reg, cfg)
		apiEnv = api.NewEnv(st, &repoRegistry{reg: reg, st: st}, cfg, engine, version(), hostname())
		apiEnv.Tasks = &opsTasks{reg: reg, eng: engine}
		apiEnv.Instance = reg.InstanceID()
		// Explicit create-repo placeholder (#210 §4): one hint set shared
		// by the api create paths (Add) and the server push pipeline
		// (Consume) — same process, so adoption needs no push-path read.
		apiEnv.PlaceholderHints = &api.PlaceholderHints{}
		// Features 01–08 assemble in exactly one place (collab.go,
		// 09 §4 touch point 3); integration tests reuse buildCollab
		// so the measured composition is the shipped composition.
		collab = buildCollab(st, cfg, reg, apiEnv, liveImport)
	}

	// ---- events bridge before server.New (§10.4 order: AppState then loops) --
	roles := roleSet(cfg.Server.Roles)
	eventsRole := len(roles) == 0 || roles["events"]
	var pullsSvc *pulls.Service
	var notifySvc *notify.Service
	var ident *identity.Service
	if collab != nil {
		pullsSvc, notifySvc, ident = collab.pullsSvc, collab.notifySvc, collab.ident
	}
	// The on-push fan-out (push mirrors + Threads run creation) is shared by
	// the git receive path and the browser editor's publish: both ARE pushes.
	onPush := onPushHookOf(collab)
	var wake func(repo string)
	if !setupOnly && eventsRole && (cfg.Events.WebhookURL != "" || pullsSvc != nil || notifySvc != nil) {
		var sinks []events.Sink
		if cfg.Events.WebhookURL != "" {
			sinks = append(sinks, &events.WebhookSink{URL: cfg.Events.WebhookURL, Secret: cfg.Events.WebhookSecret})
		}
		// [[events.sinks]] (14 §14.6): the built-in webhook and jsonl kinds
		// through the registry. Validation already named any bad key; a
		// constructor failure here is a boot error (fail closed).
		for i, spec := range cfg.Events.Sinks {
			secret := ""
			if spec.SecretEnv != "" {
				secret = os.Getenv(spec.SecretEnv)
				if secret == "" {
					log.Warn("events: sink secret_env is empty; deliveries are unsigned",
						"index", i, "name", spec.Name, "env", spec.SecretEnv)
				}
			}
			sink, serr := events.NewSink(events.SinkSpec{
				Name: spec.Name, Kind: spec.Kind, URL: spec.URL, Secret: secret,
				Path: spec.Path, MaxBytes: int64(spec.MaxBytes), Keep: spec.Keep,
			})
			if serr != nil {
				log.Error("events: bad [[events.sinks]] entry", "index", i, "err", serr)
				return 1
			}
			sinks = append(sinks, sink)
		}
		if pullsSvc != nil {
			// The pulls invalidation sink rides the bridge (Seam 4);
			// its Deliver never fails, so it never holds back the
			// shared cursor.
			sinks = append(sinks, &pullsSinkAdapter{svc: pullsSvc})
		}
		if notifySvc != nil {
			// Repository hooks subscribed to push/ref topics receive the
			// bridge batch through the activity log (docs/features/06
			// §5.3 push amendment); the delivery task, signature, retry
			// and redeliver are the existing collaboration ones.
			var lister pushCommitLister
			if engine != nil && reg != nil {
				lister = &pushCommitReader{eng: engine, layer: reg.GitLayer()}
			}
			sinks = append(sinks, &repoHooksSink{svc: notifySvc, commits: lister})
		}
		bridge := events.New(events.Deps{
			Source:        events.NewRegistrySource(reg),
			Store:         reg.Store(),
			Sinks:         sinks,
			SweepInterval: time.Duration(cfg.Events.SweepInterval),
			Logger:        log,
		})
		go bridge.Run(ctx)
		wake = func(repo string) { bridge.Wake(repo) }
		// Server-side ref publishes (merge, update-branch) wake the bridge too:
		// they happen after the pull request's own in-process hooks, and the
		// sink still owes the PR head/base reconciliation a prompt catch-up
		// (15 §2.1 round 2).
		if collab != nil && collab.pullsPub != nil {
			collab.pullsPub.wake = wake
		}
		// The browser editor's publish is a push: wake the bridge so events,
		// webhooks and Threads triggers fire promptly, not at the sweep.
		if collab != nil && collab.webeditPub != nil {
			collab.webeditPub.wake = wake
			collab.webeditPub.onPush = onPush
		}
	}

	srv := server.New(server.Options{
		Config:    cfg,
		Store:     st,
		Engine:    engine,
		API:       newAPI(apiEnv),
		DataDir:   dataDir,
		CacheRoot: cfg.Cache.Dir,
		Version:   version(),
		Instance:  instanceID(cfg),
		Kind:      serverKind(cfg),
		Boot:      boot,
		Log:       log,
		Notifier:  wake,
		ReadGate:  readGateOf(ident),
		// Forgejo #347: the repo-scoped push gate (CheckPush for
		// existing repos, CheckCreateOwner for auto-create) over the
		// same identity service (nil in setup-only → legacy gating).
		PushGate: pushGateOf(ident),
		// Forgejo #240: the pull-only refusal predicate over the same
		// store (nil in setup-only — no push paths exist there).
		MirrorGuard: mirrorGuardOf(st),
		// #210 adoption hints: the same instance apiEnv carries above
		// (nil in setup-only — no create or push paths exist there).
		PlaceholderHints: placeholderHintsOf(apiEnv),
		// Forgejo #376: the post-login avatar hook (background
		// generation for users without one; nil in setup-only —
		// no logins happen there).
		AvatarHook: avatarHookOf(ident),
		// Forgejo #623: the on-push push-mirror fan-out, extended for
		// Threads run creation (docs/features/15 §2) — one hook, both
		// consumers (nil in setup-only — no push paths exist there; nil
		// without the collab surface — no fan-out).
		OnPush: onPush,
		// PLAN-V2 §M.7: the advisory spam signal, scored inside the
		// collab-write middleware and filed through the one moderation
		// service (nil without collab wiring).
		SpamObserver: spamObserverOf(collab),
		// The setup API publishes a saved [import] policy here (the page save
		// publishes to the same holder), so the Import API reads it live.
		LiveImport: liveImport,
	})
	// Features 01–08 mount here (collab.go chainCollab, 09 §4 touch
	// point 3: one block per package + the per-user SSE mounts that
	// ride the notify handler).
	chainCollab(srv, collab)
	// D-WEB-14: the server-rendered page layer. It answers migrated UI routes
	// and falls through to the SPA for the rest during the migration; a
	// template parse error is a boot error (a broken page must not 500 later).
	//
	// Built UNCONDITIONALLY, setup-only included: /setup is a server-rendered
	// page and an INVALID config has to be fixable from the browser (§3.4), so
	// the provider must exist in every boot mode. wireWebUI binds the feature
	// seams; setup-only passes nil services, so only the setup deps bind and
	// every other page renders its "not wired" card instead of panicking.
	ui, uerr := webui.New(webui.Env{Cfg: cfg, API: apiEnv, Version: version(), Restricted: setupOnly, LiveImport: liveImport})
	if uerr != nil {
		log.Error("webui: templates failed to parse", "err", uerr)
		return 1
	}
	wireWebUI(cfg, boot, apiEnv, collab)
	srv.SetUI(ui)
	// Forgejo #370: OIDC usernames. The auth service resolves verified
	// emails to immutable usernames through the identity registry
	// (users/<username>/user.json, CAS on creation); the reverse
	// lookup backs username-carrying credentials (SSH key auth).
	// Unwired (setup-only, tests) the pure derivation base applies.
	// Store failures degrade to the base — the name stays leak-safe
	// (no @), only collision-uniqueness waits for the next login.
	if ident != nil {
		identSvc := ident
		srv.Auth().UsernameResolver = func(email string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if u, err := identSvc.ResolveUsername(ctx, email); err == nil && u != "" {
				return u
			}
			return ""
		}
		srv.Auth().EmailLookup = func(username string) (string, bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if em, err := identSvc.EmailForUsername(ctx, username); err == nil && em != "" {
				return em, true
			}
			return "", false
		}
		// PLAN-ADMIN §3.7: the credential boundary's account gate. One
		// exact-key read of users/<username>/user.json per authenticated
		// OIDC-derived credential; a store error fails the credential
		// closed (503) rather than reading as "not disabled".
		srv.Auth().AccountLookup = func(username string) (auth.AccountState, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return identSvc.UserState(ctx, username)
		}
	}

	// the SSH key registry backs both the sshd auth lookup and the
	// /api/v1/ssh-keys surface (17_ssh.md §3); setup-only has no store, so
	// the keys surface stays down until a valid config boots
	if apiEnv != nil {
		apiEnv.SSHKeys = srv.SSHKeyRegistry()
	}

	// ---- background loops (§10.4 step 5), gated by server.roles ---------------
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()

	var maintainerDone chan struct{}
	// adminMaint is the maintainer the Administration Overview reads its
	// metrics from; nil when this host does not run one (serve-only, or the
	// restricted boot) — the card is then absent, never a fake zero.
	var adminMaint *maintain.Maintainer
	if !setupOnly {
		if len(roles) == 0 || roles["maintain"] {
			m := maintain.NewWalMaintainer(reg, maintain.Options{Sweepers: maintainSweepers(collab)})
			adminMaint = m
			maintainerDone = make(chan struct{})
			go func() {
				defer close(maintainerDone)
				m.Run(drainCtx) // maintenance stops at phase-1 drain (§12)
			}()
			if collab != nil && collab.mirrorSvc != nil {
				// Forgejo #240 scheduled-sync loop (the follow.go
				// shape: its own cadence, never a maintenance unit,
				// never blocking maintenance). The bucket lease (R1
				// (d)) arbitrates across maintain hosts, so the loop
				// runs on every maintain host; enumeration is the
				// in-memory registry + mirror.json probes (no LIST).
				go collab.mirrorSvc.RunLoop(drainCtx, mirrorLoopInterval, nil)
			}
			if collab != nil && collab.pushMirrorSvc != nil {
				// Forgejo #623 scheduled-sync loop (same shape:
				// its own cadence, never a maintenance unit).
				// Repos with scheduling OFF never fire here —
				// on-push is their only trigger.
				go collab.pushMirrorSvc.RunLoop(drainCtx, pushMirrorLoopInterval, nil)
			}
		}
		prewarmAsync(ctx, engine, cfg.Cache.Prewarm, cfg.Cache.PrewarmParallelism, log)
		go watchdog(ctx, log)
		if notifySvc != nil {
			// Feature 06 background loops (Seam 5): webhook wake-ups +
			// sweeps and the daily retention pass. Same pattern as the
			// events bridge above; stops at drain via ctx.
			go notifySvc.Run(ctx)
		}
	}

	// Administration area (docs/PLAN-ADMIN.md M2): bind AFTER the background
	// loops so the Overview reads a live maintainer (nil there = absent card),
	// and before the listener accepts a request.
	wireAdminPages(cfg, boot, apiEnv, srv, adminMaint, collabModSvc(collab), collabThreadsSvc(collab), collabPackagesSvc(collab), reg, liveImport)
	// PLAN-ADMIN §3.7: the instance directory's reads and its one audited
	// transition, bound after the area itself (BindDirectory fills the seam
	// BindAdmin installed). A restricted boot has no identity service, so the
	// page renders its honest not-wired card.
	wireAdminDirectory(collab)

	// ---- listener (§10.4 step 6): plain HTTP with h2c ---------------------------
	// TLS termination belongs on the reverse proxy in front of spindle
	// (#165): the server never wraps the listener.
	handler := srv.Handler()
	httpSrv := srv.NewHTTPServer(handler, appCtxer{ctx})

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Server.Listen, "err", err)
		return exitErr
	}
	ln = srv.BuildListener(ln) // TCP_NODELAY per connection
	log.Info("listening", "addr", cfg.Server.Listen, "version", version())

	// ---- SSH git transport (17_ssh.md): disabled unless server.ssh.listen set --
	// In the restricted boot the listener is gated OFF: there is no store and no
	// auth registry, so refusing to listen is the honest version of every key
	// lookup failing closed (PLAN-OIDC-ONLY §2.1).
	if setupOnly {
		log.Info("ssh transport disabled in restricted mode")
	} else if sshSrv, serr := srv.SSH(); serr != nil {
		log.Error("ssh disabled: config error", "err", serr)
	} else if sshSrv != nil {
		go func() {
			if err := sshSrv.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
				log.Error("ssh server stopped", "err", err)
			}
		}()
	}

	serveErr := make(chan error, 2)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	// Loopback IPv6 twin so *.localhost works (§10.4 step 6). Its bind failure
	// is not fatal (the main listener is serving), but it is not silent either:
	// a twin that did not bind is why a client resolving *.localhost to ::1
	// hangs, so log it loudly enough to be diagnosed.
	if twin, ok := loopbackTwin(cfg.Server.Listen); ok {
		go func() {
			tln, terr := net.Listen("tcp", twin)
			if terr != nil {
				log.Warn("ipv6 twin bind failed; *.localhost over ::1 will not answer", "addr", twin, "err", terr)
				return
			}
			if serr := httpSrv.Serve(srv.BuildListener(tln)); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				log.Error("ipv6 twin server stopped", "addr", twin, "err", serr)
			}
		}()
	}

	// ---- static-site listener (docs/features/19 §5) -----------------------------
	// A second listener on its own address, answering one Host. Only a normal
	// boot with a store and identity starts it (a restricted boot has neither,
	// and the sites it would advertise are not readable). A bind failure is a
	// startup failure: sites were configured on, so an address that cannot be
	// bound is an outage, never a silent downgrade to "main listener only".
	var staticSrv *http.Server
	if cfg.StaticSites.Enabled && !setupOnly && ident != nil && reg != nil {
		if staticHandler := newStaticPagesHandler(cfg, ident, reg, apiEnv, log); staticHandler != nil {
			sln, lerr := net.Listen("tcp", cfg.StaticSites.Address)
			if lerr != nil {
				log.Error("static sites listen failed", "addr", cfg.StaticSites.Address, "err", lerr)
				return exitErr
			}
			staticSrv = srv.NewHTTPServer(staticHandler, appCtxer{ctx})
			go func() {
				if serr := staticSrv.Serve(srv.BuildListener(sln)); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
					log.Error("static sites server stopped", "err", serr)
				}
			}()
			// Loopback IPv6 twin so the configured *.localhost host resolves on
			// both families, exactly like the main listener (§10.4 step 6). Same
			// rule as that one: a failed twin bind is logged, never swallowed.
			if twin, ok := loopbackTwin(cfg.StaticSites.Address); ok {
				go func() {
					tln, terr := net.Listen("tcp", twin)
					if terr != nil {
						log.Warn("static ipv6 twin bind failed; the static host will not answer over ::1", "addr", twin, "err", terr)
						return
					}
					if serr := staticSrv.Serve(srv.BuildListener(tln)); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
						log.Error("static sites ipv6 twin stopped", "addr", twin, "err", serr)
					}
				}()
			}
			log.Info("static sites listening", "addr", cfg.StaticSites.Address, "host", cfg.StaticSites.Host)
		} else {
			log.Warn("static sites enabled but composition seams are missing; listener not started")
		}
	}

	// ---- two-phase drain on SIGTERM/SIGINT (§12) --------------------------------
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve failed", "err", err)
			return exitErr
		}
		return exitOK
	case <-ctx.Done():
	}

	// Phase 1 (bounded 30 s): maintenance stops; serving + /readyz stay up.
	srv.RunPhase1(drainCtx, ctx)
	// Feature-task drain (10 §B5 + 13 §8): cancel in-flight import
	// leaders AND bodies promptly and refuse new ones. reg.Tasks().Drain()
	// is the registry owner's hook (cancels every running task runCtx —
	// SIGKILLs the import clone, fails its store work); importSvc.Drain()
	// is the service hook (cancels the leader Run wait, gates Begin, arms
	// the commit-point guard that refuses post-drain manifest CAS commits).
	if reg != nil {
		reg.Tasks().Drain()
	}
	if collab != nil && collab.importSvc != nil {
		collab.importSvc.Drain()
	}
	// Notify task leaders (webhooks, fanout) derive their store work
	// from the service drainCtx: Drain cancels them promptly so a
	// wedged store call terminates instead of hanging shutdown
	// (issue #154 — same shape as the #74 import fix above).
	if collab != nil && collab.notifySvc != nil {
		collab.notifySvc.Drain()
	}
	if maintainerDone != nil {
		select {
		case <-maintainerDone:
		case <-time.After(30 * time.Second):
		}
	}
	// Phase 2: readyz flips, new requests refused, in-flight capped.
	//
	// Total bound, stated once: server.drain_timeout from this line. Main's
	// phase 2 is bounded by 2×drain_timeout (its own Shutdown deadline plus the
	// inflight wait), and the static listener's Shutdown — started HERE, at the
	// same boundary, never after phase 2 — carries an absolute deadline of
	// now+drain_timeout. The static drain runs concurrently, so the pair costs
	// max(2×drain_timeout, drain_timeout) = 2×drain_timeout and no more: a stuck
	// request on each listener cannot add a third budget, and the static
	// listener stops accepting at this instant rather than only after the app's
	// whole drain, which is what used to leave it accepting user traffic (and
	// cutting in-flight static responses at exit) through phase 2.
	drainUntil := time.Now().Add(time.Duration(cfg.Server.DrainTimeout))
	var staticDrained chan struct{}
	if staticSrv != nil {
		staticDrained = make(chan struct{})
		go func() {
			defer close(staticDrained)
			shutCtx, cancel := context.WithDeadline(context.Background(), drainUntil)
			defer cancel()
			// Shutdown closes the listener(s) immediately, then waits for
			// in-flight requests until the deadline. A failed Shutdown is
			// logged by the server itself and must not extend the bound.
			_ = staticSrv.Shutdown(shutCtx)
		}()
	}
	srv.RunPhase2(httpSrv)
	if staticDrained != nil {
		<-staticDrained
	}
	cancelDrain()
	if reg != nil {
		reg.Close()
	}
	log.Info("drained; exiting", "version", version())
	return exitOK
}

// newAPI builds the RouteProvider over the api env (nil-safe).
func newAPI(env *api.Env) server.RouteProvider {
	if env == nil {
		return nil
	}
	return server.NewAPIProvider(env)
}

// readGateOf adapts the identity service to the server ReadGate seam
// (nil in setup-only mode → legacy read gating).
func readGateOf(ident *identity.Service) server.ReadGate {
	if ident == nil {
		return nil
	}
	return ident
}

// pushGateOf adapts the identity service to the server PushGate seam
// (Forgejo #347: nil in setup-only mode → legacy host-flag push gating).
// The compiler pins the contract: *identity.Service must satisfy both
// CheckPush and CheckCreateOwner.
func pushGateOf(ident *identity.Service) server.PushGate {
	if ident == nil {
		return nil
	}
	var _ server.PushGate = ident
	return ident
}

// avatarHookOf adapts the identity service to the server AvatarHook
// seam (Forgejo #376: nil in setup-only mode → no post-login
// generation). The hook only enqueues (EnsureAvatarAsync never blocks),
// so the login response never waits on generation.
func avatarHookOf(ident *identity.Service) func(username, email string) {
	if ident == nil {
		return nil
	}
	return func(username, email string) { ident.EnsureAvatarAsync(username, email) }
}

// placeholderHintsOf shares the #210 adoption hint set between the api
// create paths and the server push pipeline (nil in setup-only mode →
// no push-path marker ops).
func placeholderHintsOf(env *api.Env) *api.PlaceholderHints {
	if env == nil {
		return nil
	}
	return env.PlaceholderHints
}

// appCtxer adapts the process context to http.Server BaseContext.
type appCtxer struct{ ctx context.Context }

func (a appCtxer) Context() context.Context { return a.ctx }

// watchdog ticks 1 s and warns when a tick is > 2.5 s late (§10.4 step 5).
func watchdog(ctx context.Context, log *slog.Logger) {
	last := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if d := now.Sub(last); d > 2500*time.Millisecond {
				log.Warn("runtime stalled", "gap", d.String())
			}
			last = now
		}
	}
}

// prewarmAsync warms the configured repos with bounded parallelism and gates
// /readyz until done (cache.prewarm_ready_timeout is enforced by readyz).
func prewarmAsync(ctx context.Context, eng *server.WalEngine, repos []string, parallelism int, log *slog.Logger) {
	if len(repos) == 0 || eng == nil {
		return
	}
	if parallelism <= 0 {
		parallelism = 2
	}
	server.SetPrewarmPending(int64(len(repos)))
	go func() {
		sem := make(chan struct{}, parallelism)
		var wg sync.WaitGroup
		var remaining int64 = int64(len(repos))
		for _, id := range repos {
			wg.Add(1)
			sem <- struct{}{}
			go func(repo string) {
				defer wg.Done()
				defer func() { <-sem }()
				rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				rid, err := git.ParseRepoId(repo)
				if err != nil {
					log.Warn("prewarm: invalid repo id", "repo", repo)
					return
				}
				if err := eng.Sync(rctx, rid, wal.LevelFull); err != nil {
					log.Warn("prewarm failed", "repo", repo, "err", err) // never fatal
				}
				remaining--
				server.SetPrewarmPending(remaining)
			}(id)
		}
		wg.Wait()
	}()
}

// ---- helpers ---------------------------------------------------------------------

// roleSet normalizes server.roles (empty = all roles).
func roleSet(roles []string) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		out[r] = true
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func instanceID(cfg *config.Config) string {
	if cfg.Maintenance.Host != "" {
		return cfg.Maintenance.Host
	}
	return hostname()
}

func serverKind(cfg *config.Config) server.ServerKind {
	if cfg.Cache.Mode == "disk" || cfg.Maintenance.Disk == "ssd" {
		return server.KindSSD
	}
	if cfg.Cache.Mode == "budget" {
		return server.KindServerless
	}
	return server.KindDev
}

// mustWarnings surfaces validation warnings at boot (§5 rule 1 and friends).
func mustWarnings(cfg *config.Config) []string {
	warnings, errs := config.Validate(cfg)
	out := append([]string{}, warnings...)
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}

// logSetupBanner emits the env-configured first-run notice: no config file,
// but a complete OIDC set came from the environment, so the instance serves
// normally. Saving /admin/config writes the file that makes it durable.
func logSetupBanner(cfg *config.Config) {
	slog.Info("first run: OIDC is configured entirely from the environment on " + cfg.Server.Listen +
		"; save /admin/config to write " + config.ConfigFileName + " and make it durable")
}

// openStore builds the object store per store.backend (D4).
func openStore(cfg *config.Config, dataDir string) (store.ObjectStore, error) {
	switch cfg.Store.Backend {
	case "memory":
		return store.NewMemory(), nil
	case "filesystem":
		root := cfg.Store.Root
		if root == "" {
			root = filepath.Join(dataDir, "store")
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("create store root %s: %w", root, err)
		}
		fsStore, err := store.NewFilesystemRoot(root, store.DefaultBulkConcurrency)
		if err != nil {
			return nil, err
		}
		return fsStore, nil
	case "s3":
		return store.NewS3(&cfg.Store)
	default:
		return nil, fmt.Errorf("unknown store.backend %q", cfg.Store.Backend)
	}
}

// loopbackTwin reports the IPv6 twin listener address for a 127.0.0.1 bind.
func loopbackTwin(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		return "", false
	}
	return net.JoinHostPort("::1", port), true
}

// newLogger builds the slog logger per telemetry.log_format / log_filter
// (RUST_LOG / SPINDLE_LOG win when set).
func newLogger(cfg *config.Config) *slog.Logger {
	filter := cfg.Telemetry.LogFilter
	if v := os.Getenv("SPINDLE_LOG"); v != "" {
		filter = v
	} else if v := os.Getenv("RUST_LOG"); v != "" {
		filter = v
	}
	level := slog.LevelInfo
	if strings.Contains(strings.ToLower(filter), "debug") || strings.Contains(strings.ToLower(filter), "trace") {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Telemetry.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// ---- RepoRegistry adapter (07_api.md §8: answered from the STORE) --------------
//
// Owners/Repos are MANIFEST-gated: a prefix without repos/<o>/<r>/manifest.pb
// is not a repository (issue #200). Stale prefixes linger after Delete: the
// sweep removes every listed object, but the filesystem CAS sidecars
// (<name>.lock) persist by design — invisible to List, yet their directory
// keeps the name behind ListPrefixes — and a fork-provisioned prefix
// (fork.json written before the child manifest lands, #150) is likewise
// unborn. Gating on the manifest keeps both out of the listings on every
// backend; Exists already gates the same way, so re-create/re-import after a
// delete sees a clean name.
type repoRegistry struct {
	reg *wal.Registry
	st  store.ObjectStore
	// onChildDelete sweeps fork provenance after a successful Delete:
	// called as onChildDelete(ctx, child, parent) with both ids in
	// "owner/name" form. Set by buildCollab (issue #457); nil skips the
	// sweep (tests without the collaboration layer). Best-effort only —
	// it must never fail the delete (which already linearized).
	onChildDelete func(ctx context.Context, child, parent string)
}

func (r *repoRegistry) Owners(ctx context.Context) ([]string, error) {
	owners, err := listOwners(ctx, r.st)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, owner := range owners {
		repos, rerr := r.liveRepos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if len(repos) > 0 {
			out = append(out, owner)
		}
	}
	return out, nil
}

func (r *repoRegistry) Repos(ctx context.Context, owner string) ([]string, error) {
	return r.liveRepos(ctx, owner)
}

// OwnerRepoCounts answers the api.RepoRegistry per-owner live-repo counts
// (Forgejo #307 — the owners/detailed repo_count rail). It walks exactly
// what Owners walks — one liveRepos pass per owner, same manifest-gated
// ghost rule, same trip profile — and keeps the lengths Owners discards,
// so the detailed endpoint pays zero added store trips for the counts.
// Owners with zero live repos are absent, never zero-valued.
//
// ### Concurrency
// Hazard: none new — the loop is sequential like Owners (no shared mutable
// state, no lock); parallelism lives inside each liveRepos call, unchanged.
func (r *repoRegistry) OwnerRepoCounts(ctx context.Context) (map[string]int, error) {
	owners, err := listOwners(ctx, r.st)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, owner := range owners {
		repos, rerr := r.liveRepos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if len(repos) > 0 {
			out[owner] = len(repos)
		}
	}
	return out, nil
}

// liveRepos lists the manifest-backed repos under owner: the raw prefix
// listing minus deleted-repo ghosts (manifest.pb swept, sidecar litter
// behind) and unborn fork-provisioned prefixes (fork.json, manifest not yet
// written — #150, which the row-level tolerateMissing still covers as a
// race). Manifest Heads run in parallel (limit 8, mirroring
// wal.refreshList); a missing/unreadable manifest drops the name — the same
// fail-closed choice as refreshList, so a transient store error hides a repo
// from one listing refresh rather than resurrecting a deleted one.
//
// ### Concurrency
// Hazard: parallel Head goroutines appending to one slice. Avoidance: a
// single mutex guards the collect; no lock is held across a store call (the
// Head happens before the lock). Output is sorted after the join.
func (r *repoRegistry) liveRepos(ctx context.Context, owner string) ([]string, error) {
	names, err := listRepos(ctx, r.st, owner)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []string{}, nil
	}
	g, gctx := store.WithContext(ctx)
	g.SetLimit(8)
	var mu sync.Mutex
	live := make([]string, 0, len(names))
	for _, name := range names {
		name := name
		g.Go(func() error {
			meta, herr := r.st.Head(gctx, "repos/"+owner+"/"+name+"/"+store.Manifest)
			if herr != nil || meta == nil {
				return nil // absent/unreadable manifest = not a repo
			}
			mu.Lock()
			live = append(live, name)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(live)
	return live, nil
}

func (r *repoRegistry) Exists(ctx context.Context, id git.RepoId) (bool, error) {
	meta, err := r.st.Head(ctx, id.StorePrefix()+store.Manifest)
	if err != nil || meta == nil {
		return false, nil //nolint:nilerr // absent manifest = not registered
	}
	return true, nil
}

func (r *repoRegistry) Create(ctx context.Context, id git.RepoId, format git.ObjectFormat) error {
	if _, err := r.reg.Create(ctx, id.String(), format); err != nil {
		var we *wal.WalError
		if errors.As(err, &we) && we.Kind == wal.WalErrAlreadyExists {
			return fmt.Errorf("%w: %s", api.ErrExists, id.String())
		}
		return err
	}
	return nil
}

func (r *repoRegistry) Delete(ctx context.Context, id git.RepoId) error {
	// Child-delete sweep (issue #457): capture the fork parent BEFORE the
	// wipe (the wipe deletes fork.json), then sweep AFTER the manifest
	// delete linearizes. The order is load-bearing for GC safety: a
	// maintain pass between the row-removal and the child wipe cannot
	// exist (the row goes second), so the walk either probes a 404 it
	// already skips or never probes at all. The sweep is best-effort —
	// a failed delete sweeps nothing, and a failed sweep never fails
	// the delete (no lock held across the store calls, 13 §2 rule 4).
	parent := forkParentOf(ctx, r.st, id.Owner, id.Name)
	_, err := r.reg.Delete(ctx, id.String())
	if err != nil {
		return err
	}
	if parent != "" && r.onChildDelete != nil {
		r.onChildDelete(ctx, id.String(), parent)
	}
	return nil
}

// ---- api.Tasks adapter (07_api.md §12: ops + task table) ------------------------

// opsTasks binds the api.Tasks seam onto wal.TaskTable and the engine ops.
type opsTasks struct {
	reg      *wal.Registry
	eng      *server.WalEngine
	ident    *identity.Service
	packages *packages.Service
	insights *insights.Service
}

func (t *opsTasks) Ops() []api.OpSpec {
	ops := []api.OpSpec{
		{Op: "sync"},
		{Op: "checkpoint", Params: []api.OpParam{{Name: "trigger"}}},
	}
	if t.ident != nil {
		// Seam 5: the access-bootstrap migration (docs/features/01 §10).
		ops = append(ops, api.OpSpec{Op: "access-bootstrap"})
	}
	if t.packages != nil {
		// Seam 5: the registry mark/sweep (docs/features/14 §6), normally
		// driven by the maintainer on its own cadence; this is the manual
		// force (SSE-attachable, joins a running pass).
		ops = append(ops, api.OpSpec{Op: packages.TaskKindGC})
	}
	if t.insights != nil {
		// Seam 5: the repository pulse refresh (docs/features/18), normally
		// driven by the maintainer's due gate; the manual force joins a
		// running refresh instead of racing it.
		ops = append(ops, api.OpSpec{Op: insights.TaskKind})
	}
	return ops
}

func (t *opsTasks) List(ctx context.Context, id git.RepoId) ([]api.TaskRecord, []api.TaskRecord, error) {
	running := []api.TaskRecord{}
	for _, rec := range t.reg.Tasks().RunningFor(id.String()) {
		running = append(running, api.TaskRecordFromWal(*rec))
	}
	recent := []api.TaskRecord{}
	for _, rec := range t.reg.Tasks().List(id.String()) {
		recent = append(recent, api.TaskRecordFromWal(*rec))
	}
	return running, recent, nil
}

func (t *opsTasks) Get(ctx context.Context, id git.RepoId, taskID string) (api.TaskRecord, bool, error) {
	rec := t.reg.Tasks().Get(taskID)
	if rec == nil {
		return api.TaskRecord{}, false, nil
	}
	return api.TaskRecordFromWal(*rec), true, nil
}

// Begin starts (or joins) an op; the SSE stream replays the per-repo progress
// broadcast and follows it live until the task record finishes.
func (t *opsTasks) Begin(ctx context.Context, id git.RepoId, op string, params map[string]string) (api.TaskStream, error) {
	h, err := t.reg.Open(ctx, id.String())
	if err != nil {
		return api.TaskStream{}, err
	}
	fn := t.opFn(id, op, params)
	if fn == nil {
		return api.TaskStream{}, fmt.Errorf("op %q is not available on this instance", op)
	}
	subID, updates, replay := h.Progress().Subscribe()
	recCh := make(chan api.TaskDone, 1)
	go func() {
		defer h.Progress().Unsubscribe(subID)
		rec, runErr := t.reg.Tasks().Run(context.WithoutCancel(ctx), id.String(), op, params, fn)
		done := api.TaskDone{}
		if runErr != nil {
			done.Err = &api.TaskErr{Status: http.StatusInternalServerError, Message: runErr.Error()}
		}
		if rec != nil {
			done.Record = api.TaskRecordFromWal(*rec)
		}
		recCh <- done
	}()
	return api.TaskStream{Replay: replayProgress(replay), Updates: convertProgress(updates), Done: recCh}, nil
}

// convertProgress bridges wal.Progress onto the wire type on a new channel.
func convertProgress(in <-chan wal.Progress) <-chan api.Progress {
	out := make(chan api.Progress)
	go func() {
		defer close(out)
		for p := range in {
			out <- api.ProgressFromWal(p)
		}
	}()
	return out
}

func (t *opsTasks) Attach(ctx context.Context, id git.RepoId, taskID string) (api.TaskStream, bool, error) {
	rec := t.reg.Tasks().Get(taskID)
	if rec == nil {
		return api.TaskStream{}, false, nil
	}
	if rec.Finished != "" {
		done := api.TaskDone{Record: api.TaskRecordFromWal(*rec)}
		recCh := make(chan api.TaskDone, 1)
		recCh <- done
		return api.TaskStream{Record: done.Record, Done: recCh}, true, nil
	}
	// Handle-free attach (law 7): the table's per-task drone carries the
	// narration, so a task whose repository handle is gone still streams —
	// the repo-transfer task tears its source handle down before the copy
	// pass (PLAN-V2 §T.1), which would strand an attach routed through
	// RepoHandle.Progress().
	subID, updates, replay, ok := t.reg.Tasks().Subscribe(taskID)
	if !ok {
		return api.TaskStream{}, false, nil
	}
	recCh := make(chan api.TaskDone, 1)
	ticker := time.NewTicker(500 * time.Millisecond)
	go func() {
		defer t.reg.Tasks().Unsubscribe(taskID, subID)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cur := t.reg.Tasks().Get(taskID); cur != nil && cur.Finished != "" {
					done := api.TaskDone{Record: api.TaskRecordFromWal(*cur)}
					recCh <- done
					return
				}
			}
		}
	}()
	return api.TaskStream{Record: api.TaskRecordFromWal(*rec), Replay: replayProgress(replay), Updates: convertProgress(updates), Done: recCh}, true, nil
}

// opFn maps an op name onto its engine action (§12.2 table subset).
func (t *opsTasks) opFn(id git.RepoId, op string, params map[string]string) func(ctx context.Context, task *wal.Task) error {
	switch op {
	case "sync":
		return func(ctx context.Context, task *wal.Task) error {
			return t.eng.Sync(ctx, id, wal.LevelServe)
		}
	case "checkpoint":
		return func(ctx context.Context, task *wal.Task) error {
			h := t.reg.Get(id.String())
			if h == nil {
				return fmt.Errorf("repo %s not open", id.String())
			}
			trigger := params["trigger"]
			if trigger == "" {
				trigger = string(wal.TriggerManual)
			}
			return h.WriteCheckpoint(ctx, wal.CheckpointTrigger(trigger))
		}
	case "access-bootstrap":
		if t.ident == nil {
			return nil
		}
		return func(ctx context.Context, task *wal.Task) error {
			created, err := t.ident.BootstrapRepo(ctx, id.Owner, id.Name)
			if err != nil {
				return err
			}
			task.Notice(bootstrapSummary(id.String(), created))
			return nil
		}
	case packages.TaskKindGC:
		if t.packages == nil {
			return nil
		}
		return func(ctx context.Context, task *wal.Task) error {
			_, err := t.packages.GC(ctx, id.Owner, id.Name, task)
			return err
		}
	case insights.TaskKind:
		if t.insights == nil {
			return nil
		}
		return func(ctx context.Context, task *wal.Task) error {
			return t.insights.Refresh(ctx, id.Owner, id.Name, task)
		}
	default:
		return nil
	}
}

// bootstrapSummary narrates the access-bootstrap outcome (Seam 5 task).
func bootstrapSummary(repo string, created bool) string {
	if created {
		return "access-bootstrap " + repo + ": materialized"
	}
	return "access-bootstrap " + repo + ": already present"
}

func replayProgress(in []wal.Progress) []api.Progress {
	out := make([]api.Progress, 0, len(in))
	for _, p := range in {
		out = append(out, api.ProgressFromWal(p))
	}
	return out
}
