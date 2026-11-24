package doctor

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store/proto"
)

// expect is the table-test assertion for one decoder verdict.
func expect(t *testing.T, name string, got *Problem, wantCheck string) {
	t.Helper()
	if wantCheck == "" {
		if got != nil {
			t.Fatalf("%s: got %s %q, want success", name, got.Check, got.Reason)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s: got success, want %s", name, wantCheck)
	}
	if got.Check != wantCheck {
		t.Fatalf("%s: check = %s (%q), want %s", name, got.Check, got.Reason, wantCheck)
	}
	if strings.TrimSpace(got.Reason) == "" {
		t.Fatalf("%s: empty reason", name)
	}
}

func TestDecodeManifest(t *testing.T) {
	base := func() *proto.Manifest {
		return &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "a/b", ObjectFormat: "sha1"}
	}
	m := base()
	valid := m.Marshal()

	bad := base()
	bad.FormatVersion = 9
	missing := base()
	missing.FormatVersion = 0
	noRepo := base()
	noRepo.Repo = ""
	mismatch := base()
	mismatch.Repo = "c/d"
	badFormat := base()
	badFormat.ObjectFormat = "md5"
	segKey := base()
	segKey.LogSegments = []*proto.LogSegmentRef{{FirstSeq: 1, LastSeq: 2}}
	segBack := base()
	segBack.LogSegments = []*proto.LogSegmentRef{{Key: "log/x.pb", FirstSeq: 5, LastSeq: 4}}
	pack := base()
	pack.Packs = []*proto.PackRef{{}}
	cp := base()
	cp.Checkpoint = &proto.CheckpointRef{Seq: 1}
	segNil := base()
	segNil.LogSegments = []*proto.LogSegmentRef{nil}
	packNil := base()
	packNil.Packs = []*proto.PackRef{nil}

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"valid", valid, ""},
		{"valid with nil entries", segNil.Marshal(), ""},
		{"invalid protobuf", []byte{0x0f}, CheckDecode},
		{"missing version", missing.Marshal(), CheckVersion},
		{"unknown version", bad.Marshal(), CheckVersion},
		{"missing repo", noRepo.Marshal(), CheckRequired},
		{"repo mismatch", mismatch.Marshal(), CheckRequired},
		{"bad object format", badFormat.Marshal(), CheckRequired},
		{"segment without key", segKey.Marshal(), CheckRequired},
		{"segment backwards", segBack.Marshal(), CheckRequired},
		{"pack without checksum", pack.Marshal(), CheckRequired},
		{"checkpoint without key", cp.Marshal(), CheckRequired},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeManifest("repos/a/b/manifest.pb", tc.raw), tc.want)
	}
	if r := decodeManifest("nonsense.pb", valid); r != nil {
		// repoFromKey fails on a non-repo path; the manifest is still valid.
		t.Fatalf("non-repo key: %+v", r)
	}
	_ = packNil
}

func TestDecodeRefSnapshot(t *testing.T) {
	valid := &proto.RefSnapshot{
		Seq:          1,
		ObjectFormat: "sha1",
		Refs: []*proto.Ref{
			{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)},
			{Name: "HEAD", Oid: ""},
		},
		HeadTarget: "refs/heads/main",
	}
	badFormat := &proto.RefSnapshot{ObjectFormat: "md5"}
	noName := &proto.RefSnapshot{ObjectFormat: "sha1", Refs: []*proto.Ref{{Oid: strings.Repeat("a", 40)}}}
	emptyOid := &proto.RefSnapshot{ObjectFormat: "sha1", Refs: []*proto.Ref{{Name: "refs/heads/x"}}}
	badName := &proto.RefSnapshot{ObjectFormat: "sha1", Refs: []*proto.Ref{{Name: "heads/x", Oid: strings.Repeat("a", 40)}}}
	shortOid := &proto.RefSnapshot{ObjectFormat: "sha256", Refs: []*proto.Ref{{Name: "refs/heads/x", Oid: strings.Repeat("a", 40)}}}
	sha256 := &proto.RefSnapshot{ObjectFormat: "sha256", Refs: []*proto.Ref{{Name: "refs/heads/x", Oid: strings.Repeat("a", 64)}}}
	nilRef := &proto.RefSnapshot{ObjectFormat: "sha1", Refs: []*proto.Ref{nil}}

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"valid", valid.Marshal(), ""},
		{"valid sha256", sha256.Marshal(), ""},
		{"nil ref", nilRef.Marshal(), ""},
		{"invalid", []byte{0x0f}, CheckDecode},
		{"bad format", badFormat.Marshal(), CheckRequired},
		{"missing name", noName.Marshal(), CheckRequired},
		{"empty oid", emptyOid.Marshal(), CheckRequired},
		{"bad ref name", badName.Marshal(), CheckRequired},
		{"short sha256 oid", shortOid.Marshal(), CheckRequired},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeRefSnapshot("repos/a/b/checkpoints/0000000000000001/refs.pb", tc.raw), tc.want)
	}
}

func TestDecodeAccess(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"valid", `{"version":1,"visibility":"public","role_bindings":[],"updated_at":"x"}`, ""},
		{"private", `{"version":2,"visibility":"private"}`, ""},
		{"authenticated", `{"version":1,"visibility":"authenticated"}`, ""},
		{"empty", ``, CheckDecode},
		{"array", `[]`, CheckDecode},
		{"truncated", `{"version":`, CheckDecode},
		{"missing version", `{"visibility":"public"}`, CheckVersion},
		{"bad visibility", `{"version":1,"visibility":"world"}`, CheckRequired},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeAccess("repos/a/b/access.json", []byte(tc.raw)), tc.want)
	}
}

func TestDecodeIdentity(t *testing.T) {
	cases := []struct{ name, key, raw, want string }{
		{"user", "users/a/user.json", `{"version":1,"username":"a"}`, ""},
		{"user missing username", "users/a/user.json", `{"version":1}`, CheckRequired},
		{"user missing version", "users/a/user.json", `{"username":"a"}`, CheckVersion},
		{"user unknown version", "users/a/user.json", `{"version":2,"username":"a"}`, CheckVersion},
		{"user bad version type", "users/a/user.json", `{"version":"1","username":"a"}`, CheckDecode},
		{"profile", "users/a/profile.json", `{"version":1,"principal":"a"}`, ""},
		{"profile missing principal", "users/a/profile.json", `{"version":1}`, CheckRequired},
		{"profile missing version", "users/a/profile.json", `{"principal":"a"}`, CheckVersion},
		{"org", "orgs/o/org.json", `{"version":1,"org":"o"}`, ""},
		{"org missing org", "orgs/o/org.json", `{"version":1}`, CheckRequired},
		{"org missing version", "orgs/o/org.json", `{"org":"o"}`, CheckVersion},
		{"members", "orgs/o/members.json", `{"version":1,"members":[]}`, ""},
		{"members missing version", "orgs/o/members.json", `{"members":[]}`, CheckVersion},
		{"blocks", "users/a/blocks.json", `{"version":1,"blocked":[]}`, ""},
		{"blocks missing version", "users/a/blocks.json", `{"blocked":[]}`, CheckVersion},
		{"alias", "users/by-email/a%40b/ref.json", `{"version":1,"username":"a"}`, ""},
		{"alias missing username", "users/by-email/a%40b/ref.json", `{"version":1}`, CheckRequired},
		{"alias missing version", "users/by-email/a%40b/ref.json", `{"username":"a"}`, CheckVersion},
		{"team", "orgs/o/teams/t.json", `{"version":1,"slug":"t"}`, ""},
		{"team unknown version", "orgs/o/teams/t.json", `{"version":3}`, CheckVersion},
		{"invitation unversioned", "orgs/o/invitations/i.json", `{"id":"i"}`, ""},
		{"user bad json", "users/a/user.json", `{`, CheckDecode},
		{"profile bad json", "users/a/profile.json", `{`, CheckDecode},
		{"org bad json", "orgs/o/org.json", `{`, CheckDecode},
		{"members bad json", "orgs/o/members.json", `{`, CheckDecode},
		{"blocks bad json", "users/a/blocks.json", `{`, CheckDecode},
		{"alias bad json", "users/by-email/a%40b/ref.json", `{`, CheckDecode},
		{"owner profile", "owners/o/profile.json", `{"display_name":"O","location":"x","timezone":"UTC","bio_markdown":""}`, ""},
		{"generic bad json", "orgs/o/teams/t.json", `{`, CheckDecode},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeIdentity(tc.key, []byte(tc.raw)), tc.want)
	}
}

func TestDecodeCollab(t *testing.T) {
	const sha = "0123456789012345678901234567890123456789"
	cases := []struct{ name, key, raw, want string }{
		{"issue thread", "repos/a/b/issues/000001/thread.json", `{"num":1,"kind":"issue","title":"t","state":"open","version":1}`, ""},
		{"pr thread", "repos/a/b/issues/000002/thread.json", `{"num":2,"kind":"pr","title":"t","state":"open","version":1}`, ""},
		{"thread bad version", "repos/a/b/issues/000001/thread.json", `{"num":1,"kind":"issue","title":"t","state":"open","version":0}`, CheckVersion},
		{"thread bad num", "repos/a/b/issues/000001/thread.json", `{"num":0,"kind":"issue","title":"t","state":"open","version":1}`, CheckRequired},
		{"thread bad kind", "repos/a/b/issues/000001/thread.json", `{"num":1,"kind":"discussion","title":"t","state":"open","version":1}`, CheckRequired},
		{"thread no title", "repos/a/b/issues/000001/thread.json", `{"num":1,"kind":"issue","state":"open","version":1}`, CheckRequired},
		{"thread no state", "repos/a/b/issues/000001/thread.json", `{"num":1,"kind":"issue","title":"t","version":1}`, CheckRequired},

		{"review thread", "repos/a/b/pulls/000001/threads/t1/thread.json", `{"tid":"t1","num":1,"kind":"review_thread","version":1}`, ""},
		{"review thread bad version", "repos/a/b/pulls/000001/threads/t1/thread.json", `{"tid":"t1","num":1,"kind":"review_thread","version":0}`, CheckVersion},
		{"review thread bad num", "repos/a/b/pulls/000001/threads/t1/thread.json", `{"tid":"t1","num":0,"kind":"review_thread","version":1}`, CheckRequired},
		{"review thread no tid", "repos/a/b/pulls/000001/threads/t1/thread.json", `{"num":1,"kind":"review_thread","version":1}`, CheckRequired},
		{"review thread bad kind", "repos/a/b/pulls/000001/threads/t1/thread.json", `{"tid":"t1","num":1,"kind":"thread","version":1}`, CheckRequired},
		{"review thread bad json", "repos/a/b/pulls/000001/threads/t1/thread.json", `{`, CheckDecode},

		{"pr", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"pr","version":1,"base":{"repo":"a/b","ref":"refs/heads/main","sha":"` + sha + `"},"head":{"repo":"a/b","ref":"refs/heads/t","sha":"` + sha + `"}}`, ""},
		{"pr bad version", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"pr","version":0,"base":{"repo":"a/b","ref":"r","sha":"s"},"head":{"repo":"a/b","ref":"r","sha":"s"}}`, CheckVersion},
		{"pr bad num", "repos/a/b/pulls/000001/pr.json", `{"num":0,"kind":"pr","version":1,"base":{"repo":"a/b","ref":"r","sha":"s"},"head":{"repo":"a/b","ref":"r","sha":"s"}}`, CheckRequired},
		{"pr bad kind", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"issue","version":1,"base":{"repo":"a/b","ref":"r","sha":"s"},"head":{"repo":"a/b","ref":"r","sha":"s"}}`, CheckRequired},
		{"pr missing base repo", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"pr","version":1,"base":{"ref":"r","sha":"s"},"head":{"repo":"a/b","ref":"r","sha":"s"}}`, CheckRequired},
		{"pr missing head ref", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"pr","version":1,"base":{"repo":"a/b","ref":"r","sha":"s"},"head":{"repo":"a/b","sha":"s"}}`, CheckRequired},
		{"pr missing head sha", "repos/a/b/pulls/000001/pr.json", `{"num":1,"kind":"pr","version":1,"base":{"repo":"a/b","ref":"r","sha":"s"},"head":{"repo":"a/b","ref":"r"}}`, CheckRequired},
		{"pr bad json", "repos/a/b/pulls/000001/pr.json", `{`, CheckDecode},

		{"mergeable", "repos/a/b/pulls/000001/mergeable.json", `{"base_ref":"r","base_sha":"b","head_sha":"h","state":"clean"}`, ""},
		{"mergeable bad state", "repos/a/b/pulls/000001/mergeable.json", `{"base_ref":"r","base_sha":"b","head_sha":"h","state":"maybe"}`, CheckRequired},
		{"mergeable missing sha", "repos/a/b/pulls/000001/mergeable.json", `{"base_ref":"r","base_sha":"b","state":"clean"}`, CheckRequired},
		{"mergeable bad json", "repos/a/b/pulls/000001/mergeable.json", `{`, CheckDecode},

		{"issues index", "repos/a/b/issues/index.json", `{"version":1,"open":[],"closed_recent":[]}`, ""},
		{"issues index bad version", "repos/a/b/issues/index.json", `{"open":[]}`, CheckVersion},
		{"issues index bad json", "repos/a/b/issues/index.json", `{`, CheckDecode},
		{"review requests", "repos/a/b/pulls/000001/review-requests.json", `{"version":1,"reviewers":[]}`, ""},
		{"review requests bad version", "repos/a/b/pulls/000001/review-requests.json", `{"reviewers":[]}`, CheckVersion},
		{"review requests bad json", "repos/a/b/pulls/000001/review-requests.json", `{`, CheckDecode},
		{"forks index", "repos/a/b/meta/forks.json", `{"version":1,"forks":[]}`, ""},
		{"forks index bad version", "repos/a/b/meta/forks.json", `{"forks":[]}`, CheckVersion},
		{"forks index bad json", "repos/a/b/meta/forks.json", `{`, CheckDecode},
		{"fork doc", "repos/a/b/fork.json", `{"parent":"p/q","version":1}`, ""},
		{"fork doc no parent", "repos/a/b/fork.json", `{"version":1}`, CheckRequired},
		{"fork doc bad version", "repos/a/b/fork.json", `{"parent":"p/q"}`, CheckVersion},
		{"fork doc bad json", "repos/a/b/fork.json", `{`, CheckDecode},

		{"issue event", "repos/a/b/issues/000001/events/000000000001.json", `{"seq":1,"type":"opened"}`, ""},
		{"issue event bad seq", "repos/a/b/issues/000001/events/000000000001.json", `{"seq":0,"type":"opened"}`, CheckRequired},
		{"issue event no type", "repos/a/b/issues/000001/events/000000000001.json", `{"seq":1}`, CheckRequired},
		{"issue event bad json", "repos/a/b/issues/000001/events/000000000001.json", `{`, CheckDecode},

		{"review thread event", "repos/a/b/pulls/000001/threads/t1/events/000000000001.json", `{"kind":"review_thread_comment","seq":1}`, ""},
		{"review thread event bad seq", "repos/a/b/pulls/000001/threads/t1/events/000000000001.json", `{"kind":"review_thread_comment","seq":0}`, CheckRequired},
		{"review thread event no kind", "repos/a/b/pulls/000001/threads/t1/events/000000000001.json", `{"seq":1}`, CheckRequired},
		{"review thread event bad json", "repos/a/b/pulls/000001/threads/t1/events/000000000001.json", `{`, CheckDecode},

		{"review", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"kind":"review","seq":1,"state":"APPROVED","commit_sha":"` + sha + `"}`, ""},
		{"review dismissal", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"kind":"review_dismissed","seq":1,"state":"WHATEVER"}`, ""},
		{"review bad seq", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"kind":"review","seq":0,"state":"APPROVED"}`, CheckRequired},
		{"review no kind", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"seq":1,"state":"APPROVED"}`, CheckRequired},
		{"review bad state", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"kind":"review","seq":1,"state":"LGTM"}`, CheckRequired},
		{"review bad sha", "repos/a/b/pulls/000001/reviews/000000000001.json", `{"kind":"review","seq":1,"state":"APPROVED","commit_sha":"xyz"}`, CheckRequired},
		{"review bad json", "repos/a/b/pulls/000001/reviews/000000000001.json", `{`, CheckDecode},

		{"generic object", "repos/a/b/pulls/000001/extra.json", `{"anything":true}`, ""},
		{"generic invalid", "repos/a/b/pulls/000001/extra.json", `{`, CheckDecode},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeCollab(tc.key, []byte(tc.raw)), tc.want)
	}
}

func TestDecodePackage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct{ name, raw, want string }{
		{"image", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + digest + `"},"layers":[{"digest":"` + digest + `"}]}`, ""},
		{"index", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + digest + `"}]}`, ""},
		{"index bad digest", `{"schemaVersion":2,"mediaType":"m","manifests":[{"digest":"nope"}]}`, CheckRequired},
		{"missing version", `{"mediaType":"m","config":{"digest":"` + digest + `"}}`, CheckVersion},
		{"unknown version", `{"schemaVersion":3,"mediaType":"m"}`, CheckVersion},
		{"missing media type", `{"schemaVersion":2,"config":{"digest":"` + digest + `"}}`, CheckRequired},
		{"no config no manifests", `{"schemaVersion":2,"mediaType":"m"}`, CheckRequired},
		{"bad config digest", `{"schemaVersion":2,"mediaType":"m","config":{"digest":"x"}}`, CheckRequired},
		{"bad layer digest", `{"schemaVersion":2,"mediaType":"m","config":{"digest":"` + digest + `"},"layers":[{"digest":"x"}]}`, CheckRequired},
		{"invalid json", `{`, CheckDecode},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodePackage("repos/a/b/packages/registry/tags/latest", []byte(tc.raw)), tc.want)
	}
}

func TestDecodeThreads(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ name, key, raw, want string }{
		{"queue index", "threads/queue/index.json", `{"version":1,"queued":[{"repo":"a/b","run":"r","job":"j"}]}`, ""},
		{"queue index bad row", "threads/queue/index.json", `{"version":1,"queued":[{"repo":"a/b"}]}`, CheckRequired},
		{"queue index bad version", "threads/queue/index.json", `{"queued":[]}`, CheckVersion},
		{"queue index bad json", "threads/queue/index.json", `{`, CheckDecode},

		{"queue head", "repos/a/b/threads/queue/head.json", `{"version":1,"seq":"s","pending":[]}`, ""},
		{"queue head bad version", "repos/a/b/threads/queue/head.json", `{"seq":"s"}`, CheckVersion},
		{"queue head bad json", "repos/a/b/threads/queue/head.json", `{`, CheckDecode},
		{"queue entry", "repos/a/b/threads/queue/0001.json", `{"seq":"1","run_id":"r","job_id":"j","state":"queued"}`, ""},
		{"queue entry no seq", "repos/a/b/threads/queue/0001.json", `{"run_id":"r","job_id":"j","state":"queued"}`, CheckRequired},
		{"queue entry no run", "repos/a/b/threads/queue/0001.json", `{"seq":"1","job_id":"j","state":"queued"}`, CheckRequired},
		{"queue entry no job", "repos/a/b/threads/queue/0001.json", `{"seq":"1","run_id":"r","state":"queued"}`, CheckRequired},
		{"queue entry no state", "repos/a/b/threads/queue/0001.json", `{"seq":"1","run_id":"r","job_id":"j"}`, CheckRequired},
		{"queue entry bad json", "repos/a/b/threads/queue/0001.json", `{`, CheckDecode},

		{"artifact", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","run":"r","job":"j","files":[{"path":"x","size":1,"sha256":"` + sha + `","index":0}],"total_size":1}`, ""},
		{"artifact no name", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"run":"r","job":"j"}`, CheckRequired},
		{"artifact no run", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","job":"j"}`, CheckRequired},
		{"artifact no job", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","run":"r"}`, CheckRequired},
		{"artifact bad sha", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","run":"r","job":"j","files":[{"sha256":"x"}]}`, CheckRequired},
		{"artifact negative size", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","run":"r","job":"j","files":[{"sha256":"` + sha + `","size":-1}]}`, CheckRequired},
		{"artifact negative total", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{"name":"out","run":"r","job":"j","total_size":-1}`, CheckRequired},
		{"artifact bad json", "repos/a/b/threads/runs/r/artifacts/out/manifest.json", `{`, CheckDecode},

		{"expiry", "repos/a/b/threads/artifact-expiry/1-r-out.json", `{"run":"r","name":"out","expires_at":"x"}`, ""},
		{"expiry no run", "repos/a/b/threads/artifact-expiry/1-r-out.json", `{"name":"out","expires_at":"x"}`, CheckRequired},
		{"expiry no name", "repos/a/b/threads/artifact-expiry/1-r-out.json", `{"run":"r","expires_at":"x"}`, CheckRequired},
		{"expiry no at", "repos/a/b/threads/artifact-expiry/1-r-out.json", `{"run":"r","name":"out"}`, CheckRequired},
		{"expiry bad json", "repos/a/b/threads/artifact-expiry/1-r-out.json", `{`, CheckDecode},

		{"job", "repos/a/b/threads/runs/r/jobs/j.json", `{"id":"j","run_id":"r","repo":"a/b","state":"queued","version":1}`, ""},
		{"job bad version", "repos/a/b/threads/runs/r/jobs/j.json", `{"id":"j","run_id":"r","repo":"a/b","state":"queued"}`, CheckVersion},
		{"job no id", "repos/a/b/threads/runs/r/jobs/j.json", `{"run_id":"r","repo":"a/b","state":"queued","version":1}`, CheckRequired},
		{"job no run", "repos/a/b/threads/runs/r/jobs/j.json", `{"id":"j","repo":"a/b","state":"queued","version":1}`, CheckRequired},
		{"job no repo", "repos/a/b/threads/runs/r/jobs/j.json", `{"id":"j","run_id":"r","state":"queued","version":1}`, CheckRequired},
		{"job no state", "repos/a/b/threads/runs/r/jobs/j.json", `{"id":"j","run_id":"r","repo":"a/b","version":1}`, CheckRequired},
		{"job bad json", "repos/a/b/threads/runs/r/jobs/j.json", `{`, CheckDecode},

		{"run", "repos/a/b/threads/runs/r.json", `{"id":"r","repo":"a/b","event":"push","state":"queued","version":1}`, ""},
		{"run bad version", "repos/a/b/threads/runs/r.json", `{"id":"r","repo":"a/b","event":"push","state":"queued"}`, CheckVersion},
		{"run no id", "repos/a/b/threads/runs/r.json", `{"repo":"a/b","event":"push","state":"queued","version":1}`, CheckRequired},
		{"run no repo", "repos/a/b/threads/runs/r.json", `{"id":"r","event":"push","state":"queued","version":1}`, CheckRequired},
		{"run no event", "repos/a/b/threads/runs/r.json", `{"id":"r","repo":"a/b","state":"queued","version":1}`, CheckRequired},
		{"run no state", "repos/a/b/threads/runs/r.json", `{"id":"r","repo":"a/b","event":"push","version":1}`, CheckRequired},
		{"run bad json", "repos/a/b/threads/runs/r.json", `{`, CheckDecode},

		{"secret generic", "repos/a/b/threads/secrets/token.json", `{"version":1,"name":"token"}`, ""},
		{"generic invalid", "repos/a/b/threads/secrets/token.json", `{`, CheckDecode},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeThreads(tc.key, []byte(tc.raw)), tc.want)
	}
}

func TestDecodeModeration(t *testing.T) {
	cases := []struct{ name, key, raw, want string }{
		{"global index", "moderation/reports/index.json", `{"version":1,"open":[{"id":1,"repo":"a/b"}],"resolved_recent":[]}`, ""},
		{"global index bad version", "moderation/reports/index.json", `{"open":[],"resolved_recent":[]}`, CheckVersion},
		{"global index bad open row", "moderation/reports/index.json", `{"version":1,"open":[{"repo":"a/b"}]}`, CheckRequired},
		{"global index bad resolved row", "moderation/reports/index.json", `{"version":1,"resolved_recent":[{"id":2}]}`, CheckRequired},
		{"global index bad json", "moderation/reports/index.json", `{`, CheckDecode},

		{"repo index", "repos/a/b/meta/moderation/reports/index.json", `{"version":1,"open":[{"id":1}],"resolved_recent":[]}`, ""},
		{"repo index bad version", "repos/a/b/meta/moderation/reports/index.json", `{"open":[]}`, CheckVersion},
		{"repo index bad row", "repos/a/b/meta/moderation/reports/index.json", `{"version":1,"open":[{"id":0}]}`, CheckRequired},
		{"repo index bad json", "repos/a/b/meta/moderation/reports/index.json", `{`, CheckDecode},

		{"record", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","kind":"thread","target":"t","reporter":"r","reason":"spam","created_at":"x"}`, ""},
		{"record id 0", "repos/a/b/meta/moderation/reports/000000000001.json", `{"repo":"a/b","kind":"thread","target":"t","reporter":"r","reason":"spam","created_at":"x"}`, CheckRequired},
		{"record no repo", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"kind":"thread","target":"t","reporter":"r","reason":"spam","created_at":"x"}`, CheckRequired},
		{"record no kind", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","target":"t","reporter":"r","reason":"spam","created_at":"x"}`, CheckRequired},
		{"record no target", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","kind":"thread","reporter":"r","reason":"spam","created_at":"x"}`, CheckRequired},
		{"record no reporter", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","kind":"thread","target":"t","reason":"spam","created_at":"x"}`, CheckRequired},
		{"record no reason", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","kind":"thread","target":"t","reporter":"r","created_at":"x"}`, CheckRequired},
		{"record no created_at", "repos/a/b/meta/moderation/reports/000000000001.json", `{"id":1,"repo":"a/b","kind":"thread","target":"t","reporter":"r","reason":"spam"}`, CheckRequired},
		{"record bad json", "repos/a/b/meta/moderation/reports/000000000001.json", `{`, CheckDecode},

		{"doc", "repos/a/b/meta/moderation.json", `{"version":1}`, ""},
		{"doc bad version", "repos/a/b/meta/moderation.json", `{}`, CheckVersion},
		{"doc bad json", "repos/a/b/meta/moderation.json", `{`, CheckDecode},

		{"audit", "moderation/audit/000000000001.json", `{"seq":1,"at":"x","actor":"a","action":"hide","target":"t"}`, ""},
		{"audit bad seq", "moderation/audit/000000000001.json", `{"at":"x","actor":"a","action":"hide","target":"t"}`, CheckRequired},
		{"audit no action", "moderation/audit/000000000001.json", `{"seq":1,"at":"x","actor":"a","target":"t"}`, CheckRequired},
		{"audit bad json", "moderation/audit/000000000001.json", `{`, CheckDecode},

		{"generic", "moderation/other.json", `{"version":1}`, ""},
		{"generic invalid", "moderation/other.json", `{`, CheckDecode},
	}
	for _, tc := range cases {
		expect(t, tc.name, decodeModeration(tc.key, []byte(tc.raw)), tc.want)
	}
}

func TestSmallHelpers(t *testing.T) {
	if baseName("a/b/c.json") != "c.json" || baseName("c.json") != "c.json" {
		t.Fatal("baseName")
	}
	if !isHexLen("abc123", 6) || isHexLen("abc12", 6) || isHexLen("abc12z", 6) {
		t.Fatal("isHexLen")
	}
	if repo, ok := repoFromKey("repos/a/b/manifest.pb"); !ok || repo != "a/b" {
		t.Fatalf("repoFromKey = %q %v", repo, ok)
	}
	for _, bad := range []string{"x/a/b", "repos//b/x", "repos/a//x", "repos/a"} {
		if _, ok := repoFromKey(bad); ok {
			t.Fatalf("repoFromKey(%q) accepted", bad)
		}
	}
	if !validDigest("sha256:"+strings.Repeat("a", 64)) || validDigest("sha256:xyz") || validDigest("md5:"+strings.Repeat("a", 64)) {
		t.Fatal("validDigest")
	}
	expect(t, "version ok", requireVersion(1, 1), "")
	expect(t, "version missing", requireVersion(0, 1), CheckVersion)
	expect(t, "version unknown", requireVersion(2, 1), CheckVersion)
	expect(t, "at least ok", requireAtLeast(3, 1), "")
	expect(t, "at least missing", requireAtLeast(0, 1), CheckVersion)
	expect(t, "at least low", requireAtLeast(-1, 1), CheckVersion)
	expect(t, "string ok", requireString("x", "v"), "")
	expect(t, "string blank", requireString("x", "  "), CheckRequired)
	expect(t, "generic empty", decodeGenericJSON(nil), CheckDecode)
	expect(t, "generic array", decodeGenericJSON([]byte(`[]`)), CheckDecode)
	expect(t, "json type mismatch", jsonInto([]byte(`{"a":"x"}`), &struct{ A int }{}), CheckDecode)
}
