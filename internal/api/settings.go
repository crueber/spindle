package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
)

// --- settings endpoints (D24, 07_api.md §11) -----------------------------------------

type settingsBody struct {
	Revision  uint64 `json:"revision"`
	Author    string `json:"author"`
	UpdatedAt string `json:"updated_at"`
	Message   string `json:"message"`
	TOML      string `json:"toml"`
}

func settingsWire(d SettingsDoc) settingsBody {
	b := settingsBody{
		Revision: d.Revision,
		Author:   d.Author,
		Message:  d.Message,
		TOML:     d.TOML,
	}
	if d.Revision > 0 {
		b.UpdatedAt = d.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return b
}

func (h *handlers) settingsGet(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	doc, err := h.env.Repo.Settings(r.Context(), RepoOf(r))
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, settingsWire(doc))
}

// WriteError is one failed API mutation in a form non-HTTP consumers (the
// server-rendered settings pages) can render: Status is the HTTP status the
// JSON handler writes and Message is its plain-text body. The JSON handlers
// and the pages therefore share one mapping.
type WriteError struct {
	Status  int
	Message string
}

// PublishSettings validates and publishes a repo settings body (the settings
// PUT semantics): body = TOML ≤ 16 KiB else 413; invalid → 400, nothing
// published. The caller has already enforced the admin gate.
func PublishSettings(ctx context.Context, e *Env, id git.RepoId, body []byte, message, author string) (uint64, *WriteError) {
	if e.Repo == nil {
		return 0, &WriteError{Status: http.StatusServiceUnavailable, Message: "repo view not configured"}
	}
	if len(body) > config.MaxRepoSettingsBytes {
		return 0, &WriteError{Status: http.StatusRequestEntityTooLarge, Message: "settings body exceeds 16 KiB"}
	}
	if _, err := config.ParseRepoSettings(body); err != nil {
		return 0, &WriteError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	rev, err := e.Repo.PublishSettings(ctx, id, body, message, author)
	if err != nil {
		return 0, viewError(err)
	}
	return rev, nil
}

// DeleteSettings publishes an empty body (back to host config) at a new
// revision. The caller has already enforced the admin gate.
func DeleteSettings(ctx context.Context, e *Env, id git.RepoId, author string) (uint64, *WriteError) {
	if e.Repo == nil {
		return 0, &WriteError{Status: http.StatusServiceUnavailable, Message: "repo view not configured"}
	}
	rev, err := e.Repo.PublishSettings(ctx, id, nil, "", author)
	if err != nil {
		return 0, viewError(err)
	}
	return rev, nil
}

// viewError maps a RepoView error onto the §2 wire status (the mapViewErr
// rule) for non-HTTP callers.
func viewError(err error) *WriteError {
	switch {
	case errors.Is(err, ErrNotFound):
		return &WriteError{Status: http.StatusNotFound, Message: err.Error()}
	case errors.Is(err, ErrExists):
		return &WriteError{Status: http.StatusConflict, Message: err.Error()}
	default:
		return &WriteError{Status: http.StatusServiceUnavailable, Message: err.Error()}
	}
}

// settingsPut publishes validated repo settings through the WAL (§11): body
// = TOML ≤ 16 KiB else 413; validated against THIS host's build; invalid →
// 400 + reason, nothing published; valid → 200 {"revision":N}.
func (h *handlers) settingsPut(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthAdmin) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxRepoSettingsBytes+1))
	if err != nil {
		writePlain(w, http.StatusRequestEntityTooLarge, "settings body exceeds 16 KiB")
		return
	}
	p := h.env.PrincipalOf(r)
	rev, werr := PublishSettings(r.Context(), h.env, RepoOf(r), body, r.URL.Query().Get("message"), p.Name)
	if werr != nil {
		writePlain(w, werr.Status, werr.Message)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Revision uint64 `json:"revision"`
	}{Revision: rev})
}

// settingsDelete publishes empty (back to host config) at a new revision.
func (h *handlers) settingsDelete(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthAdmin) {
		return
	}
	p := h.env.PrincipalOf(r)
	rev, werr := DeleteSettings(r.Context(), h.env, RepoOf(r), p.Name)
	if werr != nil {
		writePlain(w, werr.Status, werr.Message)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Revision uint64 `json:"revision"`
	}{Revision: rev})
}

// effectiveWith merges a published settings body over the host config
// (D24 "with_settings"); an empty body is the host config itself.
func (e *Env) effectiveWith(base *config.Config, body []byte) (*config.Config, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return base, nil
	}
	rs, err := config.ParseRepoSettings(body)
	if err != nil {
		return nil, err
	}
	return rs.Merge(base)
}

// --- GET …/settings/effective (application/toml) ---------------------------------------

type effectiveSections struct {
	Bundles     config.Bundles     `toml:"bundles"`
	Maintenance config.Maintenance `toml:"maintenance"`
	Compaction  config.Compaction  `toml:"compaction"`
	Upstream    config.Upstream    `toml:"upstream"`
}

func (h *handlers) settingsEffective(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	doc, err := h.env.Repo.Settings(r.Context(), RepoOf(r))
	if err != nil {
		mapViewErr(w, err)
		return
	}
	eff, err := h.env.effectiveWith(h.env.Cfg, []byte(doc.TOML))
	if err != nil {
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	sections := effectiveSections{
		Bundles:     eff.Bundles,
		Maintenance: eff.Maintenance,
		Compaction:  eff.Compaction,
		Upstream:    eff.Upstream,
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(sections); err != nil {
		writePlain(w, http.StatusInternalServerError, "encode: "+err.Error())
		return
	}
	// No host secrets, no token_env values (§11): drop the key entirely.
	out := strings.Builder{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "token_env") {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	w.Header().Set("Cache-Control", ccNoStore)
	w.Header().Set("Content-Type", "application/toml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out.String()))
}

func (h *handlers) settingsHistory(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	hist, err := h.env.Repo.SettingsHistory(r.Context(), RepoOf(r))
	if err != nil {
		mapViewErr(w, err)
		return
	}
	hist.Entries = nonNil(hist.Entries)
	writeCached(w, r, ccNoStore, "", http.StatusOK, hist)
}

// --- GET …/settings/describe + POST …/settings/validate (§3 shape verbatim) -------------

type StrategyBody struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Base          string   `json:"base,omitempty"`
	Schedule      string   `json:"schedule"`
	ScheduleHuman string   `json:"schedule_human"`
	Next          string   `json:"next"`
	Keep          int      `json:"keep"`
	BackfillMax   int      `json:"backfill_max"`
	MinCommits    int      `json:"min_commits"`
	Refs          []string `json:"refs"`
	Chain         bool     `json:"chain"`
	Filter        string   `json:"filter"`
}

// HostFacts is the this_host block of the describe shape (§3): the host
// config's placement/disk facts for the machine answering the read.
type HostFacts struct {
	Name             string   `json:"name"`
	Serves           bool     `json:"serves"`
	Maintains        bool     `json:"maintains"`
	Disk             string   `json:"disk"`
	MaxPackBytes     int64    `json:"max_pack_bytes"`
	CacheBudgetBytes int64    `json:"cache_budget_bytes"`
	Roles            []string `json:"roles"`
}

// MaintenanceBody is the describe shape's [maintenance] block.
type MaintenanceBody struct {
	Checkpoints  bool      `json:"checkpoints"`
	IntervalSecs int64     `json:"interval_secs"`
	ThisHost     HostFacts `json:"this_host"`
}

// CompactionBody is the describe shape's [compaction] block.
type CompactionBody struct {
	Enabled                 bool   `json:"enabled"`
	Factor                  int    `json:"factor"`
	TriggerPacks            int    `json:"trigger_packs"`
	TriggerBytes            int64  `json:"trigger_bytes"`
	LeaseTTLSecs            int64  `json:"lease_ttl_secs"`
	RetentionSupersededSecs int64  `json:"retention_superseded_secs"`
	Engine                  string `json:"engine"`
}

// UpstreamBody is the describe shape's [upstream] block.
type UpstreamBody struct {
	Git                string   `json:"git"`
	Lfs                string   `json:"lfs"`
	TokenEnv           bool     `json:"token_env"`
	Follow             []string `json:"follow"`
	FollowIntervalSecs int64    `json:"follow_interval_secs"`
	LastRound          string   `json:"last_round,omitempty"`
}

// FieldBody is one overridden settings key: the effective value and the
// host value it replaced.
type FieldBody struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	HostValue any    `json:"host_value"`
	Source    string `json:"source"` // "host" | "setting"
}

// DescribeBody is the §3 describe shape (GET …/settings/describe). It is
// exported so non-HTTP consumers (the server-rendered settings pages) read
// the exact same projection the JSON API serves.
type DescribeBody struct {
	Settings    settingsBody    `json:"settings"`
	Sections    []string        `json:"sections"`
	Strategies  []StrategyBody  `json:"strategies"`
	Bundles     []BundleInfo    `json:"bundles"`
	Maintenance MaintenanceBody `json:"maintenance"`
	Compaction  CompactionBody  `json:"compaction"`
	Upstream    UpstreamBody    `json:"upstream"`
	Fields      []FieldBody     `json:"fields"`
	HeadSeq     uint64          `json:"head_seq"`
	OK          *bool           `json:"ok,omitempty"`
	Errors      []string        `json:"errors,omitempty"`
}

// describeSections names the settings sections the describe shape reports.
var describeSections = []string{"bundles", "maintenance", "compaction", "upstream"}

// SettingsDocument reads the stored settings document together with both
// projections the settings pages need: the decoded RepoSettings (which
// sections the repository overrides, exactly as stored) and the merged
// effective config (the §11 "with_settings" view). One merge path — the page
// and the JSON API cannot drift — and a corrupt document is the API's error,
// never a silently-empty section editor.
func SettingsDocument(ctx context.Context, e *Env, id git.RepoId) (SettingsDoc, *config.RepoSettings, *config.Config, error) {
	doc, err := e.Repo.Settings(ctx, id)
	if err != nil {
		return SettingsDoc{}, nil, nil, err
	}
	var stored *config.RepoSettings
	if len(bytes.TrimSpace([]byte(doc.TOML))) > 0 {
		stored, err = config.ParseRepoSettings([]byte(doc.TOML))
		if err != nil {
			return SettingsDoc{}, nil, nil, err
		}
	}
	eff, err := e.effectiveWith(e.Cfg, []byte(doc.TOML))
	if err != nil {
		return SettingsDoc{}, nil, nil, err
	}
	return doc, stored, eff, nil
}

// SettingsDescribe returns the §3 describe shape for non-HTTP consumers (the
// server-rendered Scheduled tab). It runs the exact projection the JSON
// handler serves — one code path, so the page and its API twin cannot drift.
func SettingsDescribe(ctx context.Context, e *Env, id git.RepoId) (DescribeBody, error) {
	doc, _, eff, err := SettingsDocument(ctx, e, id)
	if err != nil {
		return DescribeBody{}, err
	}
	return (&handlers{env: e}).describeShape(ctx, id, doc, eff), nil
}

func (h *handlers) settingsDescribe(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	body, err := SettingsDescribe(r.Context(), h.env, RepoOf(r))
	if err != nil {
		// 404/409/503 by the §2 rule (mapViewErr): a broken stored document
		// is a 503 with its reason, exactly as the pre-seam handler wrote it.
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, body)
}

// settingsValidate: the SAME describe shape for the WOULD-BE effective
// config (body applied), plus ok and errors[]. Never publishes (§11).
func (h *handlers) settingsValidate(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	ctx := r.Context()
	id := RepoOf(r)
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxRepoSettingsBytes+1))

	ok := true
	errs := []string{}
	var doc SettingsDoc
	eff := h.env.Cfg
	if len(bytes.TrimSpace(body)) > 0 {
		rs, err := config.ParseRepoSettings(body)
		if err != nil {
			ok = false
			errs = append(errs, err.Error())
		} else if err := rs.ValidateAgainst(h.env.Cfg); err != nil {
			ok = false
			errs = append(errs, err.Error())
		} else {
			if eff, err = rs.Merge(h.env.Cfg); err != nil {
				ok = false
				errs = append(errs, err.Error())
				eff = h.env.Cfg // keep the shape renderable on the host config
			}
		}
	} else {
		// No body: describe the stored would-be effective config.
		d, err := h.env.Repo.Settings(ctx, id)
		if err == nil {
			doc = d
			if eff, err = h.env.effectiveWith(h.env.Cfg, []byte(d.TOML)); err != nil {
				ok = false
				errs = append(errs, err.Error())
				eff = h.env.Cfg // keep the shape renderable on the host config
			}
		}
	}
	out := h.describeShape(ctx, id, doc, eff)
	out.OK = &ok
	out.Errors = errs
	writeCached(w, r, ccNoStore, "", http.StatusOK, out)
}

// describeShape fills the §3 describe shape for an effective config.
func (h *handlers) describeShape(ctx context.Context, id git.RepoId, doc SettingsDoc, eff *config.Config) DescribeBody {
	body := DescribeBody{
		Settings:   settingsWire(doc),
		Sections:   describeSections,
		Strategies: []StrategyBody{},
		Bundles:    []BundleInfo{},
		Fields:     []FieldBody{},
	}
	now := h.env.Now()
	for _, s := range eff.Bundles.Strategy {
		sb := StrategyBody{
			Name:          s.Name,
			Kind:          s.Kind,
			Base:          s.Base,
			Schedule:      s.Schedule,
			ScheduleHuman: cronHuman(s.Schedule),
			Keep:          s.Keep,
			BackfillMax:   s.BackfillMax,
			MinCommits:    s.MinCommits,
			Refs:          nonNil(s.Refs),
			Chain:         s.Chain,
			Filter:        s.Filter,
		}
		if next, ok := cronNext(s.Schedule, now); ok {
			sb.Next = next.UTC().Format(time.RFC3339)
		}
		body.Strategies = append(body.Strategies, sb)
	}

	// Bundles come from the WAL-derived overview (best effort: describe is
	// config-centric; an unavailable overview leaves the list empty).
	if ov, err := h.env.Repo.Overview(ctx, id); err == nil {
		body.Bundles = nonNil(ov.Bundles)
	}

	roles := []string{}
	hostname := h.env.Hostname
	if h.env.Cfg != nil {
		roles = nonNil(h.env.Cfg.Server.Roles)
	}
	// this_host facts are per-host; the host config is authoritative even
	// when a section merges from settings (placement is host-only).
	host := eff
	if h.env.Cfg != nil {
		host = h.env.Cfg
	}
	body.Maintenance = MaintenanceBody{
		Checkpoints:  eff.Maintenance.Checkpoints,
		IntervalSecs: int64(time.Duration(eff.Maintenance.Interval) / time.Second),
		ThisHost: HostFacts{
			Name:             hostname,
			Serves:           placementAllows(host.Placement.Serve, host.Placement.ServeExclude, hostname),
			Maintains:        placementAllows(host.Placement.Maintain, host.Placement.MaintainExclude, hostname),
			Disk:             host.Maintenance.Disk,
			MaxPackBytes:     int64(host.Maintenance.MaxPackBytes),
			CacheBudgetBytes: int64(host.Cache.MaxBytes),
			Roles:            roles,
		},
	}
	body.Compaction = CompactionBody{
		Enabled:                 eff.Compaction.Enabled,
		Factor:                  eff.Compaction.Factor,
		TriggerPacks:            eff.Compaction.TriggerPacks,
		TriggerBytes:            int64(eff.Compaction.TriggerBytes),
		LeaseTTLSecs:            int64(time.Duration(eff.Compaction.LeaseTTL) / time.Second),
		RetentionSupersededSecs: int64(time.Duration(eff.Compaction.RetentionSuperseded) / time.Second),
		Engine:                  eff.Compaction.Engine,
	}
	body.Upstream = UpstreamBody{
		Git:                eff.Upstream.Git,
		Lfs:                eff.Upstream.Lfs,
		TokenEnv:           eff.Upstream.TokenEnv != "",
		Follow:             nonNil(eff.Upstream.Follow),
		FollowIntervalSecs: int64(time.Duration(eff.Maintenance.FollowInterval) / time.Second),
	}
	body.Fields = diffFields(h.env.Cfg, eff)
	if seq, err := h.env.Repo.HeadSeq(ctx, id); err == nil {
		body.HeadSeq = seq
	}
	return body
}

// placementAllows answers whether this host serves/maintains the repo per the
// placement lists ("*" wildcard; exclude wins).
func placementAllows(allow, exclude []string, hostname string) bool {
	return listMatches(allow, hostname) && !listMatches(exclude, hostname)
}

func listMatches(list []string, hostname string) bool {
	if len(list) == 0 {
		return false
	}
	for _, pat := range list {
		if pat == "*" || pat == hostname {
			return true
		}
	}
	return false
}

// diffFields lists every overridden key of the four settings sections with
// its origin: {key, value, host_value, source:"host"|"setting"} (§11). Only
// overridden keys appear; durations render as seconds, sizes as bytes.
func diffFields(host, eff *config.Config) []FieldBody {
	out := []FieldBody{}
	if host == nil || eff == nil {
		return out
	}
	sections := []struct {
		name string
		a, b any
	}{
		{"bundles", host.Bundles, eff.Bundles},
		{"maintenance", host.Maintenance, eff.Maintenance},
		{"compaction", host.Compaction, eff.Compaction},
		{"upstream", host.Upstream, eff.Upstream},
	}
	for _, sec := range sections {
		va, vb := reflect.ValueOf(sec.a), reflect.ValueOf(sec.b)
		ta := va.Type()
		for i := 0; i < ta.NumField(); i++ {
			f := ta.Field(i)
			if f.Type.Kind() != reflect.Bool && f.Type.Kind() != reflect.Int64 && f.Type.Kind() != reflect.Int && f.Type.Kind() != reflect.String && f.Type.Kind() != reflect.Float64 {
				continue // lists/structs are not scalar overrides
			}
			av, bv := va.Field(i), vb.Field(i)
			if av.Equal(bv) {
				continue // not overridden
			}
			name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			out = append(out, FieldBody{
				Key:       sec.name + "." + name,
				Value:     scalarValue(bv, f.Type),
				HostValue: scalarValue(av, f.Type),
				Source:    "setting",
			})
		}
	}
	return out
}

func scalarValue(v reflect.Value, t reflect.Type) any {
	switch t {
	case reflect.TypeOf(config.Duration(0)):
		return int64(time.Duration(v.Int()) / time.Second)
	case reflect.TypeOf(config.ByteSize(0)):
		return v.Int()
	}
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int64:
		return v.Int()
	case reflect.Float64:
		return v.Float()
	default:
		return v.String()
	}
}
