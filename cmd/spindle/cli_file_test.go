// cli_file_test.go — the Feature F CLI verbs against the HTTP fake: method,
// path, query, base64 content, precondition fields, and exit codes. No live
// server.
package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestFilePutHTTPShape(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("PUT", "/demo/paint/api/contents", 201,
		`{"commit":{"sha":"1111111111111111111111111111111111111111"},"branch":"main"}`)
	code, out, errOut := runCmd(t, srv, "tok", runFilePut,
		"demo/paint", "docs/a.md", "--branch", "main", "--message", "Add a",
		"--content", "hello\n", "--expect-blob", "2222", "--expect-branch", "3333")
	if code != exitOK {
		t.Fatalf("put = %d out=%q err=%q", code, out, errOut)
	}
	rec := f.last()
	if rec.Method != "PUT" || rec.Path != "/demo/paint/api/contents" {
		t.Fatalf("request = %s %s", rec.Method, rec.Path)
	}
	if rec.Query.Get("ref") != "main" || rec.Query.Get("path") != "docs/a.md" {
		t.Fatalf("query = %v", rec.Query)
	}
	var body map[string]any
	if err := jsonUnmarshalTest(rec.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got, _ := body["content"].(string); got != base64.StdEncoding.EncodeToString([]byte("hello\n")) {
		t.Fatalf("content = %q", got)
	}
	if body["message"] != "Add a" || body["expected_blob_sha"] != "2222" || body["expected_branch_sha"] != "3333" {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(out, "docs/a.md: 111111111111 on main") {
		t.Fatalf("out = %q", out)
	}
}

func TestFileCreateAndDelete(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/demo/paint/api/contents", 201,
		`{"commit":{"sha":"4444444444444444444444444444444444444444"},"branch":"fix/x","pr":{"num":7}}`)
	code, out, _ := runCmd(t, srv, "tok", runFileCreate,
		"demo/paint", "new.md", "--branch", "main", "--message", "New",
		"--content-file", "-", "--new-branch", "fix/x", "--open-pr")
	_ = out
	if code != exitOK {
		t.Fatalf("create = %d", code)
	}
	rec := f.last()
	if rec.Method != "POST" {
		t.Fatalf("method = %s", rec.Method)
	}
	var body map[string]any
	_ = jsonUnmarshalTest(rec.Body, &body)
	if _, has := body["expected_blob_sha"]; has {
		t.Fatalf("create sent expected_blob_sha: %v", body)
	}
	if body["new_branch"] != "fix/x" || body["open_pr"] != true {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(out, "pull request #7") {
		t.Fatalf("out = %q", out)
	}

	f.on("DELETE", "/demo/paint/api/contents", 200,
		`{"commit":{"sha":"5555555555555555555555555555555555555555"},"branch":"main"}`)
	code, out, _ = runCmd(t, srv, "tok", runFileDelete,
		"demo/paint", "old.md", "--branch", "main", "--message", "Remove", "--expect-blob", "6666")
	if code != exitOK {
		t.Fatalf("delete = %d", code)
	}
	rec = f.last()
	if rec.Method != "DELETE" || rec.Query.Get("path") != "old.md" {
		t.Fatalf("delete request = %s %v", rec.Method, rec.Query)
	}
	if strings.Contains(out, "old.md") == false {
		t.Fatalf("out = %q", out)
	}
}

func TestFileFlagAndErrorPaths(t *testing.T) {
	_, srv := newFakeAPI(t)
	// Missing required flags are usage errors.
	if code, _, _ := runCmd(t, srv, "tok", runFilePut, "demo/paint", "a.md"); code != exitArg {
		t.Fatalf("missing flags = %d", code)
	}
	if code, _, _ := runCmd(t, srv, "tok", runFileDelete, "demo/paint", "a.md", "--branch", "main", "--message", "m"); code != exitArg {
		t.Fatalf("delete without expect-blob = %d", code)
	}
	if code, _, _ := runCmd(t, srv, "tok", runFilePut, "not-a-repo", "a.md"); code != exitArg {
		t.Fatalf("bad repo = %d", code)
	}
	// A 409 is a command error (exit 1) carrying the server's message.
	f, srv2 := newFakeAPI(t)
	f.on("PUT", "/demo/paint/api/contents", 409, "branch moved under you (page: a, disk: b)")
	code, _, errOut := runCmd(t, srv2, "tok", runFilePut,
		"demo/paint", "a.md", "--branch", "main", "--message", "m", "--content", "x")
	if code != exitErr || !strings.Contains(errOut, "branch moved") {
		t.Fatalf("409 = %d %q", code, errOut)
	}
	// --json echoes the API body.
	f.on("PUT", "/demo/paint/api/contents", 201,
		`{"commit":{"sha":"7777777777777777777777777777777777777777"},"branch":"main"}`)
	code, out, _ := runCmd(t, srv2, "tok", runFilePut,
		"demo/paint", "a.md", "--branch", "main", "--message", "m", "--content", "x", "--json")
	if code != exitOK || !strings.Contains(out, `"sha": "7777777777777777777777777777777777777777"`) {
		t.Fatalf("json = %d %q", code, out)
	}
}

// jsonUnmarshalTest is a tiny local decoder (the CLI file test avoids the
// commander's helpers so a failure names the body).
func jsonUnmarshalTest(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
