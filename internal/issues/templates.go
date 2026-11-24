package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// templates.go owns §15: the repository issue templates discovered from
// `.spindle/ISSUE_TEMPLATE/*.md` and cached as one bucket projection.
//
// Discovery is a git read and runs ONLY on the push path
// (Service.RefreshTemplatesFromDefaultBranch, called from the OnPush hook).
// Every request path — the composer included — reads the cached projection
// with one exact-key GET (Service.Templates), so no page render shells out to
// git and no request pays a tree walk (law 6).
//
// GitHub issue *forms* (`.github/ISSUE_TEMPLATE/*.yml` schemas) are NOT
// implemented: only `.spindle/ISSUE_TEMPLATE` is read, and only markdown
// files. A repository with no such directory keeps today's blank composer.

// IssueTemplate is one offerable template, as stored and as the composer
// renders it. Body is the template body after the front matter.
type IssueTemplate struct {
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	About     string   `json:"about,omitempty"`
	Title     string   `json:"title,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Body      string   `json:"body"`
}

// TemplateDoc is repos/<o>/<r>/meta/issue_templates.json (CAS'd, derived).
// Commit pins the discovery source; RefreshedAt is the write time.
type TemplateDoc struct {
	Version     int             `json:"version"`
	Commit      string          `json:"commit,omitempty"`
	RefreshedAt string          `json:"refreshed_at,omitempty"`
	Templates   []IssueTemplate `json:"templates"`
}

// MaxIssueTemplates bounds one projection (the chooser is a short menu).
const MaxIssueTemplates = 20

// TemplateKey is the template projection key.
func TemplateKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/issue_templates.json"
}

// Templates reads the cached projection (one exact-key GET; absent = empty).
// Read-gated by the caller's page/route gate; the projection carries no
// content the repo read gate would not already allow.
func (s *Service) Templates(ctx context.Context, owner, repo string) (*TemplateDoc, error) {
	raw, _, err := s.getJSON(ctx, TemplateKey(owner, repo))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &TemplateDoc{Templates: []IssueTemplate{}}, nil
	}
	var doc TemplateDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: issue_templates.json: %v", ErrCorrupt, jerr)
	}
	if doc.Templates == nil {
		doc.Templates = []IssueTemplate{}
	}
	return &doc, nil
}

// ParseIssueTemplate parses one template file: optional `---` front matter
// with `name`, `about`, `title`, `labels`, `assignees` (comma lists or
// `[a, b]`), then the body. Missing front matter is legal (the file stem
// names the template, the whole file is the body). An UNTERMINATED front
// matter block or a file whose body exceeds MaxBodyBytes is malformed and
// reported as an error, so the caller skips the file instead of offering a
// template that would prefill front-matter junk.
func ParseIssueTemplate(filePath string, raw []byte) (IssueTemplate, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	tpl := IssueTemplate{Path: filePath, Body: text}
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		filePath = filePath[i+1:]
	}
	tpl.Name = strings.TrimSuffix(filePath, path.Ext(filePath))

	if strings.HasPrefix(text, "---\n") {
		rest := text[len("---\n"):]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return IssueTemplate{}, fmt.Errorf("%w: %s: unterminated front matter", ErrInvalid, filePath)
		}
		front := rest[:end]
		body := rest[end+len("\n---"):]
		body = strings.TrimLeft(body, "\n")
		if body != "" {
			body = strings.TrimSuffix(body, "\n") + "\n"
		}
		if err := parseTemplateFrontMatter(&tpl, front); err != nil {
			return IssueTemplate{}, err
		}
		tpl.Body = body
	}
	if len(tpl.Body) > MaxBodyBytes {
		return IssueTemplate{}, fmt.Errorf("%w: %s: body exceeds %d bytes", ErrInvalid, filePath, MaxBodyBytes)
	}
	if strings.TrimSpace(tpl.Name) == "" {
		tpl.Name = "Template"
	}
	return tpl, nil
}

// parseTemplateFrontMatter fills the template from `key: value` lines. It is
// deliberately a small subset (no nesting, no anchors): an unknown key is
// ignored (forward compatible), a malformed assignment is an error so the
// file is skipped rather than half-applied.
func parseTemplateFrontMatter(tpl *IssueTemplate, front string) error {
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("%w: %s: bad front matter line %q", ErrInvalid, tpl.Path, line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = templateScalar(val)
		switch key {
		case "name":
			tpl.Name = val
		case "about":
			tpl.About = val
		case "title":
			tpl.Title = val
		case "labels":
			tpl.Labels = templateList(val)
		case "assignees":
			tpl.Assignees = templateList(val)
		}
	}
	return nil
}

// templateScalar trims a front-matter value and strips one layer of quotes.
func templateScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// templateList parses `a, b` or `[a, b]` into a bounded, deduped list.
func templateList(v string) []string {
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = templateScalar(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return uniqSorted(out)
}

// RefreshTemplates re-discovers the templates at commit and CASes the
// projection. changed=false when the commit and the template list are
// unchanged (a push that touched no template writes nothing). A git read or
// per-file read failure skips that file (logged); a total discovery failure
// is returned so the caller can log it, and the previous projection keeps
// serving.
//
// ### Concurrency: one bounded CAS on the projection object; two pushes
// racing the same refresh write the same content and the loser detects the
// no-op. No lock, and nothing here runs on a request path.
func (s *Service) RefreshTemplates(ctx context.Context, owner, repo, commit string) (bool, error) {
	if s.TemplateGit == nil {
		return false, nil
	}
	commit = strings.TrimSpace(commit)
	if !templateSHA(commit) {
		return false, fmt.Errorf("%w: invalid commit %q", ErrInvalid, commit)
	}
	files, err := s.TemplateGit.ListTemplateFiles(ctx, repoName(owner, repo), commit)
	if err != nil {
		return false, err
	}
	sort.Strings(files)
	list := make([]IssueTemplate, 0, len(files))
	for _, f := range files {
		if len(list) >= MaxIssueTemplates {
			break
		}
		raw, rerr := s.TemplateGit.ReadFile(ctx, repoName(owner, repo), commit, f)
		if rerr != nil {
			s.logTemplateSkip(owner, repo, f, rerr)
			continue
		}
		tpl, perr := ParseIssueTemplate(f, raw)
		if perr != nil {
			s.logTemplateSkip(owner, repo, f, perr)
			continue
		}
		list = append(list, tpl)
	}
	changed := false
	now := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, TemplateKey(owner, repo), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		doc := &TemplateDoc{Templates: []IssueTemplate{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: issue_templates.json: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Commit == commit && reflect.DeepEqual(templateListNorm(doc.Templates), templateListNorm(list)) {
			return nil, false, nil
		}
		doc.Commit = commit
		doc.RefreshedAt = now
		doc.Templates = list
		doc.Version++
		changed = true
		out, _ := json.Marshal(doc)
		return out, true, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// templateListNorm normalizes the stored/derived lists for the no-op compare
// (slices never nil, so a JSON round-trip compares equal).
func templateListNorm(in []IssueTemplate) []IssueTemplate {
	out := make([]IssueTemplate, len(in))
	copy(out, in)
	for i := range out {
		if out[i].Labels == nil {
			out[i].Labels = []string{}
		}
		if out[i].Assignees == nil {
			out[i].Assignees = []string{}
		}
	}
	return out
}

// logTemplateSkip records a per-file discovery skip (nil-logger-safe).
func (s *Service) logTemplateSkip(owner, repo, file string, err error) {
	if s.Log == nil {
		return
	}
	s.Log.Warn("issues: skipped issue template", "repo", repoName(owner, repo), "path", file, "err", err)
}

// RefreshTemplatesFromDefaultBranch resolves HEAD in the repo's serving copy
// and refreshes the projection from it. This is the post-push hook body: it
// runs off the request path, so the composer never pays for it.
func (s *Service) RefreshTemplatesFromDefaultBranch(ctx context.Context, repo string) (bool, error) {
	if s.TemplateGit == nil {
		return false, nil
	}
	parts := strings.SplitN(strings.TrimSpace(repo), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false, fmt.Errorf("%w: bad repo id %q", ErrInvalid, repo)
	}
	commit, err := s.TemplateGit.ResolveRef(ctx, repo, "HEAD")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil // unborn HEAD: no templates to discover yet
		}
		return false, err
	}
	return s.RefreshTemplates(ctx, parts[0], parts[1], commit)
}
