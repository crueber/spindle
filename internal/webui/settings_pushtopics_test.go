// settings_pushtopics_test.go — the webhooks panel's event filter offers the
// push/ref family on REPO hooks (docs/features/06 §5.3 push amendment), the
// form round-trips those topics, and the org panel does not offer them (an org
// has no repository to push to).
package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestSettingsRepoWebhooksOffersPushTopics(t *testing.T) {
	h := newSeamsHarness(t)
	code, body := seamsGet(t, h, "/acme/monorepo/settings/webhooks")
	if code != http.StatusOK {
		t.Fatalf("GET webhooks = %d", code)
	}
	for _, want := range []string{
		`list="webhook-event-topics"`, `id="webhook-event-topics"`,
		"branch.created", "branch.updated", "branch.deleted",
		"tag.created", "tag.updated", "tag.deleted",
		"push/ref family",
	} {
		settingsMustContain(t, body, want, "push topic vocabulary")
	}
	// The hint line must be announced to the field it describes.
	settingsMustContain(t, body, `aria-describedby="webhook-event-hints"`, "hint association")

	// The form round-trips the new topics through the real CreateHook path.
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/webhooks", url.Values{
		"url":    {"http://127.0.0.1:9/hook"},
		"events": {"push,branch.created"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("create with push topics = %d (%s)", code, body)
	}
	hooks, err := h.notes.ListHooks(context.Background(), "acme", "monorepo")
	if err != nil || len(hooks) != 1 {
		t.Fatalf("hooks = %+v err %v", hooks, err)
	}
	if got := strings.Join(hooks[0].Events, ","); got != "push,branch.created" {
		t.Fatalf("stored events = %q", got)
	}
	code, body = seamsGet(t, h, "/acme/monorepo/settings/webhooks")
	if code != http.StatusOK {
		t.Fatalf("GET configured = %d", code)
	}
	settingsMustContain(t, body, "push, branch.created", "events round-trip in the row")
}

func TestSettingsOrgWebhooksDoNotOfferPushTopics(t *testing.T) {
	f := settingsOrgFakes(true)
	f.deps.OrgView = func(_ context.Context, org string, _ auth.Principal) (*SettingsOrgPage, error) {
		return &SettingsOrgPage{Org: org, CanManage: true, WebhooksWired: true}, nil
	}
	s := newSettingsServer(t, f)
	rec := settingsDo(t, s, http.MethodGet, "/acme/settings?tab=webhooks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("org webhooks = %d", rec.Code)
	}
	body := rec.Body.String()
	// The panel still renders its filter vocabulary, but never the push family.
	settingsMustContain(t, body, `id="webhook-event-topics"`, "org vocabulary renders")
	settingsMustContain(t, body, "commented", "collaboration action offered")
	settingsMustNotContain(t, body, "branch.created", "push topics are repo-scoped")
	settingsMustNotContain(t, body, "push/ref family", "no push promise on org")
}
