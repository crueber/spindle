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
//	repos/<o>/<r>/threads/runners/<id>.json          runner registry record (labels, last seen)
//	repos/<o>/<r>/threads/secrets/<name>.json        sealed secret envelope (write-only API)
//	repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/log/<attempt>-<seq>   log frames (Create-only)
//	users/<principal>/tokens/threads/<id>.json       runner credentials (hash only, revocable)
//
// `users/<principal>/…` mirrors identity's one-segment encoding (@ → %40), so
// an email principal stays one key segment.

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

// RunnersPrefix returns repos/<o>/<r>/threads/runners/.
func RunnersPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/runners/"
}

// RunnerKey returns repos/<o>/<r>/threads/runners/<id>.json.
func RunnerKey(owner, repo, id string) string {
	return RunnersPrefix(owner, repo) + id + ".json"
}

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

// encodePrincipal mirrors identity's one-segment principal encoding (@ → %40).
func encodePrincipal(p string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(p)), "@", "%40")
}

// RunnerTokenKey returns users/<principal>/tokens/threads/<id>.json — the
// runner credential record (hash only, revocable).
func RunnerTokenKey(principal, id string) string {
	return "users/" + encodePrincipal(principal) + "/tokens/threads/" + id + ".json"
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
