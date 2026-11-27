package threads

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
)

// Bucket key helpers for the runner protocol (15 §2/§3). Everything the
// protocol touches is an object; there is no queue server and no state outside
// the store.
//
//	repos/<o>/<r>/threads/queue/<seq>.json           queue entry (Create, CAS-claimed)
//	repos/<o>/<r>/threads/queue/head.json            dispatch head (CAS'd, bounded pending window)
//	threads/queue/index.json                         global dispatch hint (CAS'd, bounded)
//	threads/runners/<id>.json                        instance runner registry (labels, capacity, last seen)
//	threads/runners/tokens/<id>.json                 single-use registration token (hash only, TTL)
//	threads/runners/credentials/<id>.json            long-lived runner credential (hash only, revocable)
//	repos/<o>/<r>/threads/secrets/<name>.json        sealed secret envelope (write-only API)
//	repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/log/<attempt>-<seq>   log frames (Create-only)
//
// Runners are instance-scoped infrastructure, so their families live under
// `threads/` (never a repository) and a credential names a runner id, never a
// repository. `users/<principal>/…` mirrors identity's one-segment encoding
// (@ → %40) and is still used by the secret/token families that belong to a
// principal.

// QueuePrefix returns repos/<o>/<r>/threads/queue/.
func QueuePrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/queue/"
}

// QueueEntryKey returns repos/<o>/<r>/threads/queue/<seq>.json.
func QueueEntryKey(owner, repo, seq string) string {
	return QueuePrefix(owner, repo) + seq + ".json"
}

// QueueHeadKey returns repos/<o>/<r>/threads/queue/head.json (the dispatch head).
func QueueHeadKey(owner, repo string) string {
	return QueuePrefix(owner, repo) + "head.json"
}

// MatrixAdmissionKey returns the per-run matrix admission record
// (PAR-052): repos/<o>/<r>/threads/runs/<run>/matrix.json. It lives beside the
// job records, not in the queue prefix, so the queue index and GC never read it
// as a queue entry.
func MatrixAdmissionKey(owner, repo, runID string) string {
	return "repos/" + owner + "/" + repo + "/threads/runs/" + runID + "/matrix.json"
}

// RunnersPrefix returns threads/runners/ (the instance registry family).
func RunnersPrefix() string { return "threads/runners/" }

// RunnerKey returns threads/runners/<id>.json.
func RunnerKey(id string) string { return RunnersPrefix() + id + ".json" }

// RunnerTokensPrefix returns threads/runners/tokens/ (single-use registration
// tokens, hash only).
func RunnerTokensPrefix() string { return RunnersPrefix() + "tokens/" }

// RunnerTokenKey returns threads/runners/tokens/<id>.json.
func RunnerTokenKey(id string) string { return RunnerTokensPrefix() + id + ".json" }

// RunnerCredentialsPrefix returns threads/runners/credentials/ (long-lived
// runner credentials, hash only).
func RunnerCredentialsPrefix() string { return RunnersPrefix() + "credentials/" }

// RunnerCredentialKey returns threads/runners/credentials/<id>.json. A
// credential names a runner id and never a repository: the runner may be
// offered any repository's jobs.
func RunnerCredentialKey(id string) string { return RunnerCredentialsPrefix() + id + ".json" }

// QueueIndexKey returns threads/queue/index.json — the global dispatch hint.
func QueueIndexKey() string { return "threads/queue/index.json" }

// SecretsPrefix returns repos/<o>/<r>/threads/secrets/.
func SecretsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/secrets/"
}

// SecretKey returns repos/<o>/<r>/threads/secrets/<name>.json.
func SecretKey(owner, repo, name string) string {
	return SecretsPrefix(owner, repo) + name + ".json"
}

// LogPrefix returns repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/log/.
func LogPrefix(owner, repo, runID, jobID string) string {
	return JobsPrefix(owner, repo, runID) + jobID + "/log/"
}

// LogKey returns one immutable log frame. The attempt is part of the key, so a
// re-run attempt cannot collide with — or overwrite — its predecessor's frames.
func LogKey(owner, repo, runID, jobID string, attempt, seq int) string {
	return LogPrefix(owner, repo, runID, jobID) + logFrameName(attempt, seq)
}

// logFrameName renders "<attempt:03d>-<seq:019d>": lexical order is attempt
// order then sequence order, so a prefix LIST replays a job's whole log.
func logFrameName(attempt, seq int) string {
	return padInt(attempt, 3) + "-" + padInt(seq, 19)
}

// padInt renders v zero-padded to at least width digits.
func padInt(v, width int) string {
	s := strconv.Itoa(v)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// --- opaque job references -------------------------------------------------

// jobRefPrefix marks an opaque job reference (a URL path segment that is not
// guessable across repositories, so `jobs/{id}` in the protocol table cannot
// collide between two runs that both own a job called "build").
const jobRefPrefix = "jr_"

// JobRef mints the opaque reference for one job: jr_<b64url(owner/repo/run/job)>.
func JobRef(repo, runID, jobID string) string {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return jobRefPrefix + base64.RawURLEncoding.EncodeToString([]byte(repo+"/"+runID+"/"+jobID))
	}
	return jobRefPrefix + base64.RawURLEncoding.EncodeToString([]byte(id.Owner+"/"+id.Name+"/"+runID+"/"+jobID))
}

// parseJobRef reverses JobRef into (owner, repo, run, job).
func parseJobRef(ref string) (owner, repo, runID, jobID string, err error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(ref), jobRefPrefix)
	if !ok {
		return "", "", "", "", ErrInvalid
	}
	body, derr := base64.RawURLEncoding.DecodeString(raw)
	if derr != nil {
		return "", "", "", "", ErrInvalid
	}
	parts := strings.Split(string(body), "/")
	if len(parts) != 4 {
		return "", "", "", "", ErrInvalid
	}
	if _, perr := git.ParseRepoId(parts[0] + "/" + parts[1]); perr != nil {
		return "", "", "", "", ErrInvalid
	}
	if !runIDRe.MatchString(parts[2]) || !jobIDRe.MatchString(parts[3]) {
		return "", "", "", "", ErrInvalid
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}
