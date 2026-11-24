package webedit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Gate is the repo-scoped push rule the editor shares with git receive-pack
// (F.5 step 1, 01 §5.3): CheckPush strips host flags and applies the
// self-namespace rule. CheckRead is the visibility gate. *identity.Service
// implements both; tests substitute a fake.
type Gate interface {
	CheckPush(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// RepoDirs resolves a repo to its synced (serve-level) bare git dir.
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// RefPublisher lands the server-made commit through the WAL funnel: the pack
// uploads and the ref txn commits in ONE entry (old=zero ⇒ create).
type RefPublisher interface {
	UpdateRefWithPack(ctx context.Context, repo, ref, old, newSHA string, pack []byte, meta map[string]string) error
}

// OpenInput is the PR the editor asks the shipped pulls service to open.
type OpenInput struct {
	Title   string
	Body    string
	BaseRef string // short branch the edit was based on
	HeadRef string // short new branch
}

// PROpener is the consumer-owned seam F.6 names (the ChecksGate house style):
// composition wires *pulls.Service.OpenPR to it; this package imports no
// feature internals.
type PROpener interface {
	OpenPR(ctx context.Context, owner, repo string, in OpenInput, actor auth.Principal) (num int, err error)
}

// Service is the file-edit client. Store is read only for the repo's existing
// policy.json. MaxBytes is webedit.max_bytes (0 ⇒ DefaultMaxBytes).
type Service struct {
	Store    store.ObjectStore
	Gate     Gate
	Git      GitRunner
	Dirs     RepoDirs
	Refs     RefPublisher
	PR       PROpener
	Now      func() time.Time
	MaxBytes int64
}

// New builds a Service over st.
func New(st store.ObjectStore, gate Gate) *Service {
	return &Service{Store: st, Gate: gate}
}

// maxBytes resolves the effective cap.
func (s *Service) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

// PolicyKey is the existing per-repo policy document (no new keys).
func PolicyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/policy.json"
}

// Save applies one create/edit/delete and publishes it.
func (s *Service) Save(ctx context.Context, owner, repo string, p auth.Principal, in Input) (*Result, error) {
	pr, err := s.prepare(ctx, owner, repo, p, in)
	if err != nil {
		return nil, err
	}
	pack, perr := s.pack(ctx, pr)
	if perr != nil {
		return nil, perr
	}
	old := pr.Base
	if pr.NewBranch {
		old = strings.Repeat("0", len(pr.Base))
	}
	meta := map[string]string{"principal": p.Name, "agent": "webedit"}
	if prerr := s.Refs.UpdateRefWithPack(ctx, repoName(owner, repo), pr.TargetRef, old, pr.Commit, pack, meta); prerr != nil {
		if errors.Is(prerr, ErrConflict) {
			// The CAS lost: re-read the branch (one serve-level refresh) so the
			// 409 names both shas, never a bare "conflict".
			actual := ""
			if dir, derr := s.dir(ctx, owner, repo); derr == nil {
				if sha, rerr := s.Git.ResolveBranch(ctx, dir, pr.Branch); rerr == nil {
					actual = sha
				}
			}
			return nil, &ConflictError{What: "branch", Expected: pr.Base, Actual: actual, Why: prerr.Error()}
		}
		return nil, prerr
	}
	res := &Result{Commit: pr.Commit, Branch: pr.Branch}
	if pr.NewBranch && in.OpenPR {
		if s.PR == nil {
			return nil, fmt.Errorf("%w: pull-request opening is not wired", ErrUnavailable)
		}
		title := strings.TrimSpace(in.PRTitle)
		if title == "" {
			title = "Edit " + in.Path
		}
		body := in.PRBody
		if body == "" {
			body = in.Description
		}
		num, oerr := s.PR.OpenPR(ctx, owner, repo, OpenInput{
			Title: title, Body: body, BaseRef: pr.BranchBase, HeadRef: pr.Branch,
		}, p)
		if oerr != nil {
			// The branch is already published. Report the truth rather than
			// pretending the whole edit failed: a retry would 409 on the new
			// branch. The caller may open the PR by hand.
			return nil, fmt.Errorf("%w: branch %q created, but opening the pull request failed: %v", ErrUnavailable, pr.Branch, oerr)
		}
		res.PRNum = num
	}
	return res, nil
}

// Preview builds the pending change and returns its unified patch without
// publishing anything (the editor's diff panel).
func (s *Service) Preview(ctx context.Context, owner, repo string, p auth.Principal, in Input) (string, error) {
	pr, err := s.prepare(ctx, owner, repo, p, in)
	if err != nil {
		if errors.Is(err, ErrNoChange) {
			return "", nil
		}
		return "", err
	}
	return s.Git.DiffTree(ctx, pr.Dir, pr.Base, pr.Tree)
}

// Load reads one file for the editor page (write gate applied: the editor is
// a write surface, so a reader who cannot push gets the same 403 the save
// would).
func (s *Service) Load(ctx context.Context, owner, repo string, p auth.Principal, ref, path string) (*File, error) {
	if err := s.gatePush(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	_, name, err := normalizeBranchRef(ref)
	if err != nil {
		return nil, err
	}
	dir, err := s.dir(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	sha, err := s.Git.ResolveBranch(ctx, dir, name)
	if err != nil {
		return nil, err
	}
	return s.Git.LookupFile(ctx, dir, sha, path)
}

// prepared is one request's resolved, validated change.
type prepared struct {
	Dir        string
	Base       string
	TargetRef  string
	Branch     string // the branch the commit landed on
	BranchBase string // short base branch
	NewBranch  bool
	Tree       string
	Commit     string
}

func repoName(owner, repo string) string { return owner + "/" + repo }

// prepare runs F.5 steps 1–3: gate, resolve, preconditions, guards, policy,
// and the plumbing commit. Nothing is published here.
func (s *Service) prepare(ctx context.Context, owner, repo string, p auth.Principal, in Input) (*prepared, error) {
	if err := s.gatePush(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return nil, fmt.Errorf("%w: file editing is not wired on this build", ErrUnavailable)
	}
	path := strings.TrimSpace(in.Path)
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	msg, desc, err := ValidateMessage(in.Message, in.Description)
	if err != nil {
		return nil, err
	}
	_, branch, err := normalizeBranchRef(in.Ref)
	if err != nil {
		return nil, err
	}
	var newBranchName string
	newBranch := strings.TrimSpace(in.NewBranch) != ""
	if newBranch {
		if newBranchName, err = ValidateBranch(in.NewBranch); err != nil {
			return nil, err
		}
		if newBranchName == branch {
			return nil, fmt.Errorf("%w: new branch must differ from the base branch", ErrInvalid)
		}
	}
	if in.OpenPR && !newBranch {
		return nil, fmt.Errorf("%w: open_pr requires new_branch", ErrInvalid)
	}
	switch in.Mode {
	case ModeCreate, ModeEdit, ModeDelete:
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrInvalid, in.Mode)
	}
	if in.Mode == ModeDelete && newBranch {
		return nil, fmt.Errorf("%w: deleting through a new branch is not supported", ErrInvalid)
	}

	dir, err := s.dir(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	base, err := s.Git.ResolveBranch(ctx, dir, branch)
	if err != nil {
		return nil, err
	}
	if in.ExpectedBranchSHA != "" && in.ExpectedBranchSHA != base {
		return nil, &ConflictError{What: "branch", Expected: in.ExpectedBranchSHA, Actual: base}
	}

	var existing *File
	switch in.Mode {
	case ModeCreate:
		if in.ExpectedBlobSHA != "" {
			return nil, fmt.Errorf("%w: expected_blob_sha is forbidden when creating", ErrInvalid)
		}
		existing, err = s.Git.LookupFile(ctx, dir, base, path)
		if err == nil && existing != nil {
			return nil, &ConflictError{
				What: "blob", Actual: existing.SHA,
				Why: fmt.Sprintf("file %q already exists", path),
			}
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		existing = nil
	case ModeEdit, ModeDelete:
		existing, err = s.Git.LookupFile(ctx, dir, base, path)
		if err != nil {
			return nil, err
		}
		if in.ExpectedBlobSHA == "" {
			return nil, fmt.Errorf("%w: expected_blob_sha is required when the file exists", ErrInvalid)
		}
		if in.ExpectedBlobSHA != existing.SHA {
			return nil, &ConflictError{What: "blob", Expected: in.ExpectedBlobSHA, Actual: existing.SHA}
		}
		if existing.Binary {
			return nil, fmt.Errorf("%w: %q is binary; the web editor is for text files", ErrUnsupported, path)
		}
		if existing.LFS {
			return nil, fmt.Errorf("%w: %q is a Git LFS pointer; use git + LFS from a clone", ErrUnsupported, path)
		}
	}

	// Policy (all push effects: protect + the required-reviews push half,
	// exactly what receive-pack evaluates). The target is the ref this
	// commit moves: the base branch for a direct edit, the new branch for a
	// propose-branch edit.
	targetRef := "refs/heads/" + branch
	op := "update" // a direct edit moves an existing branch
	if newBranch {
		op = "create"
	}
	if newBranch {
		targetRef = "refs/heads/" + newBranchName
	}
	if err := s.checkPolicy(ctx, owner, repo, p, targetRef, op); err != nil {
		return nil, err
	}
	// A direct delete of a protected ref must also pass the delete op
	// (protect rules are op-scoped).
	if in.Mode == ModeDelete {
		if err := s.checkPolicy(ctx, owner, repo, p, targetRef, "delete"); err != nil {
			return nil, err
		}
	}

	mode := "100644"
	if existing != nil {
		mode = existing.Mode
	}
	build := CommitBuild{
		Base:       base,
		Path:       path,
		Mode:       mode,
		Message:    commitMessage(msg, desc),
		AuthorName: p.Name,
		AuthorMail: authorEmail(p),
		When:       s.now(),
	}
	if in.Mode == ModeDelete {
		build.Delete = true
	} else {
		content := in.Content
		if int64(len(content)) > s.maxBytes() {
			return nil, fmt.Errorf("%w: file exceeds webedit.max_bytes (%d bytes)", ErrTooLarge, s.maxBytes())
		}
		if isBinary(content) {
			return nil, fmt.Errorf("%w: content is binary (NUL byte); the web editor is for text files", ErrUnsupported)
		}
		if isLFSPointer(content) {
			return nil, fmt.Errorf("%w: content is a Git LFS pointer; use git + LFS from a clone", ErrUnsupported)
		}
		if lfs, lerr := s.pathIsLFS(ctx, dir, base, path); lerr != nil {
			return nil, lerr
		} else if lfs {
			return nil, fmt.Errorf("%w: %q is marked filter=lfs in .gitattributes; use git + LFS from a clone", ErrUnsupported, path)
		}
		blob, herr := s.Git.HashBlob(ctx, dir, content)
		if herr != nil {
			return nil, herr
		}
		build.BlobSHA = blob
	}
	commit, tree, berr := s.Git.BuildCommit(ctx, dir, build)
	if berr != nil {
		return nil, berr
	}
	pr := &prepared{
		Dir: dir, Base: base, Branch: branch, BranchBase: branch,
		NewBranch: newBranch, Tree: tree, Commit: commit,
	}
	if newBranch {
		pr.TargetRef = "refs/heads/" + newBranchName
		pr.Branch = newBranchName
	} else {
		pr.TargetRef = "refs/heads/" + branch
	}
	return pr, nil
}

// pack packs the new tip's objects. A fully-contained tip (a duplicate
// commit) publishes ref-only.
func (s *Service) pack(ctx context.Context, pr *prepared) ([]byte, error) {
	pack, err := s.Git.PackTip(ctx, pr.Dir, pr.Commit)
	if err != nil {
		return nil, err
	}
	return pack, nil
}

// pathIsLFS consults the base commit's root .gitattributes (the documented
// subset in gitattributes.go). A missing file is not an error.
func (s *Service) pathIsLFS(ctx context.Context, dir, base, path string) (bool, error) {
	body, ok, err := s.Git.RootFile(ctx, dir, base, ".gitattributes")
	if err != nil || !ok {
		return false, err
	}
	return matchGitattributes(body, path), nil
}

// gatePush is the F.5 gate: read visibility then the push rule.
func (s *Service) gatePush(ctx context.Context, owner, repo string, p auth.Principal) error {
	// Fail closed on anonymous before any store or git work, whatever the
	// gate is wired to (the tags precedent; the identity gate would 401 too,
	// but a nil/fake gate must never become an authorization bypass).
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	if s.Gate == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		if !p.Write && !p.Admin {
			return fmt.Errorf("%w: write access required", ErrForbidden)
		}
		return nil
	}
	if aerr := s.Gate.CheckRead(ctx, owner, repo, p); aerr != nil {
		return mapAuthErr(aerr)
	}
	// The push floor, not CheckRole(write): a host-wide write flag alone
	// must not authorize an edit on someone else's repo (the #347 lesson).
	if aerr := s.Gate.CheckPush(ctx, owner, repo, p); aerr != nil {
		return mapAuthErr(aerr)
	}
	return nil
}

// mapAuthErr maps the identity surface's AuthError onto this sentinel family.
func mapAuthErr(aerr *auth.AuthError) error {
	if aerr == nil {
		return nil
	}
	switch aerr.Kind {
	case auth.ErrUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
	case auth.ErrForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
	case auth.ErrInvalid:
		return fmt.Errorf("%w: %s", ErrInvalid, aerr.Why)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, aerr.Why)
	}
}

// checkPolicy evaluates the repo's existing policy.json for (principal, ref,
// op) with every push effect, exactly as receive-pack does. Missing file is
// allow-all; an unparseable file fails closed.
func (s *Service) checkPolicy(ctx context.Context, owner, repo string, p auth.Principal, ref, op string) error {
	if s.Store == nil {
		return nil
	}
	raw, _, err := store.GetBytes(ctx, s.Store, PolicyKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%w: policy read: %v", ErrUnavailable, err)
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrForbidden, perr)
	}
	v := policy.Evaluate(ctx, doc, policy.Request{Principal: p.Name, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("%w: rejected by rule '%s'", ErrForbidden, v.Rule)
	}
	return nil
}

// dir resolves the serve-level bare repo path.
func (s *Service) dir(ctx context.Context, owner, repo string) (string, error) {
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return dir, nil
}

// authorEmail renders the principal's author email: the verified email when
// present, else the principal name itself when it is already an email (static
// tokens name principals by email), else the server-minted
// `<name>@spindle.local` (the tag precedent).
func authorEmail(p auth.Principal) string {
	if e := sanitizeIdent(p.Email); e != "" && strings.Contains(e, "@") {
		return e
	}
	name := sanitizeIdent(p.Name)
	if strings.Contains(name, "@") {
		return name
	}
	return name + "@spindle.local"
}
