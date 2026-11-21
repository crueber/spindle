package tags

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestListTagEntries(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tagRefs = []TagRef{
		{Name: "v2.0.0", Object: testTagOid, Commit: testSHA, CreatedAt: "2026-01-02T03:04:05Z", Author: "jane", Annotated: true},
		{Name: "v1.0.0", Object: testSHA, Commit: testSHA, CreatedAt: "2026-01-01T00:00:00Z", Author: "joe"},
	}
	x.refs.existing["v2.0.0"] = testSHA
	svc := x.svc
	svc.SetReleaseExists(func(ctx context.Context, owner, repo, tag string) (bool, error) {
		return tag == "v2.0.0", nil
	})
	// The list gate is read: a plain reader passes.
	x.roles.grant("o", "r", "jane", "read")
	entries, err := svc.ListTagEntries(ctx(), "o", "r", writer())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries %d", len(entries))
	}
	if entries[0].Name != "v2.0.0" || !entries[0].Annotated || !entries[0].Release || entries[0].Date == "" {
		t.Fatalf("row %+v", entries[0])
	}
	if entries[1].Release || entries[1].Annotated {
		t.Fatalf("lightweight row %+v", entries[1])
	}
	// HTTP twin.
	w := doReq(t, x, "GET", "/o/r/api/tags", "", writerHeaders())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"release":true`) {
		t.Fatalf("wire: %d %s", w.Code, w.Body.String())
	}
}

func TestDeleteTagAllowsAndPublishesDelete(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	if err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1"); err != nil {
		t.Fatal(err)
	}
	if len(x.refs.deleted) != 1 || x.refs.deleted[0].sha != testSHA {
		t.Fatalf("deleted %+v", x.refs.deleted)
	}
	if _, ok := x.refs.existing["v1"]; ok {
		t.Fatal("ref still present in fake")
	}
	x.refs.existing["v1"] = testSHA // re-create for the wire twin
	w := doReq(t, x, "DELETE", "/o/r/api/refs/tags/v1", "", writerHeaders())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "refs/tags/v1") {
		t.Fatalf("wire: %d %s", w.Code, w.Body.String())
	}
}

func TestDeleteTagPolicyDenies(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	seedPolicy(t, x, `{"version":1,"rules":[{"name":"lock","match":{"refs":["refs/tags/v1"]},"effect":{"protect":{"restricts":["delete"]}}}]}`)
	err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("err %v", err)
	}
	if len(x.refs.deleted) != 0 {
		t.Fatal("deleted despite policy")
	}
	w := doReq(t, x, "DELETE", "/o/r/api/refs/tags/v1", "", writerHeaders())
	if w.Code != 403 {
		t.Fatalf("wire: %d", w.Code)
	}
}

func TestDeleteTagReleaseBackedRefused(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	x.svc.SetReleaseExists(func(ctx context.Context, owner, repo, tag string) (bool, error) {
		return tag == "v1", nil
	})
	err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "backs a release") {
		t.Fatalf("err %v", err)
	}
	if len(x.refs.deleted) != 0 {
		t.Fatal("deleted a release-backed tag")
	}
	w := doReq(t, x, "DELETE", "/o/r/api/refs/tags/v1", "", writerHeaders())
	if w.Code != 409 || !strings.Contains(w.Body.String(), "delete the release first") {
		t.Fatalf("wire: %d %s", w.Code, w.Body.String())
	}
}

func TestDeleteTagUnknownAndRoles(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	if err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := x.svc.DeleteTag(ctx(), "o", "r", auth.Principal{Name: "reader"}, "v1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader: %v", err)
	}
	x.roles.grant("o", "r", "reader", "read")
	if err := x.svc.DeleteTag(ctx(), "o", "r", auth.Principal{Name: "reader"}, "v1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader: %v", err)
	}
}

func TestDeleteTagConflictOnMove(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	// Resolve returns testSHA, but the fake holds a different oid: the CAS
	// delete loses.
	x.refs.existing["v1"] = "ffffffffffffffffffffffffffffffffffffffff"
	err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v", err)
	}
}

func TestDeleteTagPolicyCorruptFailsClosed(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	seedPolicy(t, x, `{not json`)
	if err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err %v", err)
	}
}

func TestDeleteTagUnwired(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.commits["refs/tags/v1"] = testSHA
	x.refs.existing["v1"] = testSHA
	x.svc.Refs = nil
	if err := x.svc.DeleteTag(ctx(), "o", "r", writer(), "v1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestListTagEntriesUnwired(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("o", "r", "jane", "read")
	x.svc.Git = nil
	if _, err := x.svc.ListTagEntries(ctx(), "o", "r", writer()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
}
