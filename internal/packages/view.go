// view.go — the read projections behind the Packages page and its JSON API
// twin (docs/features/14 §7). Both surfaces render the SAME structs, so the
// page and the API cannot disagree about a tag, a digest or a platform.
//
// Cost model: listing is one tags LIST plus one pointer read and one manifest
// HEAD per tag, plus one manifest GET per index (platforms). No blob is read,
// nothing here is a pull path, and the tag page is capped (§7).
package packages

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// DefaultTagPage bounds one Packages listing (§7): a page is a human surface,
// and an unbounded tag list would mean one manifest HEAD per tag.
const DefaultTagPage = 200

// TagInfo is one registry tag and the manifest it names.
type TagInfo struct {
	Tag       string   `json:"tag"`
	Digest    string   `json:"digest"`
	MediaType string   `json:"media_type"`
	Size      int64    `json:"size"`
	UpdatedAt string   `json:"updated_at"`
	Platforms []string `json:"platforms"`
	// Pull is the ready-to-run pull command (filled by the HTTP surface,
	// which is the only layer that knows the external host).
	Pull string `json:"pull,omitempty"`
}

// ManifestView is one stored manifest with the tags aiming at it.
type ManifestView struct {
	Digest    string          `json:"digest"`
	MediaType string          `json:"media_type"`
	Size      int64           `json:"size"`
	UpdatedAt string          `json:"updated_at"`
	Tags      []string        `json:"tags"`
	Platforms []string        `json:"platforms"`
	Manifest  json.RawMessage `json:"manifest,omitempty"`
}

// RepoPackages is the collection response (GET …/api/packages) and the page's
// data source.
type RepoPackages struct {
	Repository string         `json:"repository"`
	Packages   []TagInfo      `json:"packages"`
	Manifests  []ManifestView `json:"manifests"`
	TotalSize  int64          `json:"total_size"`
	More       bool           `json:"more"`
}

// indexPlatformDoc is the OCI index / Docker manifest-list shape.
type indexPlatformDoc struct {
	Manifests []struct {
		Platform *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// isIndexMediaType reports whether mt describes a multi-platform index.
func isIndexMediaType(mt string) bool {
	return strings.Contains(mt, "image.index") || strings.Contains(mt, "manifest.list")
}

// platformsOf returns "os/arch[/variant]" for every platform in an index body
// (empty for a single-platform manifest, which carries no platform member).
func platformsOf(raw []byte, mediaType string) []string {
	if !isIndexMediaType(mediaType) {
		return []string{}
	}
	var doc indexPlatformDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{}
	}
	out := make([]string, 0, len(doc.Manifests))
	for _, m := range doc.Manifests {
		if m.Platform == nil || m.Platform.OS == "" || m.Platform.Architecture == "" {
			continue
		}
		p := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			p += "/" + m.Platform.Variant
		}
		out = append(out, p)
	}
	return out
}

// ListPackages reads up to limit tags with their manifest metadata. The page
// and the JSON collection both use it; a tag whose pointer or manifest
// vanished under us is skipped (never a broken row).
func (s *Service) ListPackages(ctx context.Context, owner, repo string, limit int) (*RepoPackages, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > DefaultTagPage {
		limit = DefaultTagPage
	}
	tags, more, oerr := s.ListTags(ctx, owner, repo, "", limit)
	if oerr != nil {
		return nil, oerr
	}
	out := &RepoPackages{
		Repository: owner + "/" + repo,
		Packages:   []TagInfo{},
		Manifests:  []ManifestView{},
		More:       more,
	}
	byDigest := map[string]int{} // digest → index in out.Manifests
	for _, tag := range tags {
		raw, _, gerr := store.GetBytes(ctx, s.Store, tagKey(owner, repo, tag), store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				continue // the pointer vanished under us
			}
			return nil, mapStoreErr(gerr)
		}
		var doc tagDoc
		if json.Unmarshal(raw, &doc) != nil || !IsDigest(doc.Digest) {
			continue
		}
		m, merr := s.manifestView(ctx, owner, repo, doc.Digest, false)
		if merr != nil {
			continue
		}
		info := TagInfo{
			Tag:       tag,
			Digest:    doc.Digest,
			MediaType: m.MediaType,
			Size:      m.Size,
			UpdatedAt: doc.UpdatedAt,
			Platforms: m.Platforms,
		}
		out.Packages = append(out.Packages, info)
		at, ok := byDigest[doc.Digest]
		if !ok {
			view := *m
			view.Tags = []string{tag}
			view.UpdatedAt = doc.UpdatedAt
			byDigest[doc.Digest] = len(out.Manifests)
			out.Manifests = append(out.Manifests, view)
			out.TotalSize += view.Size
		} else {
			out.Manifests[at].Tags = append(out.Manifests[at].Tags, tag)
			if doc.UpdatedAt > out.Manifests[at].UpdatedAt {
				out.Manifests[at].UpdatedAt = doc.UpdatedAt
			}
		}
	}
	sort.Slice(out.Manifests, func(i, j int) bool {
		if out.Manifests[i].UpdatedAt != out.Manifests[j].UpdatedAt {
			return out.Manifests[i].UpdatedAt > out.Manifests[j].UpdatedAt
		}
		return out.Manifests[i].Digest < out.Manifests[j].Digest
	})
	return out, nil
}

// GetPackage reads one manifest by digest with its tag set and body.
func (s *Service) GetPackage(ctx context.Context, owner, repo, digest string) (*ManifestView, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if !IsDigest(digest) {
		return nil, errDigestInvalid("invalid manifest digest")
	}
	view, oerr := s.manifestView(ctx, owner, repo, digest, true)
	if oerr != nil {
		if oerr.Status == 404 {
			return nil, errManifestUnknown(digest)
		}
		return nil, oerr
	}
	tags, oerr := s.tagsForDigest(ctx, owner, repo, digest)
	if oerr != nil {
		return nil, oerr
	}
	view.Tags = tags
	if len(tags) > 0 {
		if raw, _, gerr := store.GetBytes(ctx, s.Store, tagKey(owner, repo, tags[0]), store.GetOptions{}); gerr == nil {
			var doc tagDoc
			if json.Unmarshal(raw, &doc) == nil {
				view.UpdatedAt = doc.UpdatedAt
			}
		}
	}
	return view, nil
}

// manifestView reads one manifest's metadata (HEAD for size; the body only
// when wantBody or when the media type is an index whose platforms we need).
func (s *Service) manifestView(ctx context.Context, owner, repo, digest string, wantBody bool) (*ManifestView, *Error) {
	key := manifestKey(owner, repo, DigestHex(digest))
	meta, err := s.Store.Head(ctx, key)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if meta == nil {
		return nil, errManifestUnknown(digest)
	}
	view := &ManifestView{
		Digest:    digest,
		Size:      meta.Size,
		Tags:      []string{},
		Platforms: []string{},
	}
	raw, gm, gerr := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if gerr != nil {
		if !store.IsNotFound(gerr) {
			return nil, mapStoreErr(gerr)
		}
		return nil, errManifestUnknown(digest)
	}
	view.MediaType = manifestMediaType(raw)
	view.Platforms = platformsOf(raw, view.MediaType)
	if wantBody {
		body := make([]byte, len(raw))
		copy(body, raw)
		view.Manifest = body
	}
	if view.Size <= 0 {
		view.Size = int64(gm.Size)
	}
	return view, nil
}

// tagsForDigest scans the tag prefix for pointers whose digest matches (one
// LIST + one pointer read per tag; a single-object view, never a hot path).
func (s *Service) tagsForDigest(ctx context.Context, owner, repo, digest string) ([]string, *Error) {
	out := []string{}
	prefix := tagsPrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, mapStoreErr(err)
	}
	for _, key := range keys {
		raw, _, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, mapStoreErr(err)
		}
		var doc tagDoc
		if json.Unmarshal(raw, &doc) == nil && doc.Digest == digest {
			out = append(out, strings.TrimPrefix(key, prefix))
		}
	}
	sort.Strings(out)
	return out, nil
}
