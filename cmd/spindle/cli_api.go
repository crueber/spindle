// cli_api.go — the WP11 API client (11_config_cli.md §6.6): ONE HTTP helper
// for every network command, token + server resolution (SPINDLE_TOKEN /
// SPINDLE_SERVER, then the `spindle auth login` state file, 0600), the
// documented exit-code mapping (0 ok, 1 user/API error, 2 usage, 3 not
// found, 4 auth), and the group/subcommand table that dispatch AND shell
// completion read. No command here touches the object store: the CLI is a
// client of the shipped JSON API, never a second source of truth.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// WP11 exit-code contract (§6.6). exitNotFound shares the numeric value the
// operator surface already gives exit 3 (`config check --strict`); the two
// are distinct commands, so the overload is documented, not ambiguous.
const (
	exitAuth     = 4 // authentication or authorization failed (401/403)
	exitNotFound = 3 // the named object does not exist (404)
)

// defaultServer is the CLI's default base URL (§6.6).
const defaultServer = "http://127.0.0.1:8080"

// --- login state ----------------------------------------------------------

// loginEntry is one server's stored credential. The token is written 0600
// and never printed (auth status prints booleans only).
type loginEntry struct {
	Token     string `json:"token"`
	Principal string `json:"principal,omitempty"`
	Write     bool   `json:"write,omitempty"`
	Admin     bool   `json:"admin,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// loginState is the on-disk file: `<config-dir>/spindle/auth.json`.
type loginState struct {
	Version int                   `json:"version"`
	Current string                `json:"current,omitempty"` // server chosen by the last login
	Servers map[string]loginEntry `json:"servers"`
}

// stateDir resolves the CLI config dir: SPINDLE_CONFIG_DIR, else the OS
// user config dir + /spindle (~/.config/spindle on Linux).
func stateDir() string {
	if v := strings.TrimSpace(os.Getenv("SPINDLE_CONFIG_DIR")); v != "" {
		return v
	}
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return filepath.Join(d, "spindle")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "spindle")
}

// statePath is the login state file path.
func statePath() string { return filepath.Join(stateDir(), "auth.json") }

// loadState reads the state file; a missing file is an empty state, not an
// error (first run). A malformed file IS an error: silently dropping a
// credential would be worse than failing loudly.
func loadState() (loginState, error) {
	st := loginState{Version: 1, Servers: map[string]loginEntry{}}
	raw, err := os.ReadFile(statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s: %w", statePath(), err)
	}
	if st.Servers == nil {
		st.Servers = map[string]loginEntry{}
	}
	return st, nil
}

// saveState writes the state file atomically with mode 0600 (the file holds
// bearer tokens). An existing file's looser mode is tightened.
func saveState(st loginState) error {
	st.Version = 1
	if st.Servers == nil {
		st.Servers = map[string]loginEntry{}
	}
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, statePath()); err != nil {
		return err
	}
	return os.Chmod(statePath(), 0o600)
}

// normalizeServer validates/normalizes a base URL: scheme required (http or
// https), a bare host gains http://, trailing slashes are trimmed. A path
// prefix is preserved (a proxied install may live under /spindle).
func normalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty server URL")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", s)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid server URL %q: scheme must be http or https", s)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// resolveServer picks the base URL: --server, then SPINDLE_SERVER, then the
// server the last login selected, then the default.
func resolveServer(c *cli) (string, loginState, error) {
	st, err := loadState()
	if err != nil {
		return "", st, err
	}
	base := strings.TrimSpace(c.server)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("SPINDLE_SERVER"))
	}
	if base == "" {
		base = st.Current
	}
	if base == "" {
		base = defaultServer
	}
	base, err = normalizeServer(base)
	if err != nil {
		return "", st, err
	}
	return base, st, nil
}

// resolveToken returns the bearer token and where it came from ("env" or
// "state" or "none"). SPINDLE_TOKEN wins; WALGIT_TOKEN is the legacy alias.
func resolveToken(base string, st loginState) (token, source string) {
	for _, name := range []string{"SPINDLE_TOKEN", "WALGIT_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, "env"
		}
	}
	if e, ok := st.Servers[base]; ok && e.Token != "" {
		return e.Token, "state"
	}
	return "", "none"
}

// --- HTTP client ----------------------------------------------------------

// apiError is a non-2xx response. The body is the API's plain-text error
// (07_api.md §2), already redacted by the client.
type apiError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *apiError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 512 {
		b = b[:512] + "…"
	}
	prefix := ""
	if e.Method != "" {
		prefix = e.Method + " " + e.Path + ": "
	}
	if b == "" {
		return fmt.Sprintf("%sHTTP %d", prefix, e.Status)
	}
	return fmt.Sprintf("%sHTTP %d: %s", prefix, e.Status, b)
}

// apiClient is the one HTTP helper every network command uses.
type apiClient struct {
	base   string
	token  string
	source string
	hc     *http.Client
	out    io.Writer
	errOut io.Writer
	quiet  bool
}

// newAPIClient resolves server + token and builds the client.
func newAPIClient(c *cli, f *apiFlags) (*apiClient, error) {
	base, st, err := resolveServer(c)
	if err != nil {
		return nil, err
	}
	tok, src := resolveToken(base, st)
	a := &apiClient{
		base:   base,
		token:  tok,
		source: src,
		// No Timeout: --ctx is the deadline (a release asset upload may be
		// minutes long); main cancels it on SIGINT/SIGTERM.
		hc:     &http.Client{},
		out:    c.stdout(),
		errOut: c.stderr(),
	}
	if f != nil {
		a.quiet = f.quiet
	}
	return a, nil
}

// redact removes the bearer token from anything we print (a misbehaving
// proxy or a debug echo must never leak it).
func (a *apiClient) redact(s string) string {
	if a.token == "" {
		return s
	}
	return strings.ReplaceAll(s, a.token, "[REDACTED]")
}

// send performs one request and returns the body. path is absolute on the
// server ("/api/v1/me"); q may be nil.
func (a *apiClient) send(ctx context.Context, method, path string, q url.Values, body []byte, hdr http.Header) ([]byte, error) {
	var rdr io.Reader
	var length int64 = -1
	if body != nil {
		rdr = bytes.NewReader(body)
		length = int64(len(body))
	}
	return a.sendReader(ctx, method, path, q, rdr, length, hdr, body != nil)
}

// sendReader is send with a streaming body (release asset uploads: a 2 GiB
// asset must not be read into memory).
func (a *apiClient) sendReader(ctx context.Context, method, path string, q url.Values, body io.Reader, length int64, hdr http.Header, isJSON bool) ([]byte, error) {
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if length >= 0 {
		req.ContentLength = length
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spindle/"+version())
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range hdr {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s", method, path, a.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil {
		return nil, fmt.Errorf("%s %s: reading response: %s", method, path, a.redact(rerr.Error()))
	}
	if resp.StatusCode >= 400 {
		return raw, &apiError{Method: method, Path: path, Status: resp.StatusCode,
			Body: a.redact(string(raw))}
	}
	return raw, nil
}

// call marshals an optional JSON body and sends it.
func (a *apiClient) call(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	return a.send(ctx, method, path, q, raw, nil)
}

// callInto is call + decode into v (v may be nil to ignore the body).
func (a *apiClient) callInto(ctx context.Context, method, path string, q url.Values, body, v any) ([]byte, error) {
	raw, err := a.call(ctx, method, path, q, body)
	if err != nil {
		return raw, err
	}
	if v != nil && len(bytes.TrimSpace(raw)) > 0 {
		if derr := json.Unmarshal(raw, v); derr != nil {
			return raw, fmt.Errorf("%s %s: decoding response: %s", method, path, a.redact(derr.Error()))
		}
	}
	return raw, nil
}

// emitJSON pretty-prints an API body for --json (the body stays the API's
// own schema; the CLI invents no envelope). Non-JSON bodies pass through.
func (a *apiClient) emitJSON(raw []byte) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return exitOK
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		fmt.Fprintln(a.out, string(trimmed))
		return exitOK
	}
	buf.WriteByte('\n')
	_, _ = a.out.Write(buf.Bytes())
	return exitOK
}

// fail prints one redacted error line, adds the login hint on 401/403, and
// maps the exit code.
func (a *apiClient) fail(err error) int {
	msg := a.redact(err.Error())
	fmt.Fprintf(a.errOut, "spindle: %s\n", msg)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
		fmt.Fprintf(a.errOut, "spindle: hint: run `spindle auth login --server %s` or set SPINDLE_TOKEN\n", a.base)
	}
	return exitCodeFor(err)
}

// exitCodeFor is the one mapping (§6.6): 401/403 → 4, 404 → 3, anything
// else → 1. Transport failures are 1.
func exitCodeFor(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return exitAuth
		case http.StatusNotFound:
			return exitNotFound
		}
	}
	return exitErr
}

// notEnabled rewrites a 404 from a documented-but-not-yet-implemented
// surface (packages, team repo bindings) into a clear message (exit 1),
// instead of the misleading "not found" (exit 3). Served surfaces —
// threads, issues, pulls, releases, checks — must NOT use this: their 404
// means the named object is missing.
func (a *apiClient) notEnabled(err error, feature string) int {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		fmt.Fprintf(a.errOut, "spindle: %s: feature not enabled on this server (%s %s returned 404)\n",
			feature, ae.Method, ae.Path)
		return exitErr
	}
	return a.fail(err)
}

// --- flags & output -------------------------------------------------------

// apiFlags are the flags every network command shares.
type apiFlags struct {
	json  bool
	quiet bool
	limit int
}

func (f *apiFlags) add(fs *flag.FlagSet) {
	fs.BoolVar(&f.json, "json", false, "print the API's JSON (stable schema, re-indented)")
	fs.BoolVar(&f.quiet, "quiet", false, "suppress headers and confirmations")
	fs.IntVar(&f.limit, "limit", 0, "maximum number of rows to request")
}

// usageByCommand maps "issue list" → the command table's documented usage
// line, so a command's own `--help` prints real syntax (positionals and all)
// rather than a placeholder. Derived from the table, never maintained apart.
var usageByCommand = map[string]string{}

func init() {
	for _, g := range apiGroups {
		for i := range g.commands {
			usageByCommand[g.name+" "+g.commands[i].name] = g.commands[i].usage
		}
	}
}

// newAPIFlagSet builds a FlagSet that reports usage to the CLI's stderr.
// Usage prints THIS command's usage line and every flag it defines, so
// `spindle issue list --help` is the per-command help §6.6 promises (the
// global table stays `spindle help` / a leading `-h`).
func newAPIFlagSet(c *cli, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr())
	fs.Usage = func() {
		usage := usageByCommand[name]
		if usage == "" {
			usage = "spindle " + name + " [flags] [args]"
		}
		fmt.Fprintf(c.stderr(), "usage: %s\n\nflags:\n", usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseContinue is parseAPI's "no help, no error" marker: run the command.
const parseContinue = -1

// parseAPI parses argv with flags allowed AFTER positionals (gh-style).
// A requested `-h`/`--help` prints this command's usage + flags (the
// FlagSet's Usage) and returns exitOK; any other parse error returns
// exitArg; parseContinue means the caller should keep going.
func parseAPI(fs *flag.FlagSet, args []string) (pos []string, exit int) {
	var flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if k := strings.IndexByte(name, '='); k >= 0 {
				continue
			}
			f := fs.Lookup(name)
			if f == nil || !isBoolFlag(f.Value) {
				if i+1 < len(args) {
					flags = append(flags, args[i+1])
					i++
				}
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, exitOK
		}
		return nil, exitArg
	}
	return pos, parseContinue
}

// isBoolFlag reports whether a flag.Value is a boolean (so --flag consumes
// no value when permuting argv).
func isBoolFlag(v flag.Value) bool {
	b, ok := v.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// table is a tab-aligned human table.
type table struct {
	w   *tabwriter.Writer
	out io.Writer
}

func newTable(w io.Writer, headers ...string) *table {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	t := &table{w: tw, out: w}
	if len(headers) > 0 {
		t.row(headers...)
	}
	return t
}

func (t *table) row(cols ...string) {
	_, _ = fmt.Fprintln(t.w, strings.Join(cols, "\t"))
}

func (t *table) flush() { _ = t.w.Flush() }

// --- paths ----------------------------------------------------------------

// repoSelector is the default OWNER/REPO for commands that take one: the
// global -R (peeled in main) then SPINDLE_REPO. Empty when neither is set, in
// which case the command requires the positional.
func (c *cli) repoSelector() string {
	if c != nil {
		if v := strings.TrimSpace(c.repo); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("SPINDLE_REPO"))
}

// withRepoSelector splices the default repo into the positionals when the
// caller omitted OWNER/REPO. n is how many positionals the command wants.
func (c *cli) withRepoSelector(pos []string, n int) []string {
	sel := c.repoSelector()
	if sel == "" || len(pos) >= n {
		return pos
	}
	return append([]string{sel}, pos...)
}

// apiPath builds /owner/repo/api/<segments…> with per-segment escaping.
func apiPath(owner, repo string, segs ...string) string {
	var b strings.Builder
	b.WriteString("/")
	b.WriteString(url.PathEscape(owner))
	b.WriteString("/")
	b.WriteString(url.PathEscape(repo))
	b.WriteString("/api")
	for _, s := range segs {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// topPath builds /api/v1/<segments…> with per-segment escaping.
func topPath(segs ...string) string {
	var b strings.Builder
	b.WriteString("/api/v1")
	for _, s := range segs {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// readBodyFlag resolves --body/--body-file: an explicit file, "-" = stdin,
// otherwise the literal string.
func readBodyFlag(c *cli, literal, file string) (string, int) {
	if file != "" {
		if file == "-" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(c.stderr(), "spindle: reading stdin: %v\n", err)
				return "", exitErr
			}
			return string(raw), exitOK
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
			return "", exitErr
		}
		return string(raw), exitOK
	}
	if literal == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: reading stdin: %v\n", err)
			return "", exitErr
		}
		return string(raw), exitOK
	}
	return literal, exitOK
}

// repeatable is a flag.Value collecting repeated string flags (--label,
// --assignee, -H).
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// --- command table (dispatch + completion) --------------------------------

// apiCommand is one row of the WP11 command table.
type apiCommand struct {
	name  string   // within the group: "list", "member list", "add-repo"
	usage string   // full usage line for help
	help  string   // one-line summary
	flags []string // long flag names (completion)
	run   func(ctx context.Context, c *cli, args []string) int
}

// apiGroup is a top-level command (auth, user, org, …) with its subtables.
type apiGroup struct {
	name     string
	help     string
	commands []apiCommand
}

// groupRunner dispatches a group: two-token subcommands first ("member list"),
// then one token — the same resolution order as main's flat table. A leading
// `-h`/`--help` prints the group table (exit 0), never "unknown subcommand".
func groupRunner(g *apiGroup) func(context.Context, *cli, []string) int {
	return func(ctx context.Context, c *cli, args []string) int {
		if len(args) == 0 {
			printGroupHelp(c, g)
			return exitArg
		}
		if args[0] == "-h" || args[0] == "--help" {
			printGroupHelp(c, g)
			return exitOK
		}
		if len(args) >= 2 {
			key := args[0] + " " + args[1]
			for i := range g.commands {
				if g.commands[i].name == key {
					return g.commands[i].run(ctx, c, args[2:])
				}
			}
		}
		for i := range g.commands {
			if g.commands[i].name == args[0] {
				return g.commands[i].run(ctx, c, args[1:])
			}
		}
		fmt.Fprintf(c.stderr(), "spindle %s: unknown subcommand %q\n\n", g.name, args[0])
		printGroupHelp(c, g)
		return exitArg
	}
}

// printGroupHelp renders the group's usage table.
func printGroupHelp(c *cli, g *apiGroup) {
	w := c.stderr()
	fmt.Fprintf(w, "spindle %s — %s\n\n", g.name, g.help)
	fmt.Fprintf(w, "Usage: spindle %s <subcommand> [flags]\n\n", g.name)
	t := newTable(w)
	for i := range g.commands {
		cmd := &g.commands[i]
		t.row("  "+cmd.name, cmd.help)
	}
	t.flush()
	fmt.Fprintf(w, "\nRun \"spindle %s <subcommand> -h\" for flags.\n", g.name)
}

// apiGroups is the WP11 surface. Dispatch, help, and completion all read
// this table — there is no second list to drift.
var apiGroups = []*apiGroup{
	authGroup, userGroup, orgGroup, issueGroup, prGroup, releaseGroup,
	checksGroup, packagesGroup, threadsGroup, fileGroup,
}

// --- completion -----------------------------------------------------------

// completionWords builds the prefix → candidate-words map from the SAME
// tables dispatch uses: top-level commands, two-token operator commands,
// group subcommands, and each command's flags.
func completionWords() map[string][]string {
	words := map[string][]string{}
	add := func(key string, vals ...string) {
		seen := map[string]bool{}
		for _, v := range words[key] {
			seen[v] = true
		}
		for _, v := range vals {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			words[key] = append(words[key], v)
		}
	}
	for _, cmd := range subcommands {
		parts := strings.Fields(cmd.name)
		if len(parts) == 1 {
			add("", parts[0])
			continue
		}
		add("", parts[0])
		add(parts[0], parts[1])
	}
	for _, g := range apiGroups {
		add("", g.name)
		for i := range g.commands {
			parts := strings.Fields(g.commands[i].name)
			parent := g.name
			for _, p := range parts {
				add(parent, p)
				parent += " " + p
			}
			for _, fl := range g.commands[i].flags {
				add(parent, "--"+fl)
			}
		}
	}
	add("", "help")
	// Global flags are valid everywhere.
	for key := range words {
		add(key, "--config", "--data-dir", "--server", "--help", "-h")
	}
	return words
}

// completionScript generates bash/zsh/fish from completionWords().
func completionScript(shell string) (string, error) {
	switch shell {
	case "bash":
		return bashCompletion(), nil
	case "zsh":
		return zshCompletion(), nil
	case "fish":
		return fishCompletion(), nil
	}
	return "", fmt.Errorf("unknown shell %q: use bash, zsh, or fish", shell)
}

func sortedWordKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func bashCompletion() string {
	words := completionWords()
	var b strings.Builder
	b.WriteString("# bash completion for spindle (generated from the command table)\n")
	b.WriteString("_spindle() {\n")
	b.WriteString("    local cur key i w words_str\n")
	b.WriteString("    COMPREPLY=()\n")
	b.WriteString("    cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("    key=\"\"\n")
	b.WriteString("    for ((i=1; i<COMP_CWORD; i++)); do\n")
	b.WriteString("        w=\"${COMP_WORDS[i]}\"\n")
	b.WriteString("        [[ \"$w\" == -* ]] && continue\n")
	b.WriteString("        key=\"${key:+$key }$w\"\n")
	b.WriteString("    done\n")
	b.WriteString("    words_str=\"\"\n")
	b.WriteString("    case \"$key\" in\n")
	for _, key := range sortedWordKeys(words) {
		fmt.Fprintf(&b, "    %q) words_str=%q ;;\n", key, strings.Join(words[key], " "))
	}
	b.WriteString("    esac\n")
	b.WriteString("    COMPREPLY=( $(compgen -W \"$words_str\" -- \"$cur\") )\n")
	b.WriteString("}\n")
	b.WriteString("complete -o default -F _spindle spindle\n")
	return b.String()
}

func zshCompletion() string {
	words := completionWords()
	var b strings.Builder
	b.WriteString("#compdef spindle\n")
	b.WriteString("# zsh completion for spindle (generated from the command table)\n")
	b.WriteString("_spindle() {\n")
	b.WriteString("    local key=\"\" i w\n")
	b.WriteString("    local -a candidates\n")
	b.WriteString("    for ((i=2; i<CURRENT; i++)); do\n")
	b.WriteString("        w=\"${words[i]}\"\n")
	b.WriteString("        [[ \"$w\" == -* ]] && continue\n")
	b.WriteString("        key=\"${key:+$key }$w\"\n")
	b.WriteString("    done\n")
	b.WriteString("    case \"$key\" in\n")
	for _, key := range sortedWordKeys(words) {
		fmt.Fprintf(&b, "    %q) candidates=(%s) ;;\n", key, strings.Join(words[key], " "))
	}
	b.WriteString("    esac\n")
	b.WriteString("    if (( ${#candidates} )); then\n")
	b.WriteString("        _describe 'spindle' candidates\n")
	b.WriteString("    else\n")
	b.WriteString("        _files\n")
	b.WriteString("    fi\n")
	b.WriteString("}\n")
	b.WriteString("_spindle \"$@\"\n")
	return b.String()
}

func fishCompletion() string {
	words := completionWords()
	var b strings.Builder
	b.WriteString("# fish completion for spindle (generated from the command table)\n")
	b.WriteString("complete -c spindle -f\n")
	for _, key := range sortedWordKeys(words) {
		var flags, vals []string
		for _, v := range words[key] {
			if strings.HasPrefix(v, "-") {
				flags = append(flags, v)
			} else {
				vals = append(vals, v)
			}
		}
		var cond string
		switch key {
		case "":
			cond = "__fish_use_subcommand"
		default:
			parts := strings.Split(key, " ")
			var cs []string
			for _, p := range parts {
				cs = append(cs, "__fish_seen_subcommand_from "+p)
			}
			cond = strings.Join(cs, "; and ")
		}
		if len(vals) > 0 {
			fmt.Fprintf(&b, "complete -c spindle -n %q -a %q\n", cond, strings.Join(vals, " "))
		}
		for _, f := range flags {
			name := strings.TrimPrefix(f, "--")
			if name == "h" {
				fmt.Fprintf(&b, "complete -c spindle -n %q -s h\n", cond)
				continue
			}
			fmt.Fprintf(&b, "complete -c spindle -n %q -l %s\n", cond, name)
		}
	}
	return b.String()
}

// --- misc shared ----------------------------------------------------------

// nowStamp is RFC 3339 UTC for state files.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// trimNewline strips one trailing newline (tokens read from a file/pipe).
func trimNewline(s string) string { return strings.TrimRight(s, "\r\n") }
