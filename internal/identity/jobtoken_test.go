package identity

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// PAR-113: a Threads job token resolves to the minimum repo role its effective
// permissions imply, only inside its own repository, and never to admin.
func TestJobTokenRoleResolution(t *testing.T) {
	s := testService()
	ctx := context.Background()
	mustAccess(t, s, "acme", "priv", VisibilityPrivate, nil)
	mustAccess(t, s, "acme", "other", VisibilityPrivate, nil)

	job := func(repo string, perms map[string]string) auth.Principal {
		return auth.Principal{Name: "ci:r1", JobRepo: repo, JobPerms: perms}
	}
	cases := []struct {
		name string
		p    auth.Principal
		repo string
		want Role
	}{
		{"contents read", job("acme/priv", map[string]string{"contents": "read"}), "priv", RoleRead},
		{"contents write is write", job("acme/priv", map[string]string{"contents": "write"}), "priv", RoleWrite},
		{"pull-requests write is write", job("acme/priv", map[string]string{"pull-requests": "write"}), "priv", RoleWrite},
		{"issues write is triage", job("acme/priv", map[string]string{"issues": "write", "contents": "read"}), "priv", RoleTriage},
		{"issues read is read", job("acme/priv", map[string]string{"issues": "read"}), "priv", RoleRead},
		{"id-token only is no role", job("acme/priv", map[string]string{"id-token": "write", "contents": "none"}), "priv", ""},
		{"packages write is no repository role", job("acme/priv", map[string]string{"packages": "write", "contents": "none"}), "priv", ""},
		{"checks write is no repository role", job("acme/priv", map[string]string{"checks": "write", "contents": "none"}), "priv", ""},
		{"statuses read is no repository role", job("acme/priv", map[string]string{"statuses": "read", "contents": "none"}), "priv", ""},
		{"another repo is no role", job("acme/priv", map[string]string{"contents": "write"}), "other", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := s.Resolve(ctx, "acme", tc.repo, tc.p)
			if got != tc.want {
				t.Fatalf("role = %q, want %q", got, tc.want)
			}
		})
	}

	// The mapped role is enforced through the same gates every package uses.
	writerJob := job("acme/priv", map[string]string{"issues": "write", "contents": "read"})
	if aerr := s.CheckRole(ctx, "acme", "priv", writerJob, RoleTriage); aerr != nil {
		t.Fatalf("issues: write denied triage: %v", aerr)
	}
	if aerr := s.CheckRole(ctx, "acme", "priv", writerJob, RoleWrite); aerr == nil {
		t.Fatal("issues: write resolved to write")
	}
	if aerr := s.CheckRole(ctx, "acme", "priv", writerJob, RoleAdmin); aerr == nil {
		t.Fatal("a job token reached admin (could mint credentials)")
	}
	if aerr := s.CheckRole(ctx, "acme", "priv", writerJob, RoleTriage); aerr != nil {
		t.Fatalf("read on a private repo should be granted to the job's own repo: %v", aerr)
	}
	if aerr := s.CheckRead(ctx, "acme", "priv", job("acme/priv", map[string]string{"contents": "read"})); aerr != nil {
		t.Fatalf("contents: read denied its own private repo: %v", aerr)
	}
	if aerr := s.CheckRead(ctx, "acme", "other", job("acme/priv", map[string]string{"contents": "read"})); aerr == nil {
		t.Fatal("job token read another private repository")
	}
}
