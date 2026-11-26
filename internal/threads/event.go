package threads

import (
	"encoding/json"
	"fmt"
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

// ReleaseAsset is one asset row in the event payload (name + size only: the
// download URLs are server-relative and the bytes are not part of an event).
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
