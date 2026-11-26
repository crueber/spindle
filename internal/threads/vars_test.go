package threads

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestVariablesScopeAndSecrecy pins PAR-035: repository variables override
// organization variables, values are stored sealed and never returned, and a
// reserved prefix is refused at save.
func TestVariablesScopeAndSecrecy(t *testing.T) {
	e := newTestEnv(t)
	if _, err := NewKeyring(strings.Repeat("v", 32), ""); err != nil {
		t.Fatal(err)
	}
	k, _ := NewKeyring(strings.Repeat("v", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}

	if err := e.svc.SetOrgVariable(ctx(), "o", "REGION", "eu-org", admin); err != nil {
		t.Fatalf("org variable: %v", err)
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "REGION", "eu-repo", admin); err != nil {
		t.Fatalf("repo variable: %v", err)
	}
	if err := e.svc.SetVariable(ctx(), "o/r", "ONLY_REPO", "one", admin); err != nil {
		t.Fatalf("repo-only variable: %v", err)
	}
	// Reserved prefixes are refused at save.
	for _, bad := range []string{"GITHUB_TOKEN", "SPINDLE_REF", "FORGEJO_TOKEN", "RUNNER_NAME"} {
		if err := e.svc.SetVariable(ctx(), "o/r", bad, "x", admin); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", bad, err)
		}
	}
	vals, err := e.svc.variableValues(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if vals["REGION"] != "eu-repo" || vals["ONLY_REPO"] != "one" {
		t.Fatalf("resolved = %+v (repository must override organization)", vals)
	}
	// Values never come back from the read surface.
	list, err := e.svc.ListVariables(ctx(), "o/r", admin)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, v := range list {
		if strings.Contains(v.Name, "eu-") {
			t.Fatalf("a value leaked through the name: %+v", v)
		}
	}
	// The stored document is sealed (no plaintext in the bucket).
	raw, _, err := store.GetBytes(ctx(), e.store, VarsKey("o", "r"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "eu-repo") || !strings.Contains(string(raw), "sealed") {
		t.Fatalf("variable stored in the clear: %s", raw)
	}
}

// TestEventPayloadProjection pins PAR-033's bounded projection: only the
// documented fields reach `github.event.*`, a body is truncated, and an
// over-cap payload is refused instead of silently trimmed.
func TestEventPayloadProjection(t *testing.T) {
	payload := IssueCommentEventPayload("o/r", "created", "ada", strings.Repeat("x", maxEventBodyBytes+100),
		7, "title", "open", []string{"bug"}, "ada")
	fields := EventFieldsOf(payload)
	if fields["event.action"] != "created" || fields["event.issue.number"] != "7" ||
		fields["event.sender.login"] != "ada" {
		t.Fatalf("fields = %+v", fields)
	}
	if body := fields["event.comment.body"]; len(body) != maxEventBodyBytes {
		t.Fatalf("comment body = %d bytes, want the %d-byte cap", len(body), maxEventBodyBytes)
	}
	if fields["event.issue.labels"] != "bug" {
		t.Fatalf("labels projection = %q", fields["event.issue.labels"])
	}
	raw, err := EncodeEventPayload(payload)
	if err != nil || len(raw) == 0 {
		t.Fatalf("encode = %d bytes, %v", len(raw), err)
	}
	huge := map[string]any{"big": strings.Repeat("z", maxEventPayloadBytes)}
	if _, err := EncodeEventPayload(huge); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("over-cap payload err = %v, want ErrUnprocessable", err)
	}
}
