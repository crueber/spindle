package threads

import (
	"context"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// The job checkout credential (15 §4/§5, round-2 pass 2).
//
// A job container clones its repository over spindle's own git HTTP transport
// with the attempt token (`wjt_…`) as the password. Composition registers this
// resolver on the server's Seam 2 ExtraCredential hook (the checks wct_ tokens
// are the sibling shape), so the credential resolves to an unprivileged
// `ci:<runner>` principal carrying ONE repository grant:
//
//   - read on the run's repository always (the checkout);
//   - push when the attempt token carries the `push` scope, which the start call
//     derives from the job's effective `permissions:` (`contents: write`).
//
// The grant is repository-scoped and nothing else: the principal has no host
// Write/Admin flag, so it can never touch another repository, the settings API
// or an org. A fork run's token is minted without `push` (the trust class wins),
// so a fork job cannot push no matter what its workflow declares.
//
// The token is short-lived (the job's timeout plus a grace window, capped at six
// hours) and it is *revoked at completion*: the resolver re-reads the job record
// (one exact-key GET) and rejects a token whose job is terminal or whose attempt
// has been superseded — the same identity authority every protocol write uses.
// An expired token is rejected by the signature check, before any store read.

// ClonePrincipalName renders the git principal of one runner (the checks
// package's ci:<id> convention).
func ClonePrincipalName(runnerID string) string { return "ci:" + runnerID }

// IsJobPrincipal reports whether a git principal name is a Threads job's clone
// credential (ci:<runner>). A WAL ref event pushed under it came from a job, not
// a user: workflows never trigger on it (the GITHUB_TOKEN rule, 15 §5). The WAL
// event's `pusher` is this name, so composition can mark the event without a
// second store read.
func IsJobPrincipal(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), "ci:")
}

// ResolveCloneCredential claims a wjt_ attempt token for git access. The
// principal's grant is (claims.Repo, read) plus push when the token carries the
// `push` scope.
func (s *Service) ResolveCloneCredential(token string) (auth.Principal, *auth.AuthError, bool) {
	// The seam carries no request context; a credential resolution is one GET,
	// bounded so a wedged store cannot stall a git handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.resolveCloneCredential(ctx, token)
}

// resolveCloneCredential is ResolveCloneCredential with a context (the server
// chain calls it opportunistically; a store hiccup must fail the credential
// closed rather than fall through and try the token as an ID token).
func (s *Service) resolveCloneCredential(ctx context.Context, token string) (auth.Principal, *auth.AuthError, bool) {
	if !strings.HasPrefix(strings.TrimSpace(token), attemptTokenPrefix) {
		return auth.Principal{}, nil, false
	}
	claims, err := s.parseAttemptToken(token, s.nowUTC())
	if err != nil || !claims.Allows(ScopeRead) {
		// Claimed (the prefix is ours) but rejected: a real 401, never a
		// fall-through that would try the token as an ID token.
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid job token"}, true
	}
	// Revocation at completion: the job record's attempt is the identity
	// authority (15 §3). One exact-key GET, and only for a job-token request.
	owner, repo, lerr := splitRepo(claims.Repo)
	if lerr != nil {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid job token"}, true
	}
	job, _, jerr := s.loadJobAndRun(ctx, owner, repo, claims.Run, claims.Job)
	if jerr != nil || job == nil || job.Attempt != claims.Attempt || terminal(job.State) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "job token revoked"}, true
	}
	return auth.Principal{
		Name:    ClonePrincipalName(claims.Runner),
		JobRepo: claims.Repo,
		JobPush: claims.Allows(ScopePush),
	}, nil, true
}

// ClaimRunnerCredential claims the runner protocol's own bearer shape (wrt_)
// for the server chain. It resolves the credential SHAPE only — an
// unprivileged ci:<id> principal with no write — because the secret, the
// revocation state and the repository scope are verified handler-side by
// authorizeRunner against the credential record (one exact-key GET). Without
// this claim a token-mode or oidc-mode instance would reject every runner
// protocol call in the chain before the runner handler ran (pass B2).
func ClaimRunnerCredential(token string) (auth.Principal, *auth.AuthError, bool) {
	if !strings.HasPrefix(strings.TrimSpace(token), runnerTokenPrefix) {
		return auth.Principal{}, nil, false
	}
	_, id, _, ok := tokenParts(token, runnerTokenPrefix)
	if !ok {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid runner token"}, true
	}
	return auth.Principal{Name: ClonePrincipalName(id)}, nil, true
}
