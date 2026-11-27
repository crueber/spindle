package webui

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/notify"
)

// Pure-helper branches the fleet/deployment pages added; these are cheap
// deterministic coverage for the package floor.
func TestWave4WebUIPureHelpers(t *testing.T) {
	if !containsStrFold([]string{"Alpha"}, "alpha") || containsStrFold([]string{"Alpha"}, "beta") {
		t.Fatal("containsStrFold")
	}
	if !isDigits("123") || isDigits("") || isDigits("1234567") || isDigits("12a") {
		t.Fatal("isDigits")
	}
	if yesNo(true) != "yes" || yesNo(false) != "no" {
		t.Fatal("yesNo")
	}
	if settingsPushAuthLabel("") != "no auth" {
		t.Fatal("settingsPushAuthLabel empty")
	}
	if settingsPushAuthLabel("no-such-kind") != "no-such-kind" {
		t.Fatal("settingsPushAuthLabel unknown")
	}
	if settingsPushAuthLabel(settingsPushAuthKinds[0].ID) != settingsPushAuthKinds[0].Label {
		t.Fatal("settingsPushAuthLabel known")
	}
	if gcTaskTitle(true) == gcTaskTitle(false) {
		t.Fatal("gcTaskTitle")
	}
	if got := settingsReportHref("o", "r", SettingsReportCard{Kind: "user", Target: "user:ada"}); got != "/ada" {
		t.Fatalf("user href = %q", got)
	}
	if got := settingsReportHref("o", "r", SettingsReportCard{Kind: "user"}); got != "/o" {
		t.Fatalf("owner href = %q", got)
	}
	if got := settingsReportHref("o", "r", SettingsReportCard{Target: "pull:7"}); got != "/o/r/pull/7" {
		t.Fatalf("pull href = %q", got)
	}
	if got := settingsReportHref("o", "r", SettingsReportCard{Target: "nope"}); got != "/o/r" {
		t.Fatalf("fallback href = %q", got)
	}
}

func TestRotateWebhookGuards(t *testing.T) {
	var s SettingsServices
	if _, werr := s.rotateWebhook(context.Background(), "o", "r", SettingsRepoWrite{}); werr == nil || werr.Status != 503 {
		t.Fatalf("unwired rotate = %+v", werr)
	}
	s.Notify = &notify.Service{}
	if _, werr := s.rotateWebhook(context.Background(), "o", "r", SettingsRepoWrite{Values: map[string]string{"secret": ""}}); werr == nil || werr.Status != 422 {
		t.Fatalf("empty secret = %+v", werr)
	}
}
