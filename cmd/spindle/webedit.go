// webedit.go — Feature F (docs/features/17_file_edit.md) composition: the
// browser file editor service (Seam 1, both lanes) over the P6 push gate owned
// by identity, stock git through the bounded pool, repo dirs through the WAL
// registry, and ref publishes through the WAL funnel. Nothing here is a second
// writer: the commit's pack is ingested through Layer.Ingest and the ref txn
// rides RepoHandle.Publish exactly like a pushed commit, so policy, events,
// Threads CI, checks and the events bridge observe it as a push.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	walgit "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
	"github.com/crueber/spindle/internal/webedit"
)

// newWebeditService builds the file-edit service over st/ident. pullsSvc is
// the shipped PR opener (nil in tests without the collab layer: open_pr then
// answers 503 truthfully after the branch lands).
func newWebeditService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, gitBinary string, pullsSvc *pulls.Service, cfg *config.Config) (*webedit.Service, *webedit.Handler, *webeditPublisher) {
	api.RegisterExposed(webedit.ExposedTemplates...)
	svc := webedit.New(st, ident)
	svc.Git = webedit.NewSubprocessGit(gitBinary)
	svc.Dirs = &pullsDirs{reg: reg}
	pub := &webeditPublisher{reg: reg}
	svc.Refs = pub
	if cfg != nil {
		svc.MaxBytes = int64(cfg.Webedit.MaxBytes)
	}
	if pullsSvc != nil {
		svc.PR = &pullsPROpener{svc: pullsSvc}
	}
	return svc, &webedit.Handler{Svc: svc}, pub
}

// chainWebedit fronts the core mux with the contents lane surface (Seam 1);
// authentication resolves through the server chain (Seam 2).
func chainWebedit(srv *server.Server, h *webedit.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// webeditPublisher publishes a server-made commit through the normal WAL
// funnel: one PUSH entry carrying the ref update and the pack (old=zero is a
// branch create). The manifest CAS arbitrates concurrent editors; a conflict
// maps to webedit.ErrConflict (409), never a force.
type webeditPublisher struct {
	reg    *wal.Registry
	wake   func(repo string)
	onPush func(id walgit.RepoId, refs []server.PushedRef, byJob bool)
}

// UpdateRefWithPack ingests the pack into the serving copy and commits the ref
// txn in the same WAL entry — the ref never dangles bucket-missing objects.
func (p *webeditPublisher) UpdateRefWithPack(ctx context.Context, repo, ref, old, newSHA string, pack []byte, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	var prepared *wal.PreparedPack
	if len(pack) > 0 {
		prepared, err = ingestWebeditPack(ctx, p.reg, h, pack)
		if err != nil {
			return err
		}
	}
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn:  &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: ref, OldOid: old, NewOid: newSHA}}},
		Pack: prepared,
		Meta: agent,
	})
	if err != nil {
		return err
	}
	for _, rr := range res.PerRef {
		if rr.Err != nil {
			if rr.Err.Kind == wal.RefErrConflict || rr.Err.Kind == wal.RefErrStale {
				return fmt.Errorf("%w: %s", webedit.ErrConflict, rr.Err.Detail)
			}
			return fmt.Errorf("publish %s: %s", rr.Name, rr.Err.Detail)
		}
	}
	if p.wake != nil {
		p.wake(repo)
	}
	// The same on-push fan-out a git push runs (push mirrors + Threads run
	// creation): the browser edit IS a push, so configured workflows fire.
	if p.onPush != nil {
		if id, perr := walgit.ParseRepoId(repo); perr == nil {
			p.onPush(id, []server.PushedRef{{Ref: ref, New: newSHA}}, false)
		}
	}
	return nil
}

// ingestWebeditPack ingests one complete pack into the serving copy and
// renders the PreparedPack the funnel uploads (the tags precedent). The pack
// is O(KiB–MiB): one blob, its ancestor trees and one commit.
func ingestWebeditPack(ctx context.Context, reg *wal.Registry, h *wal.RepoHandle, pack []byte) (*wal.PreparedPack, error) {
	res, err := reg.GitLayer().Ingest(ctx, h.Repo(), bytes.NewReader(pack), int64(len(pack)+1), false, true)
	if err != nil {
		return nil, err
	}
	sum := string(res.Checksum)
	packDir := h.Repo().PackDir()
	packPath := filepath.Join(packDir, "pack-"+sum+".pack")
	idxPath := filepath.Join(packDir, "pack-"+sum+".idx")
	packFi, err := os.Stat(packPath)
	if err != nil {
		return nil, err
	}
	idxFi, err := os.Stat(idxPath)
	if err != nil {
		return nil, err
	}
	return &wal.PreparedPack{
		Checksum:    sum,
		PackPath:    packPath,
		IdxPath:     idxPath,
		PackSize:    uint64(packFi.Size()),
		IdxSize:     uint64(idxFi.Size()),
		ObjectCount: res.ObjectCount,
	}, nil
}

// pullsPROpener adapts the shipped pulls open service to webedit.PROpener
// (F.6: composition owns the bridge; webedit imports no pulls internals).
type pullsPROpener struct {
	svc *pulls.Service
}

func (p *pullsPROpener) OpenPR(ctx context.Context, owner, repo string, in webedit.OpenInput, actor auth.Principal) (int, error) {
	th, _, err := p.svc.OpenPR(ctx, owner, repo, actor, pulls.OpenInput{
		Title: in.Title, Body: in.Body,
		BaseRef: "refs/heads/" + in.BaseRef,
		HeadRef: "refs/heads/" + in.HeadRef,
	}, "")
	if err != nil {
		switch {
		case errors.Is(err, pulls.ErrInvalid):
			return 0, fmt.Errorf("%w: %v", webedit.ErrInvalid, err)
		case errors.Is(err, pulls.ErrUnprocessable):
			return 0, fmt.Errorf("%w: %v", webedit.ErrUnsupported, err)
		case errors.Is(err, pulls.ErrConflict):
			return 0, fmt.Errorf("%w: %v", webedit.ErrConflict, err)
		case errors.Is(err, pulls.ErrForbidden):
			return 0, fmt.Errorf("%w: %v", webedit.ErrForbidden, err)
		case errors.Is(err, pulls.ErrUnauthorized):
			return 0, fmt.Errorf("%w: %v", webedit.ErrUnauthorized, err)
		default:
			return 0, err
		}
	}
	if th == nil {
		return 0, fmt.Errorf("%w: pull request opened without a number", webedit.ErrUnavailable)
	}
	return th.Num, nil
}

// compile-time seam assertions: the composition consumes exactly the narrow
// interfaces webedit defines (core never imports webedit).
var (
	_ webedit.Gate         = (*identity.Service)(nil)
	_ webedit.RepoDirs     = (*pullsDirs)(nil)
	_ webedit.RefPublisher = (*webeditPublisher)(nil)
	_ webedit.PROpener     = (*pullsPROpener)(nil)
	_ server.ExtraRoutes   = (*webedit.Handler)(nil)
)
