package threads

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Event payloads (PAR-033). A workflow reads `github.event.*` in expressions
// and the full JSON at $GITHUB_EVENT_PATH / $FORGEJO_EVENT_PATH. The payload is
// a fixed, documented projection built by the trigger's emitter:
//
//   - only the fields listed in workflow.EventFields reach the expression
//     context;
//   - a free-text body (an issue/comment body) is truncated to
//     maxEventBodyBytes, so a 5 MB comment is never copied into every run;
//   - an asset or label list is capped at maxEventListEntries;
//   - the encoded JSON is capped at maxEventPayloadBytes, and an over-cap
//     payload is a creation error (fail closed, never a silently truncated
//     event).
//
// The payload lives on the run header, so a rerun and every job of the run read
// the same bytes the trigger delivered.
const (
	maxEventBodyBytes    = 4 << 10
	maxEventListEntries  = 100
	maxEventPayloadBytes = 64 << 10
)

// PushEventPayload builds the `push` payload.
func PushEventPayload(repo, ref, before, after, actor string) map[string]any {
	return map[string]any{
		"ref":    ref,
		"before": before,
		"after":  after,
		"repository": map[string]any{
			"full_name": repo,
			"name":      repoNamePart(repo),
			"owner":     map[string]any{"login": ownerPart(repo)},
		},
		"sender": map[string]any{"login": actor},
	}
}

// PullEventPayload builds the `pull_request` payload. extra carries the
// trigger-specific action detail (a label name, the review state) under the
// documented key.
func PullEventPayload(repo, action string, num int, title, state, author, baseRef, baseSHA, headRef, headSHA, headRepo string, draft, merged, fork bool, actor string) map[string]any {
	return map[string]any{
		"action": action,
		"repository": map[string]any{
			"full_name": repo,
			"name":      repoNamePart(repo),
			"owner":     map[string]any{"login": ownerPart(repo)},
		},
		"sender": map[string]any{"login": actor},
		"pull_request": map[string]any{
			"number": num,
			"title":  title,
			"state":  state,
			"draft":  draft,
			"merged": merged,
			"user":   map[string]any{"login": author},
			"base": map[string]any{
				"ref": shortRef(baseRef), "sha": baseSHA,
				"repo": map[string]any{"full_name": repo},
			},
			"head": map[string]any{
				"ref": shortRef(headRef), "sha": headSHA,
				"repo": map[string]any{"full_name": headRepo},
			},
		},
	}
}

// RepositoryDispatchEventPayload builds the `repository_dispatch` payload
// (PAR-018). `client_payload` is the caller's bounded object (the API validates
// it before creation); the event reaches a workflow as
// `github.event.client_payload.<key>` and as $GITHUB_EVENT_PATH.
func RepositoryDispatchEventPayload(repo, eventType string, client map[string]any, actor string) map[string]any {
	cp := map[string]any{}
	for k, v := range client {
		cp[k] = v
	}
	return map[string]any{
		"action":         eventType,
		"event_type":     eventType,
		"repository":     repositoryProjection(repo),
		"sender":         map[string]any{"login": actor},
		"client_payload": cp,
	}
}

// WorkflowRunParent is the bounded projection of an upstream run carried by a
// `workflow_run` payload (PAR-019). Artifact ids and URLs are deliberately
// absent: cross-run artifact access is a separate, approved API (D-3), not
// something an event payload grants.
type WorkflowRunParent struct {
	ID         string
	Name       string
	Event      string
	Status     string
	Conclusion string
	RunAttempt int
	HeadBranch string
	HeadSHA    string
	WorkflowID string
	Repository string
	Actor      string
}

// WorkflowRunEventPayload builds the `workflow_run` payload.
func WorkflowRunEventPayload(action string, parent WorkflowRunParent) map[string]any {
	status := "completed"
	if action == "requested" {
		status = "queued"
	}
	return map[string]any{
		"action":     action,
		"repository": repositoryProjection(parent.Repository),
		"sender":     map[string]any{"login": parent.Actor},
		"workflow_run": map[string]any{
			"id":          parent.ID,
			"name":        parent.Name,
			"event":       parent.Event,
			"status":      status,
			"conclusion":  parent.Conclusion,
			"run_attempt": parent.RunAttempt,
			"head_branch": parent.HeadBranch,
			"head_sha":    parent.HeadSHA,
			"workflow_id": parent.WorkflowID,
			"repository":  map[string]any{"full_name": parent.Repository},
		},
	}
}

// RefEventPayload builds the `create`/`delete` payload (PAR-021): the ref and
// its type, nothing about content (a deleted ref has no readable tip).
func RefEventPayload(repo, ref, refType, actor string) map[string]any {
	return map[string]any{
		"ref":        ref,
		"ref_type":   refType,
		"repository": repositoryProjection(repo),
		"sender":     map[string]any{"login": actor},
	}
}

// DeploymentEventPayload builds the `deployment` / `deployment_status` payload
// (PAR-021). The run and job ids identify the record; no artifact or secret is
// named.
func DeploymentEventPayload(repo, actor, environment, ref, sha, runID, jobID, state, description string) map[string]any {
	payload := map[string]any{
		"repository": repositoryProjection(repo),
		"sender":     map[string]any{"login": actor},
		"deployment": map[string]any{
			"id":          jobID,
			"environment": environment,
			"ref":         ref,
			"sha":         sha,
			"task":        "deploy",
			"creator":     map[string]any{"login": actor},
			"run_id":      runID,
		},
	}
	if state != "" {
		payload["deployment_status"] = map[string]any{
			"state":       state,
			"description": truncateBody(description),
			"creator":     map[string]any{"login": actor},
		}
	}
	return payload
}

// repositoryProjection is the shared repository object in every payload.
func repositoryProjection(repo string) map[string]any {
	return map[string]any{
		"full_name": repo,
		"name":      repoNamePart(repo),
		"owner":     map[string]any{"login": ownerPart(repo)},
	}
}

// maxClientPayloadKeys is GitHub's bound on a dispatch's top-level properties.
// The byte cap is the shared event-payload cap (64 KiB), enforced on the whole
// payload after projection.
const maxClientPayloadKeys = 10

// maxClientPayloadDepth bounds nested objects/arrays inside client_payload: a
// dispatch is a trigger, not a data channel, and a deep document is a way to
// make every run's header large.
const maxClientPayloadDepth = 4

// ValidateClientPayload bounds a repository_dispatch client_payload before any
// run exists. It accepts JSON objects with scalar leaves (string, bool, number,
// null) and nested objects/arrays up to the documented depth, at most
// maxClientPayloadKeys top-level properties and 64 KiB encoded. Anything else
// is a named 422 — never a silently truncated event.
func ValidateClientPayload(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	if len(raw) > maxClientPayloadKeys {
		return nil, fmt.Errorf("%w: client_payload has %d top-level properties (max %d)", ErrUnprocessable, len(raw), maxClientPayloadKeys)
	}
	out := make(map[string]any, len(raw))
	for key, val := range raw {
		if !clientPayloadKeyRe.MatchString(key) {
			return nil, fmt.Errorf("%w: client_payload key %q must match [A-Za-z0-9_-]{1,64}", ErrUnprocessable, key)
		}
		if err := validateClientValue(val, 1); err != nil {
			return nil, fmt.Errorf("%w: client_payload.%s: %v", ErrUnprocessable, key, err)
		}
		out[key] = val
	}
	rawJSON, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("%w: client_payload: %v", ErrUnprocessable, err)
	}
	if len(rawJSON) > maxEventPayloadBytes {
		return nil, fmt.Errorf("%w: client_payload is %d bytes, over the %d-byte cap", ErrTooLarge, len(rawJSON), maxEventPayloadBytes)
	}
	return out, nil
}

// clientPayloadKeyRe is the client_payload key charset (an identifier the
// expression parser can name).
var clientPayloadKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateClientValue bounds nesting and leaf types.
func validateClientValue(val any, depth int) error {
	if depth > maxClientPayloadDepth {
		return fmt.Errorf("nesting exceeds %d levels", maxClientPayloadDepth)
	}
	switch v := val.(type) {
	case nil, string, bool, float64:
		return nil
	case map[string]any:
		for k, item := range v {
			if len(k) > 64 || k == "" {
				return fmt.Errorf("nested key %q is not a valid name (1…64 bytes)", k)
			}
			if err := validateClientValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, item := range v {
			if err := validateClientValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported value type %T (expected string, boolean, number, null, object or array)", val)
	}
}

type ReleaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ReleaseEventPayload builds the `release` payload. assets is capped (a release
// with thousands of assets must not inflate every run).
func ReleaseEventPayload(repo, action, tag, name, target, author string, draft, prerelease bool, assets []ReleaseAsset) map[string]any {
	if len(assets) > maxEventListEntries {
		assets = assets[:maxEventListEntries]
	}
	rows := make([]map[string]any, 0, len(assets))
	for _, a := range assets {
		rows = append(rows, map[string]any{"name": a.Name, "size": a.Size})
	}
	return map[string]any{
		"action": action,
		"repository": map[string]any{
			"full_name": repo,
			"name":      repoNamePart(repo),
			"owner":     map[string]any{"login": ownerPart(repo)},
		},
		"sender": map[string]any{"login": author},
		"release": map[string]any{
			"tag_name":         tag,
			"name":             name,
			"draft":            draft,
			"prerelease":       prerelease,
			"target_commitish": target,
			"author":           map[string]any{"login": author},
			"assets":           rows,
		},
	}
}

// IssueEventPayload builds the `issues` payload.
func IssueEventPayload(repo, action, author string, num int, title, state string, labels []string) map[string]any {
	if len(labels) > maxEventListEntries {
		labels = labels[:maxEventListEntries]
	}
	return map[string]any{
		"action": action,
		"repository": map[string]any{
			"full_name": repo,
			"name":      repoNamePart(repo),
			"owner":     map[string]any{"login": ownerPart(repo)},
		},
		"sender": map[string]any{"login": author},
		"issue": map[string]any{
			"number": num,
			"title":  title,
			"state":  state,
			"user":   map[string]any{"login": author},
			"labels": append([]string(nil), labels...),
		},
	}
}

// IssueCommentEventPayload builds the `issue_comment` payload (a comment on an
// issue; the body is bounded).
func IssueCommentEventPayload(repo, action, author, body string, num int, title, state string, labels []string, commentAuthor string) map[string]any {
	if len(labels) > maxEventListEntries {
		labels = labels[:maxEventListEntries]
	}
	return map[string]any{
		"action": action,
		"repository": map[string]any{
			"full_name": repo,
			"name":      repoNamePart(repo),
			"owner":     map[string]any{"login": ownerPart(repo)},
		},
		"sender": map[string]any{"login": author},
		"issue": map[string]any{
			"number": num,
			"title":  title,
			"state":  state,
			"user":   map[string]any{"login": author},
			"labels": append([]string(nil), labels...),
		},
		"comment": map[string]any{
			"body": truncateBody(body),
			"user": map[string]any{"login": commentAuthor},
		},
	}
}

// truncateBody bounds a free-text body carried in an event payload.
func truncateBody(body string) string {
	if len(body) <= maxEventBodyBytes {
		return body
	}
	cut := maxEventBodyBytes
	for cut > 0 && !utf8Start(body[cut]) {
		cut--
	}
	return body[:cut]
}

// utf8Start reports a byte that begins a UTF-8 rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// EncodeEventPayload marshals a payload under the documented size cap.
func EncodeEventPayload(payload map[string]any) ([]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: event payload: %v", ErrInvalid, err)
	}
	if len(raw) > maxEventPayloadBytes {
		return nil, fmt.Errorf("%w: event payload is %d bytes, over the %d-byte cap", ErrUnprocessable, len(raw), maxEventPayloadBytes)
	}
	return raw, nil
}

// EventFieldsOf flattens a payload's documented fields into the expression
// context (`github.event.action` → "event.action"). Only workflow.EventFields
// are projected: the event JSON is the complete record, the context is the
// closed, documented subset.
func EventFieldsOf(payload map[string]any) map[string]string {
	if len(payload) == 0 {
		return nil
	}
	out := make(map[string]string, len(workflow.EventFields))
	for _, field := range workflow.EventFields {
		if val, ok := payloadPath(payload, field); ok {
			if s, ok := eventScalar(val); ok {
				out["event."+field] = s
			}
		}
	}
	// repository_dispatch's client_payload keys are the dispatcher's, so they
	// are projected dynamically (the one wildcard in the closed vocabulary).
	if cp, ok := payload["client_payload"].(map[string]any); ok {
		for key, val := range cp {
			if s, ok := eventScalar(val); ok {
				out["event.client_payload."+key] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// payloadPath walks a dotted path in the payload map.
func payloadPath(payload map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = payload
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// eventScalar renders one projected value as text. Lists (labels, release
// assets) are rendered by name/JSON so a condition can read them without a
// second context shape.
func eventScalar(val any) (string, bool) {
	switch v := val.(type) {
	case string:
		return v, true
	case bool:
		return fmt.Sprintf("%t", v), true
	case int:
		return fmt.Sprintf("%d", v), true
	case int64:
		return fmt.Sprintf("%d", v), true
	case float64:
		return fmt.Sprintf("%v", v), true
	case []string:
		return strings.Join(v, ","), true
	case []map[string]any:
		return joinEventNames(v), true
	case []any:
		// A payload decoded from the run header has JSON's generic list type.
		names := make([]string, 0, len(v))
		for _, item := range v {
			switch val := item.(type) {
			case string:
				names = append(names, val)
			case map[string]any:
				if name, ok := val["name"].(string); ok {
					names = append(names, name)
				}
			}
		}
		sort.Strings(names)
		return strings.Join(names, ","), true
	}
	return "", false
}

// shortRef strips a refs/heads/ prefix (the payload's branch spelling).
func shortRef(ref string) string {
	if branch, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/"); ok {
		return branch
	}
	return strings.TrimSpace(ref)
}

// joinEventNames renders a []map row list as its sorted name column.
func joinEventNames(rows []map[string]any) string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// eventInputs projects the dispatch inputs into the payload's `inputs` object
// (GitHub exposes `github.event.inputs` as the string form of every input).
func eventInputs(inputs map[string]string) map[string]any {
	if len(inputs) == 0 {
		return nil
	}
	out := make(map[string]any, len(inputs))
	for k, v := range inputs {
		out[k] = v
	}
	return out
}

// repoNamePart / ownerPart split "owner/name" for the repository object.
func repoNamePart(repo string) string {
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func ownerPart(repo string) string {
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		return repo[:i]
	}
	return ""
}
