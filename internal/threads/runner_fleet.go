package threads

// Fleet operations (PAR-180/PAR-181, PAR-173).
//
// The fleet is list / drain / resume / rotate / revoke / deregister:
//
//	list        ListRunners (admin) and the runner's own declare answer
//	drain       finish the current job, claim nothing new (RunnerDraining)
//	resume      back to active (admin, or the runner itself)
//	rotate      issue a new credential with an overlap window so a running
//	            worker never holds a dead token (zero downtime); the old hash
//	            stays valid until PreviousValidUntil, then stops
//	revoke      admin-only kill (real 401 on the next call)
//	deregister  remove the record and credential (admin, or the runner itself)
//
// Drain, resume, rotate and deregister are available to the runner through its
// own credential (the runner can retire or rotate itself without an admin
// session) and to the host administrator through /api/v1/admin/runners. Every
// mutation is audited; no token value ever reaches a log, an audit detail or an
// answer beyond the one rotation/registration response that must carry it.
//
// An EPHEMERAL runner (PAR-173) is a one-job worker: the server deletes its
// registry record and credential after the first terminal job, so a disposable
// pool worker cannot be replayed. The runner process exits after the same job.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// RotationGrace is the overlap window a rotated-away credential keeps working
// (15 minutes: enough for a runner to fetch the new token and rewrite its token
// file, short enough that a leaked old token is not permanently live).
const RotationGrace = 15 * time.Minute

// RunnerTokenRotation is the rotate answer: the new secret travels exactly
// once, with the old one's deadline.
type RunnerTokenRotation struct {
	RunnerID      string `json:"runner_id"`
	Token         string `json:"token"`
	PreviousUntil string `json:"previous_valid_until"`
}

// DrainRunner stops one runner claiming new jobs while its current job finishes
// (host admin, audited). It is idempotent.
func (s *Service) DrainRunner(ctx context.Context, id string, p auth.Principal) error {
	if err := requireHostAdmin(p); err != nil {
		return err
	}
	return s.setRunnerState(ctx, id, RunnerDraining, p.Name, AuditRunnerDrain, "state=draining")
}

// ResumeRunner returns a draining runner to active (host admin, audited).
func (s *Service) ResumeRunner(ctx context.Context, id string, p auth.Principal) error {
	if err := requireHostAdmin(p); err != nil {
		return err
	}
	return s.setRunnerState(ctx, id, RunnerActive, p.Name, AuditRunnerResume, "state=active")
}

// setRunnerState is the shared drain/resume CAS. A revoked runner is refused:
// re-arming a killed credential is exactly what revoke exists to prevent.
func (s *Service) setRunnerState(ctx context.Context, id, state, actor, action, detail string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !tokenIDRe.MatchString(id) {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	_, err := s.casUpdate(ctx, RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
		}
		if d.State == RunnerRevoked || d.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: runner %s is revoked", ErrConflict, id)
		}
		if d.State == state {
			return nil, false, nil
		}
		d.State = state
		d.Version++
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return err
	}
	s.auditAdmin(ctx, actor, action, "runner:"+id, detail)
	return nil
}

// RotateRunnerToken issues a fresh credential for one runner with an overlap
// window (host admin, audited). The old secret keeps working until the window
// closes, so a worker that is mid-job can finish and pick up the new token; the
// next request after the window is a real 401.
func (s *Service) RotateRunnerToken(ctx context.Context, id string, p auth.Principal) (*RunnerTokenRotation, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	return s.rotateRunnerToken(ctx, strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(p.Name))
}

// RotateOwnRunnerToken is the runner-authenticated rotation: an operator can
// rotate a worker's credential in place (`spindle runner rotate --token-file`)
// without holding an admin session. The runner's own bearer is the authority.
func (s *Service) RotateOwnRunnerToken(ctx context.Context, runner *runnerAuth) (*RunnerTokenRotation, error) {
	if runner == nil || runner.Doc == nil {
		return nil, fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	if runner.Doc.IsDraining() {
		return nil, fmt.Errorf("%w: a draining runner cannot rotate; finish the drain first", ErrConflict)
	}
	return s.rotateRunnerToken(ctx, runner.Doc.ID, "runner:"+runner.Doc.ID)
}

// rotateRunnerToken is the shared implementation.
func (s *Service) rotateRunnerToken(ctx context.Context, id, actor string) (*RunnerTokenRotation, error) {
	if !tokenIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	doc, _, derr := s.loadRunner(ctx, id)
	if derr != nil {
		return nil, derr
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	if doc.State == RunnerRevoked || doc.RevokedAt != nil {
		return nil, fmt.Errorf("%w: runner %s is revoked", ErrConflict, id)
	}
	principal := strings.TrimSpace(doc.RegisteredBy)
	if principal == "" {
		principal = "runner"
	}
	token, hash, merr := mintScopedToken(runnerTokenPrefix, principal, id)
	if merr != nil {
		return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, merr)
	}
	until := s.nowUTC().Add(RotationGrace).Format(dateTimeFmt)
	var previous string
	_, err := s.casUpdate(ctx, RunnerCredentialKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: runner %s has no credential", ErrNotFound, id)
		}
		var cred RunnerCredential
		if jerr := json.Unmarshal(cur, &cred); jerr != nil {
			return nil, false, fmt.Errorf("%w: credential %s: %v", ErrCorrupt, id, jerr)
		}
		if cred.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: runner %s is revoked", ErrConflict, id)
		}
		previous = cred.TokenHash
		cred.TokenHash = hash
		cred.PreviousHash = previous
		cred.PreviousValidUntil = &until
		cred.Version++
		return mustJSON(&cred), true, nil
	})
	if err != nil {
		return nil, err
	}
	s.auditAdmin(ctx, actor, AuditRunnerRotate, "runner:"+id, "previous_valid_until="+until)
	return &RunnerTokenRotation{RunnerID: id, Token: token, PreviousUntil: until}, nil
}

// DrainOwnRunner is the runner-authenticated drain: a worker taking itself out
// of rotation keeps serving until its current job finishes and claims nothing
// new. Idempotent, audited.
func (s *Service) DrainOwnRunner(ctx context.Context, runner *runnerAuth) error {
	if runner == nil || runner.Doc == nil {
		return fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	return s.setRunnerState(ctx, runner.Doc.ID, RunnerDraining, "runner:"+runner.Doc.ID, AuditRunnerDrain, "state=draining")
}

// ResumeOwnRunner is the runner-authenticated resume (the operator undoes a
// drain from the worker's own credential).
func (s *Service) ResumeOwnRunner(ctx context.Context, runner *runnerAuth) error {
	if runner == nil || runner.Doc == nil {
		return fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	return s.setRunnerState(ctx, runner.Doc.ID, RunnerActive, "runner:"+runner.Doc.ID, AuditRunnerResume, "state=active")
}

// DeregisterRunner removes one runner's record and credential (host admin,
// audited). Unlike revoke it leaves no tombstone: the id is gone and can be
// re-registered. It is the cleanup path for a retired worker.
func (s *Service) DeregisterRunner(ctx context.Context, id string, p auth.Principal) error {
	if err := requireHostAdmin(p); err != nil {
		return err
	}
	return s.deregisterRunner(ctx, strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(p.Name))
}

// DeregisterOwnRunner is the runner-authenticated deregistration (a worker
// retiring itself; `spindle runner deregister`).
func (s *Service) DeregisterOwnRunner(ctx context.Context, runner *runnerAuth) error {
	if runner == nil || runner.Doc == nil {
		return fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	return s.deregisterRunner(ctx, runner.Doc.ID, "runner:"+runner.Doc.ID)
}

// deregisterRunner is the shared removal. The credential is deleted FIRST: once
// the record is gone a stale bearer must already be dead, never briefly valid
// against a record that no longer exists.
func (s *Service) deregisterRunner(ctx context.Context, id, actor string) error {
	if !tokenIDRe.MatchString(id) {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	doc, _, derr := s.loadRunner(ctx, id)
	if derr != nil {
		return derr
	}
	if doc == nil {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	if doc.CurrentJob != nil {
		return fmt.Errorf("%w: runner %s is running a job; drain it first", ErrConflict, id)
	}
	if err := s.Store.Delete(ctx, RunnerCredentialKey(id), ""); err != nil && !store.IsPreconditionFailed(err) {
		return err
	}
	if err := s.Store.Delete(ctx, RunnerKey(id), ""); err != nil && !store.IsPreconditionFailed(err) {
		return err
	}
	_ = s.Store.Delete(ctx, RunnerTokenKey(id), "")
	s.auditAdmin(ctx, actor, AuditRunnerDeregister, "runner:"+id, "record and credential deleted")
	return nil
}

// retireEphemeralRunner deletes a one-job runner after its terminal attempt
// (PAR-173). It runs on a fresh-enough context (the completion request's) and is
// best effort: the job's conclusion is already committed, and the sweeper's
// stale-ephemeral pass can finish the deletion if this call loses a race.
func (s *Service) retireEphemeralRunner(ctx context.Context, id string) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !tokenIDRe.MatchString(id) {
		return
	}
	doc, _, err := s.loadRunner(ctx, id)
	if err != nil || doc == nil || !doc.Ephemeral {
		return
	}
	_ = s.Store.Delete(ctx, RunnerCredentialKey(id), "")
	_ = s.Store.Delete(ctx, RunnerKey(id), "")
	_ = s.Store.Delete(ctx, RunnerTokenKey(id), "")
	s.auditAdmin(ctx, "system", AuditRunnerEphemeralEnd, "runner:"+id, "one job completed; record and credential deleted")
}
