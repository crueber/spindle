// issue_templates.go — docs/features/02_issues.md §15 composition: refresh the
// cached issue-template projection when a push lands. Discovery is a git read,
// so it must never run on a request path (law 6); the post-push hook is the
// one place a repository's `.spindle/ISSUE_TEMPLATE` tree is walked and the
// result CASed into meta/issue_templates.json. The composer then reads that
// object with one exact-key GET.
package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server"
)

// issueTemplatesPushTimeout bounds one refresh pass (off the request path; the
// deadline keeps a hung git/store call from leaking a goroutine forever).
const issueTemplatesPushTimeout = 30 * time.Second

// issuesTemplatesOnPushOf builds the on-push template-refresh hook. Any
// branch create/update triggers it: the refresh always reads the default
// branch (HEAD in the serving copy), so a feature-branch push costs one
// discovery walk and leaves the projection correct. Deletes and tag pushes
// cannot change template content. Nil service / nil git reader → nil hook.
func issuesTemplatesOnPushOf(c *collabWiring) func(id git.RepoId, refs []server.PushedRef, byJob bool) {
	if c == nil || c.issuesSvc == nil || c.issuesSvc.TemplateGit == nil {
		return nil
	}
	svc := c.issuesSvc
	return func(id git.RepoId, refs []server.PushedRef, byJob bool) {
		if byJob {
			return // a Threads job token's push never re-discovers templates
		}
		interesting := false
		for _, ref := range refs {
			if strings.HasPrefix(ref.Ref, "refs/heads/") && !isZeroOID(ref.New) {
				interesting = true
				break
			}
		}
		if !interesting {
			return
		}
		repo := id.String()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), issueTemplatesPushTimeout)
			defer cancel()
			if _, err := svc.RefreshTemplatesFromDefaultBranch(ctx, repo); err != nil {
				slog.Default().Warn("issues: refresh issue templates", "repo", repo, "err", err)
			}
		}()
	}
}
