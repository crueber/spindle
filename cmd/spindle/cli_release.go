// cli_release.go — `spindle release` and `spindle checks`
// (11_config_cli.md §6.6): the shipped releases + asset routes
// (docs/features/07_releases_stars.md §6) and the checks surface
// (05_checks_statuses.md §4). Assets stream from disk with the documented
// Content-Length + X-Walgit-Asset-Sha256 headers.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var releaseGroup = &apiGroup{
	name: "release",
	help: "releases and downloadable assets",
	commands: []apiCommand{
		{name: "list", usage: "spindle release list OWNER/REPO [--limit N]",
			help:  "list releases",
			flags: []string{"json", "limit", "after"}, run: runReleaseList},
		{name: "show", usage: "spindle release show OWNER/REPO TAG",
			help:  "one release",
			flags: []string{"json"}, run: runReleaseShow},
		{name: "create", usage: "spindle release create OWNER/REPO TAG [--name N] [--notes S|--notes-file F] [--draft] [--prerelease]",
			help:  "create or update a release for a tag",
			flags: []string{"json", "quiet", "name", "notes", "notes-file", "draft", "prerelease"}, run: runReleaseCreate},
		{name: "delete", usage: "spindle release delete OWNER/REPO TAG",
			help:  "delete a release and its assets",
			flags: []string{"json", "quiet"}, run: runReleaseDelete},
		{name: "upload", usage: "spindle release upload OWNER/REPO TAG FILE… [--name ASSET]",
			help:  "upload an asset (one file, or --name for one)",
			flags: []string{"json", "quiet", "name"}, run: runReleaseUpload},
	},
}

var checksGroup = &apiGroup{
	name: "checks",
	help: "commit checks and statuses",
	commands: []apiCommand{
		{name: "list", usage: "spindle checks list OWNER/REPO [--limit N] [--after SHA]",
			help:  "list commits that have checks",
			flags: []string{"json", "limit", "after"}, run: runChecksList},
		{name: "status", usage: "spindle checks status OWNER/REPO [REF]",
			help:  "combined check state for a ref or commit (default HEAD)",
			flags: []string{"json"}, run: runChecksStatus},
	},
}

// releaseDoc mirrors the release wire shape (assets [] never null).
type releaseDoc struct {
	Tag         string              `json:"tag"`
	TagSHA      string              `json:"tag_sha"`
	Name        string              `json:"name"`
	Body        string              `json:"body"`
	Draft       bool                `json:"draft"`
	Prerelease  bool                `json:"prerelease"`
	Author      string              `json:"author"`
	CreatedAt   string              `json:"created_at"`
	PublishedAt *string             `json:"published_at"`
	UpdatedAt   string              `json:"updated_at"`
	Assets      []releaseAssetEntry `json:"assets"`
}

type releaseAssetEntry struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	SHA256             string `json:"sha256"`
	ContentType        string `json:"content_type"`
	UploadedAt         string `json:"uploaded_at"`
	Uploader           string `json:"uploader"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseListResponse struct {
	Releases []releaseDoc `json:"releases"`
	More     bool         `json:"more"`
}

func runReleaseList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "release list")
	f.add(fs)
	var after string
	fs.StringVar(&after, "after", "", "created_at cursor")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "release list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if f.limit > 0 {
		q.Set("n", strconv.Itoa(f.limit))
	}
	var res releaseListResponse
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "releases"), q, nil, &res)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "TAG", "NAME", "DEV", "AUTHOR", "CREATED", "ASSETS")
	for _, r := range res.Releases {
		dev := ""
		if r.Draft {
			dev = "draft"
		} else if r.Prerelease {
			dev = "pre"
		}
		t.row(r.Tag, r.Name, dev, r.Author, r.CreatedAt, strconv.Itoa(len(r.Assets)))
	}
	t.flush()
	if res.More {
		fmt.Fprintln(c.stderr(), "more results available (use --limit/--after)")
	}
	return exitOK
}

func runReleaseShow(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "release show")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, tag, ok := repoAndWord(c, pos, "release show OWNER/REPO TAG")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	var r releaseDoc
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "releases", tag), nil, nil, &r)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	printRelease(c, r)
	return exitOK
}

func runReleaseCreate(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "release create")
	f.add(fs)
	var name, notes, notesFile string
	var draft, prerelease bool
	fs.StringVar(&name, "name", "", "release display name")
	fs.StringVar(&notes, "notes", "", "release notes (markdown)")
	fs.StringVar(&notesFile, "notes-file", "", "read the notes from FILE (- = stdin)")
	fs.BoolVar(&draft, "draft", false, "save as a draft")
	fs.BoolVar(&prerelease, "prerelease", false, "mark as a prerelease")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, tag, ok := repoAndWord(c, pos, "release create OWNER/REPO TAG")
	if !ok {
		return exitArg
	}
	body, exit := readBodyFlag(c, notes, notesFile)
	if exit != exitOK {
		return exit
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	req := map[string]any{"name": name, "body": body, "draft": draft, "prerelease": prerelease}
	var r releaseDoc
	raw, err := a.callInto(ctx, "PUT", apiPath(owner, repo, "releases", tag), nil, req, &r)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Saved release %s\n%s\n", r.Tag, a.base+"/"+owner+"/"+repo+"/releases/"+r.Tag)
	}
	return exitOK
}

func runReleaseDelete(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "release delete")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, tag, ok := repoAndWord(c, pos, "release delete OWNER/REPO TAG")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "DELETE", apiPath(owner, repo, "releases", tag), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		fmt.Fprintf(c.stdout(), "Deleted release %s\n", tag)
	}
	return exitOK
}

// runReleaseUpload streams one file (the documented shape is one asset per
// request: raw bytes + Content-Length + X-Walgit-Asset-Sha256).
func runReleaseUpload(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "release upload")
	f.add(fs)
	var name string
	fs.StringVar(&name, "name", "", "asset name (default: the file's base name)")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) < 2 || len(pos) > 3 {
		fmt.Fprintln(c.stderr(), "usage: spindle release upload OWNER/REPO TAG FILE [--name ASSET]")
		return exitArg
	}
	owner, repo, ok := splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return exitArg
	}
	tag, file := pos[1], pos[2]
	if name == "" {
		name = filepath.Base(file)
	}
	fh, err := os.Open(file)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, fh); err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	ctype := mime.TypeByExtension(filepath.Ext(file))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", ctype)
	hdr.Set("X-Walgit-Asset-Sha256", hex.EncodeToString(sum.Sum(nil)))
	raw, err := a.sendReader(ctx, "POST", apiPath(owner, repo, "releases", tag, "assets", name),
		nil, fh, info.Size(), hdr, false)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	if !f.quiet {
		var entry releaseAssetEntry
		if err := json.Unmarshal(raw, &entry); err == nil {
			fmt.Fprintf(c.stdout(), "Uploaded %s (%d bytes, sha256 %s)\n", entry.Name, entry.Size, short12(entry.SHA256))
		} else {
			fmt.Fprintf(c.stdout(), "Uploaded %s\n", name)
		}
	}
	return exitOK
}

func printRelease(c *cli, r releaseDoc) {
	flags := ""
	if r.Draft {
		flags = " [draft]"
	}
	if r.Prerelease {
		flags += " [prerelease]"
	}
	fmt.Fprintf(c.stdout(), "%s%s\n", r.Tag, flags)
	if r.Name != "" {
		fmt.Fprintf(c.stdout(), "  name:    %s\n", r.Name)
	}
	fmt.Fprintf(c.stdout(), "  author:  %s\n  created: %s\n", r.Author, r.CreatedAt)
	if r.TagSHA != "" {
		fmt.Fprintf(c.stdout(), "  tag sha: %s\n", r.TagSHA)
	}
	if strings.TrimSpace(r.Body) != "" {
		fmt.Fprintln(c.stdout())
		fmt.Fprintln(c.stdout(), strings.TrimSpace(r.Body))
	}
	if len(r.Assets) > 0 {
		fmt.Fprintln(c.stdout())
		t := newTable(c.stdout(), "ASSET", "SIZE", "SHA256", "UPLOADED")
		for _, as := range r.Assets {
			t.row(as.Name, strconv.FormatInt(as.Size, 10), short12(as.SHA256), as.UploadedAt)
		}
		t.flush()
	}
}

// --- checks ---------------------------------------------------------------

type checksIndexRow struct {
	SHA       string `json:"sha"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

type checksPage struct {
	Checks []checksIndexRow `json:"checks"`
	More   bool             `json:"more"`
}

type resolveDoc struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func runChecksList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "checks list")
	f.add(fs)
	var after string
	fs.StringVar(&after, "after", "", "sha cursor")
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	owner, repo, ok := oneRepo(c, pos, "checks list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if f.limit > 0 {
		q.Set("n", strconv.Itoa(f.limit))
	}
	var page checksPage
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "checks"), q, nil, &page)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "SHA", "STATE", "UPDATED")
	for _, row := range page.Checks {
		t.row(short12(row.SHA), row.State, row.UpdatedAt)
	}
	t.flush()
	if page.More {
		fmt.Fprintln(c.stderr(), "more results available (use --limit/--after)")
	}
	return exitOK
}

func runChecksStatus(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "checks status")
	f.add(fs)
	pos, ok := parseAPI(fs, args)
	if !ok {
		return exitArg
	}
	if len(pos) < 1 || len(pos) > 2 {
		fmt.Fprintln(c.stderr(), "usage: spindle checks status OWNER/REPO [REF]")
		return exitArg
	}
	owner, repo, ok := splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return exitArg
	}
	ref := "HEAD"
	if len(pos) == 2 {
		ref = pos[1]
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	// Resolve the ref to a full sha first (07_api.md §3 resolve is the
	// documented ref→sha call; a hex sha resolves to itself).
	var res resolveDoc
	if _, err := a.callInto(ctx, "GET", apiPath(owner, repo, "resolve", ref), nil, nil, &res); err != nil {
		return a.fail(err)
	}
	if res.SHA == "" {
		fmt.Fprintf(c.stderr(), "spindle: %q did not resolve to a commit\n", ref)
		return exitNotFound
	}
	var combined checkCombined
	raw, err := a.callInto(ctx, "GET", apiPath(owner, repo, "checks", res.SHA), nil, nil, &combined)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	printCombinedChecks(c, combined)
	return exitOK
}

// repoAndWord parses OWNER/REPO plus one free-form token (tag, slug…).
func repoAndWord(c *cli, pos []string, usage string) (owner, repo, word string, ok bool) {
	if len(pos) != 2 {
		fmt.Fprintf(c.stderr(), "usage: spindle %s\n", usage)
		return "", "", "", false
	}
	owner, repo, ok = splitRepo(pos[0])
	if !ok {
		fmt.Fprintf(c.stderr(), "spindle: %q is not OWNER/REPO\n", pos[0])
		return "", "", "", false
	}
	return owner, repo, pos[1], true
}
