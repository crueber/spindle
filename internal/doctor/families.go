// families.go — the bucket families the doctor samples and their codecs.
//
// A JSON family is decoded strictly enough to catch corruption but tolerates
// unknown fields (14_extensibility.md §14.12: additive fields are the compat
// rule, so rejecting them would make the doctor fail on every newer writer).
// Protobuf families use the same hand-rolled codec the server uses
// (internal/store/proto), not a second parser.
package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/store/proto"
)

// families returns the sampled families in report order.
func families() []family {
	return []family{
		{
			name:      "manifest",
			maxSize:   16 << 20,
			repoExact: []string{"manifest.pb"},
			decode:    decodeManifest,
		},
		{
			name:         "refs",
			maxSize:      64 << 20,
			repoPrefixes: []string{"checkpoints/"},
			repoMatch:    func(k string) bool { return strings.HasSuffix(k, "/refs.pb") },
			decode:       decodeRefSnapshot,
		},
		{
			name:      "access",
			maxSize:   4 << 20,
			repoExact: []string{"access.json"},
			decode:    decodeAccess,
		},
		{
			name:         "identity",
			maxSize:      4 << 20,
			rootPrefixes: []string{"users/", "owners/", "orgs/"},
			rootMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			decode:       decodeIdentity,
		},
		{
			name:         "collab",
			maxSize:      8 << 20,
			repoPrefixes: []string{"issues/", "pulls/"},
			repoMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			decode:       decodeCollab,
		},
		{
			name:         "packages",
			maxSize:      16 << 20,
			repoPrefixes: []string{"packages/registry/manifests/", "packages/registry/tags/"},
			decode:       decodePackage,
		},
		{
			name:         "threads",
			maxSize:      16 << 20,
			repoPrefixes: []string{"threads/"},
			repoMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			rootPrefixes: []string{"threads/"},
			rootMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			decode:       decodeThreads,
		},
		{
			name:         "moderation",
			maxSize:      8 << 20,
			repoPrefixes: []string{"meta/moderation/"},
			repoMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			repoExact:    []string{"meta/moderation.json"},
			rootPrefixes: []string{"moderation/"},
			rootMatch:    func(k string) bool { return strings.HasSuffix(k, ".json") },
			decode:       decodeModeration,
		},
	}
}

// ---- shared helpers ---------------------------------------------------------

func baseName(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// jsonInto validates that raw is a JSON object and unmarshals it into v.
func jsonInto(raw []byte, v any) *Problem {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return problem(CheckDecode, "empty object")
	}
	if t[0] != '{' {
		return problem(CheckDecode, "not a JSON object (starts with %q)", string(t[:1]))
	}
	if err := json.Unmarshal(t, v); err != nil {
		return problem(CheckDecode, "invalid JSON: %v", err)
	}
	return nil
}

func requireVersion(v, want int) *Problem {
	if v == 0 {
		return problem(CheckVersion, "missing schema version (want %d)", want)
	}
	if v != want {
		return problem(CheckVersion, "unknown schema version %d (want %d)", v, want)
	}
	return nil
}

// requireAtLeast is the CAS-revision rule: a document whose version field is
// its CAS token must have been written at least once.
func requireAtLeast(v, min int) *Problem {
	if v == 0 {
		return problem(CheckVersion, "missing document version (want >= %d)", min)
	}
	if v < min {
		return problem(CheckVersion, "unknown document version %d (want >= %d)", v, min)
	}
	return nil
}

func requireString(field, value string) *Problem {
	if strings.TrimSpace(value) == "" {
		return problem(CheckRequired, "%s is required but empty", field)
	}
	return nil
}

// isHexLen reports whether s is exactly n lowercase/uppercase hex characters.
func isHexLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// repoFromKey extracts "owner/name" from repos/<o>/<r>/…; ok=false otherwise.
func repoFromKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "repos/")
	if !ok {
		return "", false
	}
	owner, rest, ok := strings.Cut(rest, "/")
	if !ok || owner == "" {
		return "", false
	}
	name, _, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return "", false
	}
	return owner + "/" + name, true
}

// ---- protobuf families ------------------------------------------------------

func decodeManifest(key string, raw []byte) *Problem {
	var m proto.Manifest
	if err := m.Unmarshal(raw); err != nil {
		return problem(CheckDecode, "manifest does not decode as protobuf: %v", err)
	}
	if m.FormatVersion == 0 {
		return problem(CheckVersion, "missing manifest format_version (want %d)", proto.WALFormatVersion)
	}
	if m.FormatVersion != proto.WALFormatVersion {
		return problem(CheckVersion, "unknown manifest format_version %d (want %d)", m.FormatVersion, proto.WALFormatVersion)
	}
	if p := requireString("manifest.repo", m.Repo); p != nil {
		return p
	}
	if want, ok := repoFromKey(key); ok && m.Repo != want {
		return problem(CheckRequired, "manifest.repo %q does not match its key path (%q)", m.Repo, want)
	}
	if m.ObjectFormat != "sha1" && m.ObjectFormat != "sha256" {
		return problem(CheckRequired, "manifest.object_format %q is not sha1|sha256", m.ObjectFormat)
	}
	for i, seg := range m.LogSegments {
		if p := requireString(fmt.Sprintf("manifest.log_segments[%d].key", i), seg.Key); p != nil {
			return p
		}
		if seg.LastSeq < seg.FirstSeq {
			return problem(CheckRequired, "manifest.log_segments[%d] ends before it starts (%d < %d)", i, seg.LastSeq, seg.FirstSeq)
		}
	}
	for i, pack := range m.Packs {
		if p := requireString(fmt.Sprintf("manifest.packs[%d].checksum", i), pack.Checksum); p != nil {
			return p
		}
	}
	if m.Checkpoint != nil && strings.TrimSpace(m.Checkpoint.Key) == "" {
		return problem(CheckRequired, "manifest.checkpoint.key is empty")
	}
	return nil
}

func decodeRefSnapshot(key string, raw []byte) *Problem {
	var r proto.RefSnapshot
	if err := r.Unmarshal(raw); err != nil {
		return problem(CheckDecode, "refs snapshot does not decode as protobuf: %v", err)
	}
	if r.ObjectFormat != "sha1" && r.ObjectFormat != "sha256" {
		return problem(CheckRequired, "refs snapshot object_format %q is not sha1|sha256", r.ObjectFormat)
	}
	for i, ref := range r.Refs {
		if p := requireString(fmt.Sprintf("refs[%d].name", i), ref.Name); p != nil {
			return p
		}
		if ref.Name != "HEAD" && !strings.HasPrefix(ref.Name, "refs/") {
			return problem(CheckRequired, "refs[%d].name %q is neither HEAD nor a refs/ name", i, ref.Name)
		}
		if ref.Oid == "" {
			if ref.Name != "HEAD" {
				return problem(CheckRequired, "refs[%d] (%s) has an empty oid", i, ref.Name)
			}
			continue
		}
		n := 40
		if r.ObjectFormat == "sha256" {
			n = 64
		}
		if !isHexLen(ref.Oid, n) {
			return problem(CheckRequired, "refs[%d] (%s) oid %q is not %d hex chars", i, ref.Name, ref.Oid, n)
		}
	}
	return nil
}

// ---- JSON families ----------------------------------------------------------

func decodeAccess(key string, raw []byte) *Problem {
	var d struct {
		Version    int    `json:"version"`
		Visibility string `json:"visibility"`
		UpdatedAt  string `json:"updated_at"`
	}
	if p := jsonInto(raw, &d); p != nil {
		return p
	}
	if p := requireAtLeast(d.Version, 1); p != nil {
		return p
	}
	switch strings.ToLower(d.Visibility) {
	case "public", "authenticated", "private":
	default:
		return problem(CheckRequired, "access.visibility %q is not public|authenticated|private", d.Visibility)
	}
	return nil
}

func decodeIdentity(key string, raw []byte) *Problem {
	switch baseName(key) {
	case "user.json":
		var d struct {
			Version  int    `json:"version"`
			Username string `json:"username"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		return requireString("user.username", d.Username)
	case "profile.json":
		if strings.HasPrefix(key, "owners/") {
			// owners/<owner>/profile.json is the owner-scoped sidecar
			// (display_name/location/timezone/bio_markdown, no version and no
			// principal); it is not the user Profile document below.
			return decodeGenericJSON(raw)
		}
		var d struct {
			Version   int    `json:"version"`
			Principal string `json:"principal"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		return requireString("profile.principal", d.Principal)
	case "org.json":
		var d struct {
			Version int    `json:"version"`
			Org     string `json:"org"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		return requireString("org.org", d.Org)
	case "members.json":
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireVersion(d.Version, 1)
	case "blocks.json":
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireVersion(d.Version, 1)
	case "ref.json":
		var d struct {
			Version  int    `json:"version"`
			Username string `json:"username"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		return requireString("alias.username", d.Username)
	}
	// teams/<slug>.json and the invitation/inbox families: versioned docs whose
	// exact field set is owned by internal/identity. Validating the envelope
	// catches corruption without re-encoding another package's schema.
	var d struct {
		Version int `json:"version"`
	}
	if p := jsonInto(raw, &d); p != nil {
		return p
	}
	if d.Version != 0 {
		return requireVersion(d.Version, 1)
	}
	return nil
}

func decodeCollab(key string, raw []byte) *Problem {
	base := baseName(key)
	switch {
	case base == "thread.json" && strings.Contains(key, "/pulls/"):
		var d struct {
			TID     string `json:"tid"`
			Num     int    `json:"num"`
			Kind    string `json:"kind"`
			Version int    `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		if d.Num <= 0 {
			return problem(CheckRequired, "review thread num is %d", d.Num)
		}
		if p := requireString("review thread tid", d.TID); p != nil {
			return p
		}
		if d.Kind != "review_thread" {
			return problem(CheckRequired, "review thread kind %q is not review_thread", d.Kind)
		}
		return nil
	case base == "thread.json":
		var d struct {
			Num     int    `json:"num"`
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			State   string `json:"state"`
			Version int    `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		if d.Num <= 0 {
			return problem(CheckRequired, "thread num is %d", d.Num)
		}
		if d.Kind != "issue" && d.Kind != "pr" {
			return problem(CheckRequired, "thread kind %q is not issue|pr", d.Kind)
		}
		if p := requireString("thread.title", d.Title); p != nil {
			return p
		}
		return requireString("thread.state", d.State)
	case base == "pr.json":
		var d struct {
			Num     int    `json:"num"`
			Kind    string `json:"kind"`
			Version int    `json:"version"`
			Base    struct {
				Repo string `json:"repo"`
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
			} `json:"base"`
			Head struct {
				Repo string `json:"repo"`
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
			} `json:"head"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireAtLeast(d.Version, 1); p != nil {
			return p
		}
		if d.Num <= 0 {
			return problem(CheckRequired, "pr num is %d", d.Num)
		}
		if d.Kind != "pr" {
			return problem(CheckRequired, "pr kind %q is not pr", d.Kind)
		}
		for _, ep := range []struct{ name, repo, ref, sha string }{
			{"base", d.Base.Repo, d.Base.Ref, d.Base.SHA},
			{"head", d.Head.Repo, d.Head.Ref, d.Head.SHA},
		} {
			if p := requireString("pr."+ep.name+".repo", ep.repo); p != nil {
				return p
			}
			if p := requireString("pr."+ep.name+".ref", ep.ref); p != nil {
				return p
			}
			if p := requireString("pr."+ep.name+".sha", ep.sha); p != nil {
				return p
			}
		}
		return nil
	case base == "mergeable.json":
		var d struct {
			BaseRef string `json:"base_ref"`
			BaseSHA string `json:"base_sha"`
			HeadSHA string `json:"head_sha"`
			State   string `json:"state"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		for name, v := range map[string]string{"base_ref": d.BaseRef, "base_sha": d.BaseSHA, "head_sha": d.HeadSHA} {
			if p := requireString("mergeable."+name, v); p != nil {
				return p
			}
		}
		switch d.State {
		case "clean", "dirty", "behind", "up_to_date", "unknown":
			return nil
		}
		return problem(CheckRequired, "mergeable.state %q is not a documented state", d.State)
	case base == "index.json" && strings.Contains(key, "/issues/"):
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireVersion(d.Version, 1)
	case base == "review-requests.json":
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireVersion(d.Version, 1)
	case base == "forks.json":
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireAtLeast(d.Version, 1)
	case base == "fork.json":
		var d struct {
			Parent  string `json:"parent"`
			Version int    `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireAtLeast(d.Version, 1); p != nil {
			return p
		}
		return requireString("fork.parent", d.Parent)
	case strings.Contains(key, "/threads/") && strings.Contains(key, "/events/"):
		var d struct {
			Kind string `json:"kind"`
			Seq  int    `json:"seq"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if d.Seq <= 0 {
			return problem(CheckRequired, "review-thread comment seq is %d", d.Seq)
		}
		return requireString("review-thread comment kind", d.Kind)
	case strings.Contains(key, "/events/"):
		var d struct {
			Seq  int    `json:"seq"`
			Type string `json:"type"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if d.Seq <= 0 {
			return problem(CheckRequired, "event seq is %d", d.Seq)
		}
		return requireString("event.type", d.Type)
	case strings.Contains(key, "/reviews/"):
		var d struct {
			Kind      string `json:"kind"`
			Seq       int    `json:"seq"`
			State     string `json:"state"`
			CommitSHA string `json:"commit_sha"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if d.Seq <= 0 {
			return problem(CheckRequired, "review seq is %d", d.Seq)
		}
		if p := requireString("review.kind", d.Kind); p != nil {
			return p
		}
		if d.Kind == "review" {
			switch d.State {
			case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
			default:
				return problem(CheckRequired, "review.state %q is not APPROVED|CHANGES_REQUESTED|COMMENTED", d.State)
			}
			if d.CommitSHA != "" && !isHexLen(d.CommitSHA, 40) && !isHexLen(d.CommitSHA, 64) {
				return problem(CheckRequired, "review.commit_sha %q is not a full hex oid", d.CommitSHA)
			}
		}
		return nil
	}
	return decodeGenericJSON(raw)
}

func decodeGenericJSON(raw []byte) *Problem {
	var obj map[string]json.RawMessage
	return jsonInto(raw, &obj)
}

func decodePackage(key string, raw []byte) *Problem {
	var d struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if p := jsonInto(raw, &d); p != nil {
		return p
	}
	// OCI Distribution §3: both image manifests and image indexes carry
	// schemaVersion 2; the tag family stores the same bytes, so one codec.
	if p := requireVersion(d.SchemaVersion, 2); p != nil {
		return p
	}
	if p := requireString("package.mediaType", d.MediaType); p != nil {
		return p
	}
	if len(d.Manifests) > 0 {
		for i, m := range d.Manifests {
			if !validDigest(m.Digest) {
				return problem(CheckRequired, "package.manifests[%d].digest %q is not sha256:<64 hex>", i, m.Digest)
			}
		}
		return nil
	}
	if d.Config == nil {
		return problem(CheckRequired, "package manifest has neither config nor manifests[]")
	}
	if !validDigest(d.Config.Digest) {
		return problem(CheckRequired, "package.config.digest %q is not sha256:<64 hex>", d.Config.Digest)
	}
	for i, l := range d.Layers {
		if !validDigest(l.Digest) {
			return problem(CheckRequired, "package.layers[%d].digest %q is not sha256:<64 hex>", i, l.Digest)
		}
	}
	return nil
}

func validDigest(s string) bool {
	rest, ok := strings.CutPrefix(s, "sha256:")
	return ok && isHexLen(rest, 64)
}

func decodeThreads(key string, raw []byte) *Problem {
	base := baseName(key)
	switch {
	case key == "threads/queue/index.json":
		var d struct {
			Version int `json:"version"`
			Queued  []struct {
				Repo  string `json:"repo"`
				RunID string `json:"run"`
				JobID string `json:"job"`
			} `json:"queued"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		for i, row := range d.Queued {
			if row.Repo == "" || row.RunID == "" || row.JobID == "" {
				return problem(CheckRequired, "queue index row %d lacks repo/run/job", i)
			}
		}
		return nil
	case strings.HasSuffix(key, "/threads/queue/head.json"):
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireVersion(d.Version, 1)
	case strings.Contains(key, "/threads/queue/") && base != "index.json" && base != "head.json":
		var d struct {
			Seq   string `json:"seq"`
			RunID string `json:"run_id"`
			JobID string `json:"job_id"`
			State string `json:"state"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireString("queue entry seq", d.Seq); p != nil {
			return p
		}
		if p := requireString("queue entry run_id", d.RunID); p != nil {
			return p
		}
		if p := requireString("queue entry job_id", d.JobID); p != nil {
			return p
		}
		return requireString("queue entry state", d.State)
	case strings.Contains(key, "/artifacts/") && base == "manifest.json":
		var d struct {
			Name  string `json:"name"`
			Run   string `json:"run"`
			Job   string `json:"job"`
			Files []struct {
				Path   string `json:"path"`
				Size   int64  `json:"size"`
				SHA256 string `json:"sha256"`
				Index  int    `json:"index"`
			} `json:"files"`
			TotalSize int64 `json:"total_size"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireString("artifact.name", d.Name); p != nil {
			return p
		}
		if p := requireString("artifact.run", d.Run); p != nil {
			return p
		}
		if p := requireString("artifact.job", d.Job); p != nil {
			return p
		}
		for i, f := range d.Files {
			if !isHexLen(f.SHA256, 64) {
				return problem(CheckRequired, "artifact.files[%d].sha256 %q is not 64 hex chars", i, f.SHA256)
			}
			if f.Size < 0 {
				return problem(CheckRequired, "artifact.files[%d].size is negative (%d)", i, f.Size)
			}
		}
		if d.TotalSize < 0 {
			return problem(CheckRequired, "artifact.total_size is negative (%d)", d.TotalSize)
		}
		return nil
	case strings.Contains(key, "/threads/artifact-expiry/"):
		var d struct {
			Run     string `json:"run"`
			Name    string `json:"name"`
			Expires string `json:"expires_at"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireString("artifact-expiry run", d.Run); p != nil {
			return p
		}
		if p := requireString("artifact-expiry name", d.Name); p != nil {
			return p
		}
		return requireString("artifact-expiry expires_at", d.Expires)
	case strings.Contains(key, "/jobs/") && strings.HasSuffix(key, ".json"):
		var d struct {
			ID      string `json:"id"`
			RunID   string `json:"run_id"`
			Repo    string `json:"repo"`
			State   string `json:"state"`
			Version int    `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		if p := requireString("job.id", d.ID); p != nil {
			return p
		}
		if p := requireString("job.run_id", d.RunID); p != nil {
			return p
		}
		if p := requireString("job.repo", d.Repo); p != nil {
			return p
		}
		return requireString("job.state", d.State)
	case strings.Contains(key, "/threads/runs/") && strings.HasSuffix(key, ".json") && !strings.Contains(key, "/artifacts/"):
		var d struct {
			ID      string `json:"id"`
			Repo    string `json:"repo"`
			Event   string `json:"event"`
			State   string `json:"state"`
			Version int    `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireVersion(d.Version, 1); p != nil {
			return p
		}
		if p := requireString("run.id", d.ID); p != nil {
			return p
		}
		if p := requireString("run.repo", d.Repo); p != nil {
			return p
		}
		if p := requireString("run.event", d.Event); p != nil {
			return p
		}
		return requireString("run.state", d.State)
	}
	return decodeGenericJSON(raw)
}

func decodeModeration(key string, raw []byte) *Problem {
	base := baseName(key)
	switch {
	case key == "moderation/reports/index.json":
		var d struct {
			Version int `json:"version"`
			Open    []struct {
				ID   int    `json:"id"`
				Repo string `json:"repo"`
			} `json:"open"`
			Resolved []struct {
				ID   int    `json:"id"`
				Repo string `json:"repo"`
			} `json:"resolved_recent"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireAtLeast(d.Version, 1); p != nil {
			return p
		}
		for i, row := range d.Open {
			if row.ID <= 0 || row.Repo == "" {
				return problem(CheckRequired, "global report row %d lacks id/repo", i)
			}
		}
		for i, row := range d.Resolved {
			if row.ID <= 0 || row.Repo == "" {
				return problem(CheckRequired, "global report row %d lacks id/repo", i)
			}
		}
		return nil
	case strings.HasSuffix(key, "/meta/moderation/reports/index.json"):
		var d struct {
			Version int `json:"version"`
			Open    []struct {
				ID int `json:"id"`
			} `json:"open"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if p := requireAtLeast(d.Version, 1); p != nil {
			return p
		}
		for i, row := range d.Open {
			if row.ID <= 0 {
				return problem(CheckRequired, "report row %d has id %d", i, row.ID)
			}
		}
		return nil
	case strings.Contains(key, "/meta/moderation/reports/") && strings.HasSuffix(key, ".json"):
		var d struct {
			ID        int    `json:"id"`
			Repo      string `json:"repo"`
			Kind      string `json:"kind"`
			Target    string `json:"target"`
			Reporter  string `json:"reporter"`
			Reason    string `json:"reason"`
			CreatedAt string `json:"created_at"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if d.ID <= 0 {
			return problem(CheckRequired, "report id is %d", d.ID)
		}
		for name, v := range map[string]string{
			"repo": d.Repo, "kind": d.Kind, "target": d.Target,
			"reporter": d.Reporter, "reason": d.Reason, "created_at": d.CreatedAt,
		} {
			if p := requireString("report."+name, v); p != nil {
				return p
			}
		}
		return nil
	case base == "moderation.json":
		var d struct {
			Version int `json:"version"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		return requireAtLeast(d.Version, 1)
	case strings.Contains(key, "moderation/audit/") && strings.HasSuffix(key, ".json"):
		var d struct {
			Seq    int    `json:"seq"`
			At     string `json:"at"`
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
		}
		if p := jsonInto(raw, &d); p != nil {
			return p
		}
		if d.Seq <= 0 {
			return problem(CheckRequired, "audit seq is %d", d.Seq)
		}
		for name, v := range map[string]string{"at": d.At, "actor": d.Actor, "action": d.Action, "target": d.Target} {
			if p := requireString("audit."+name, v); p != nil {
				return p
			}
		}
		return nil
	}
	return decodeGenericJSON(raw)
}
