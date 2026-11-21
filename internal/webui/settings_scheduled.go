// settings_scheduled.go — the Scheduled tasks tab's structured editors
// (WP9 G14) and the Settings → WAL panel (WP9 G16).
//
// The four per-repo tuning sections (bundles, maintenance, compaction,
// upstream) are ordinary knobs that used to be raw-TOML-only. Each is edited
// as a fieldset over the SAME stored document the config tab shows:
//
//   - values are read from the stored RepoSettings section when the document
//     overrides it, otherwise from the merged effective config (shown with an
//     "inherited" note);
//   - the section's Override checkbox is the only thing that makes a write
//     happen — unchecked means the section is deleted from the document, so
//     an untouched page re-publishes exactly the same sections;
//   - the form is coerced into config structs by the section types' own
//     UnmarshalText parsers (ByteSize/Duration), re-encoded, and validated
//     through config.ParseRepoSettings + ValidateAgainst (the settings PUT's
//     parse plus the /settings/validate rule) before api.PublishSettings
//     commits it; a rejection writes nothing.
//
// Bundle strategies use the repeated-row protocol established for /setup
// (internal/setup/rows.go): K#<i>.<tomlname> cells, K#<i>.__base for the
// stored row identity, K#<i>.__remove to drop a row, and one blank "new row"
// whose fully-empty cells are ignored. This file is the repo-settings twin of
// that editor, not a second idiom.
package webui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

// --- section metadata -------------------------------------------------------

type scheduledFieldSpec struct {
	key, label, typ, hint string
	enum                  []string
}

type scheduledSectionSpec struct {
	id, label, hint string
	fields          []scheduledFieldSpec
}

var scheduledSections = []scheduledSectionSpec{
	{id: "bundles", label: "Bundle strategies & packing",
		hint: "A present [bundles] section replaces the host bundles config wholesale, so enable the override only when you mean to pin every value here.",
		fields: []scheduledFieldSpec{
			{key: "min_commits", label: "Min commits", typ: "int", hint: "skip a bundle when the slot has fewer commits"},
			{key: "min_bytes", label: "Min bytes", typ: "size", hint: "64MiB"},
			{key: "main_only", label: "Main branch only", typ: "bool"},
			{key: "serve_via", label: "Serve via", typ: "select", enum: []string{"proxy", "signed_url"}},
			{key: "signed_url_ttl", label: "Signed URL TTL", typ: "duration", hint: "1h"},
			{key: "signed_url_for", label: "Signed URL for", typ: "list", hint: "comma-separated globs"},
			{key: "extra_refs", label: "Extra refs", typ: "list", hint: "comma-separated ref globs"},
			{key: "advertise", label: "Advertise bundle URIs", typ: "bool"},
			{key: "advertise_filtered", label: "Advertise filtered URIs", typ: "bool"},
			{key: "require", label: "Require features", typ: "list", hint: "comma-separated capability names"},
		}},
	{id: "maintenance", label: "Maintenance",
		hint: "A present [maintenance] section replaces the host maintenance config wholesale.",
		fields: []scheduledFieldSpec{
			{key: "interval", label: "Interval", typ: "duration", hint: "5m"},
			{key: "checkpoints", label: "Checkpoints", typ: "bool"},
			{key: "max_pack_bytes", label: "Max pack bytes", typ: "size", hint: "4GiB"},
			{key: "disk", label: "Disk", typ: "select", enum: []string{"tmpfs", "ssd"}},
			{key: "host", label: "Host", typ: "text", hint: "pin maintenance to one hostname"},
			{key: "fsck_interval", label: "Fsck interval", typ: "duration", hint: "24h"},
			{key: "follow_interval", label: "Upstream follow interval", typ: "duration", hint: "10m"},
		}},
	{id: "compaction", label: "Compaction",
		hint: "A present [compaction] section replaces the host compaction config wholesale.",
		fields: []scheduledFieldSpec{
			{key: "enabled", label: "Enabled", typ: "bool"},
			{key: "factor", label: "Factor", typ: "int", hint: "merge this many packs at once"},
			{key: "trigger_packs", label: "Trigger packs", typ: "int"},
			{key: "trigger_bytes", label: "Trigger bytes", typ: "size", hint: "512MiB"},
			{key: "lease_ttl", label: "Lease TTL", typ: "duration", hint: "5m"},
			{key: "retention_superseded", label: "Retention (superseded)", typ: "duration", hint: "168h"},
			{key: "engine", label: "Engine", typ: "select", enum: []string{"git"}},
		}},
	{id: "upstream", label: "Upstream",
		hint: "Upstream fields merge one by one: a blank value inherits the host config. token_env stays host-only.",
		fields: []scheduledFieldSpec{
			{key: "git", label: "Git upstream URL", typ: "text", hint: "https://example.com/acme/mono.git"},
			{key: "lfs", label: "LFS upstream URL", typ: "text"},
			{key: "follow", label: "Follow", typ: "list", hint: "comma-separated ref globs to follow"},
		}},
}

// strategyCells is the bundle-strategy row schema. Every cell is one toml
// field of config.BundleStrategy, so the row editor cannot invent a key the
// type does not define.
var strategyCells = []scheduledFieldSpec{
	{key: "name", label: "Name", typ: "text"},
	{key: "kind", label: "Kind", typ: "select", enum: []string{"full", "incremental"}},
	{key: "base", label: "Base", typ: "text"},
	{key: "schedule", label: "Schedule", typ: "text"},
	{key: "keep", label: "Keep", typ: "int"},
	{key: "backfill_max", label: "Backfill max", typ: "int"},
	{key: "min_commits", label: "Min commits", typ: "int"},
	{key: "chain", label: "Chain", typ: "bool"},
	{key: "filter", label: "Filter", typ: "text"},
	{key: "refs", label: "Refs", typ: "list"},
}

const strategyPrefix = "bundles.strategy"

// settingsScheduledSections projects the stored + effective config into the
// page model. form overlays posted values (the rejected-write re-render); nil
// renders the stored/effective state.
func settingsScheduledSections(stored *config.RepoSettings, eff *config.Config, form map[string]string) []SettingsSection {
	if eff == nil {
		return nil
	}
	out := make([]SettingsSection, 0, len(scheduledSections))

	bundlesStored := stored != nil && stored.Bundles != nil
	b := eff.Bundles
	if bundlesStored {
		b = *stored.Bundles
	}
	bundles := settingsSectionOf(scheduledSections[0], bundlesStored, form)
	bundles.Rows = settingsStrategyRows(b.Strategy, form)
	bundles.RowPrefix = strategyPrefix
	for _, f := range scheduledSections[0].fields {
		bundles.Fields = append(bundles.Fields, settingsFieldOf("bundles", f, settingsBundlesValue(b, f.key), form))
	}
	out = append(out, bundles)

	maintenanceStored := stored != nil && stored.Maintenance != nil
	m := eff.Maintenance
	if maintenanceStored {
		m = *stored.Maintenance
	}
	maintenance := settingsSectionOf(scheduledSections[1], maintenanceStored, form)
	for _, f := range scheduledSections[1].fields {
		maintenance.Fields = append(maintenance.Fields, settingsFieldOf("maintenance", f, settingsMaintenanceValue(m, f.key), form))
	}
	out = append(out, maintenance)

	compactionStored := stored != nil && stored.Compaction != nil
	c := eff.Compaction
	if compactionStored {
		c = *stored.Compaction
	}
	compaction := settingsSectionOf(scheduledSections[2], compactionStored, form)
	for _, f := range scheduledSections[2].fields {
		compaction.Fields = append(compaction.Fields, settingsFieldOf("compaction", f, settingsCompactionValue(c, f.key), form))
	}
	out = append(out, compaction)

	upstreamStored := stored != nil && stored.Upstream != nil
	u := eff.Upstream
	if upstreamStored {
		u = config.Upstream{Git: stored.Upstream.Git, Lfs: stored.Upstream.Lfs, Follow: stored.Upstream.Follow}
	}
	upstream := settingsSectionOf(scheduledSections[3], upstreamStored, form)
	for _, f := range scheduledSections[3].fields {
		upstream.Fields = append(upstream.Fields, settingsFieldOf("upstream", f, settingsUpstreamValue(u, f.key), form))
	}
	out = append(out, upstream)
	return out
}

func settingsSectionOf(spec scheduledSectionSpec, override bool, form map[string]string) SettingsSection {
	sec := SettingsSection{
		ID: spec.id, Label: spec.label, Hint: spec.hint,
		Override: override, Inherited: !override,
		FieldErrors: map[string]string{},
	}
	if form != nil {
		sec.Override = form[spec.id+".override"] == "true"
		sec.Inherited = !sec.Override
	}
	return sec
}

func settingsFieldOf(section string, spec scheduledFieldSpec, def string, form map[string]string) SettingsField {
	name := section + "." + spec.key
	value := def
	if form != nil {
		if raw, ok := form[name]; ok {
			value = raw
		}
	}
	return SettingsField{
		Name: name, Label: spec.label, Type: spec.typ, Value: value,
		Enum: spec.enum, Hint: spec.hint,
	}
}

func settingsBundlesValue(b config.Bundles, key string) string {
	switch key {
	case "min_commits":
		return strconv.Itoa(b.MinCommits)
	case "min_bytes":
		return b.MinBytes.String()
	case "main_only":
		return strconv.FormatBool(b.MainOnly)
	case "serve_via":
		return b.ServeVia
	case "signed_url_ttl":
		return fmtSpecDuration(b.SignedURLTTL.String())
	case "signed_url_for":
		return strings.Join(b.SignedURLFor, ", ")
	case "extra_refs":
		return strings.Join(b.ExtraRefs, ", ")
	case "advertise":
		return strconv.FormatBool(b.Advertise)
	case "advertise_filtered":
		return strconv.FormatBool(b.AdvertiseFiltered)
	case "require":
		return strings.Join(b.Require, ", ")
	}
	return ""
}

func settingsMaintenanceValue(m config.Maintenance, key string) string {
	switch key {
	case "interval":
		return fmtSpecDuration(m.Interval.String())
	case "checkpoints":
		return strconv.FormatBool(m.Checkpoints)
	case "max_pack_bytes":
		return m.MaxPackBytes.String()
	case "disk":
		return m.Disk
	case "host":
		return m.Host
	case "fsck_interval":
		return fmtSpecDuration(m.FsckInterval.String())
	case "follow_interval":
		return fmtSpecDuration(m.FollowInterval.String())
	}
	return ""
}

func settingsCompactionValue(c config.Compaction, key string) string {
	switch key {
	case "enabled":
		return strconv.FormatBool(c.Enabled)
	case "factor":
		return strconv.Itoa(c.Factor)
	case "trigger_packs":
		return strconv.Itoa(c.TriggerPacks)
	case "trigger_bytes":
		return c.TriggerBytes.String()
	case "lease_ttl":
		return fmtSpecDuration(c.LeaseTTL.String())
	case "retention_superseded":
		return fmtSpecDuration(c.RetentionSuperseded.String())
	case "engine":
		return c.Engine
	}
	return ""
}

func settingsUpstreamValue(u config.Upstream, key string) string {
	switch key {
	case "git":
		return u.Git
	case "lfs":
		return u.Lfs
	case "follow":
		return strings.Join(u.Follow, ", ")
	}
	return ""
}

// settingsStrategyRows renders the reusable strategy rows plus one blank add
// row. With form != nil the rows come from the posted cells (the rejected
// write re-render), preserving the operator's edits and row identities.
func settingsStrategyRows(strategies []config.BundleStrategy, form map[string]string) []SettingsRow {
	if form != nil {
		return settingsStrategyRowsFromForm(form)
	}
	rows := make([]SettingsRow, 0, len(strategies)+1)
	for i, s := range strategies {
		rows = append(rows, settingsStrategyRow(i, i, "", settingsStrategyValues(s)))
	}
	rows = append(rows, settingsStrategyRow(len(strategies), -1, "new row", nil))
	return rows
}

func settingsStrategyValues(s config.BundleStrategy) map[string]string {
	return map[string]string{
		"name": s.Name, "kind": s.Kind, "base": s.Base, "schedule": s.Schedule,
		"keep": strconv.Itoa(s.Keep), "backfill_max": strconv.Itoa(s.BackfillMax),
		"min_commits": strconv.Itoa(s.MinCommits), "chain": strconv.FormatBool(s.Chain),
		"filter": s.Filter, "refs": strings.Join(s.Refs, ", "),
	}
}

func settingsStrategyRow(index, base int, caption string, values map[string]string) SettingsRow {
	row := SettingsRow{
		Index: index, Base: base, Caption: caption,
		BaseKey:   strategyRowBaseKey(index),
		RemoveKey: strategyRowRemoveKey(index),
	}
	if caption == "" && values != nil {
		row.Caption = values["name"]
	}
	if row.Caption == "" && base < 0 {
		row.Caption = "new row"
	}
	for _, cell := range strategyCells {
		row.Cells = append(row.Cells, SettingsRowCell{
			Name: strategyRowCellKey(index, cell.key), Label: cell.label,
			Type: cell.typ, Value: values[cell.key], Enum: cell.enum,
		})
	}
	return row
}

func strategyRowCellKey(i int, name string) string {
	return fmt.Sprintf("%s#%d.%s", strategyPrefix, i, name)
}

func strategyRowBaseKey(i int) string { return fmt.Sprintf("%s#%d.__base", strategyPrefix, i) }

func strategyRowRemoveKey(i int) string { return fmt.Sprintf("%s#%d.__remove", strategyPrefix, i) }

// settingsStrategyRowsFromForm reconstructs rows from the flat posted values.
func settingsStrategyRowsFromForm(form map[string]string) []SettingsRow {
	indexes := map[int]bool{}
	for k := range form {
		if !strings.HasPrefix(k, strategyPrefix+"#") {
			continue
		}
		rest := strings.TrimPrefix(k, strategyPrefix+"#")
		dot := strings.IndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		if i, err := strconv.Atoi(rest[:dot]); err == nil && i >= 0 {
			indexes[i] = true
		}
	}
	if len(indexes) == 0 {
		return []SettingsRow{settingsStrategyRow(0, -1, "new row", nil)}
	}
	max := -1
	for i := range indexes {
		if i > max {
			max = i
		}
	}
	rows := make([]SettingsRow, 0, max+1)
	for i := 0; i <= max; i++ {
		if !indexes[i] {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(form[strategyRowRemoveKey(i)]), "true") {
			continue
		}
		base := -1
		if raw, ok := form[strategyRowBaseKey(i)]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 0 {
				base = n
			}
		}
		cells := map[string]string{}
		for _, cell := range strategyCells {
			cells[cell.key] = form[strategyRowCellKey(i, cell.key)]
		}
		rows = append(rows, settingsStrategyRow(i, base, "", cells))
	}
	return rows
}

// --- write path -------------------------------------------------------------

// writeScheduled publishes the four tuning sections over the stored document.
// It never mutates another section: non-overridden sections are removed from
// the document map and the rest of the TOML (description, features, review,
// integrations) round-trips through the map unchanged.
func (s SettingsServices) writeScheduled(ctx context.Context, owner, repo string, p auth.Principal, w SettingsRepoWrite) (SettingsWriteResult, *SettingsWriteError) {
	if s.API == nil || s.API.Repo == nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusServiceUnavailable, "settings store not configured")
	}
	// An empty POST is not "uncheck everything": the rendered form carries
	// this marker, so a stray submit can never silently drop all four
	// overrides.
	if w.Get("scheduled_form") != "1" {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "scheduled settings form is incomplete — reload the page")
	}
	id := s.repoID(owner, repo)
	doc, _, _, err := api.SettingsDocument(ctx, s.API, id)
	if err != nil {
		return SettingsWriteResult{}, settingsWriteErr(settingsRepoErrStatus(err), err.Error())
	}
	raw := map[string]any{}
	if strings.TrimSpace(doc.TOML) != "" {
		if _, derr := toml.Decode(doc.TOML, &raw); derr != nil {
			return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "the stored settings document does not parse: "+derr.Error())
		}
	}
	if w.Get("bundles.override") == "true" {
		bundles, werr := settingsBundlesFromForm(w.Values)
		if werr != nil {
			return SettingsWriteResult{}, werr
		}
		raw["bundles"] = bundles
	} else {
		delete(raw, "bundles")
	}
	if w.Get("maintenance.override") == "true" {
		maintenance, werr := settingsMaintenanceFromForm(w.Values)
		if werr != nil {
			return SettingsWriteResult{}, werr
		}
		raw["maintenance"] = maintenance
	} else {
		delete(raw, "maintenance")
	}
	if w.Get("compaction.override") == "true" {
		compaction, werr := settingsCompactionFromForm(w.Values)
		if werr != nil {
			return SettingsWriteResult{}, werr
		}
		raw["compaction"] = compaction
	} else {
		delete(raw, "compaction")
	}
	if w.Get("upstream.override") == "true" {
		upstream, werr := settingsUpstreamFromForm(w.Values)
		if werr != nil {
			return SettingsWriteResult{}, werr
		}
		raw["upstream"] = upstream
	} else {
		delete(raw, "upstream")
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return SettingsWriteResult{}, settingsWriteErr(http.StatusUnprocessableEntity, "encode settings: "+err.Error())
	}
	body := buf.Bytes()
	rs, perr := config.ParseRepoSettings(body)
	if perr != nil {
		return SettingsWriteResult{}, settingsWriteErrField(http.StatusUnprocessableEntity, settingsValidationField(perr.Error()), perr.Error())
	}
	if s.API.Cfg != nil {
		if verr := rs.ValidateAgainst(s.API.Cfg); verr != nil {
			return SettingsWriteResult{}, settingsWriteErrField(http.StatusUnprocessableEntity, settingsValidationField(verr.Error()), verr.Error())
		}
	}
	rev, werr := api.PublishSettings(ctx, s.API, id, body, "scheduled settings", p.Name)
	if werr != nil {
		return SettingsWriteResult{}, settingsWriteErr(werr.Status, werr.Message)
	}
	return SettingsWriteResult{Notice: "scheduled settings published at #" + strconv.FormatUint(rev, 10)}, nil
}

func settingsBundlesFromForm(v map[string]string) (config.Bundles, *SettingsWriteError) {
	b := config.Bundles{}
	if werr := settingsIntValue(v, "bundles.min_commits", &b.MinCommits); werr != nil {
		return b, werr
	}
	if werr := settingsSizeValue(v, "bundles.min_bytes", &b.MinBytes); werr != nil {
		return b, werr
	}
	if werr := settingsBoolValue(v, "bundles.main_only", &b.MainOnly); werr != nil {
		return b, werr
	}
	b.ServeVia = strings.TrimSpace(v["bundles.serve_via"])
	if werr := settingsDurationValue(v, "bundles.signed_url_ttl", &b.SignedURLTTL); werr != nil {
		return b, werr
	}
	b.SignedURLFor = settingsListValue(v["bundles.signed_url_for"])
	b.ExtraRefs = settingsListValue(v["bundles.extra_refs"])
	if werr := settingsBoolValue(v, "bundles.advertise", &b.Advertise); werr != nil {
		return b, werr
	}
	if werr := settingsBoolValue(v, "bundles.advertise_filtered", &b.AdvertiseFiltered); werr != nil {
		return b, werr
	}
	b.Require = settingsListValue(v["bundles.require"])
	strategies, werr := settingsStrategyFromForm(v)
	if werr != nil {
		return b, werr
	}
	b.Strategy = strategies
	return b, nil
}

func settingsMaintenanceFromForm(v map[string]string) (config.Maintenance, *SettingsWriteError) {
	m := config.Maintenance{}
	if werr := settingsDurationValue(v, "maintenance.interval", &m.Interval); werr != nil {
		return m, werr
	}
	if werr := settingsBoolValue(v, "maintenance.checkpoints", &m.Checkpoints); werr != nil {
		return m, werr
	}
	if werr := settingsSizeValue(v, "maintenance.max_pack_bytes", &m.MaxPackBytes); werr != nil {
		return m, werr
	}
	m.Disk = strings.TrimSpace(v["maintenance.disk"])
	m.Host = strings.TrimSpace(v["maintenance.host"])
	if werr := settingsDurationValue(v, "maintenance.fsck_interval", &m.FsckInterval); werr != nil {
		return m, werr
	}
	if werr := settingsDurationValue(v, "maintenance.follow_interval", &m.FollowInterval); werr != nil {
		return m, werr
	}
	return m, nil
}

func settingsCompactionFromForm(v map[string]string) (config.Compaction, *SettingsWriteError) {
	c := config.Compaction{}
	if werr := settingsBoolValue(v, "compaction.enabled", &c.Enabled); werr != nil {
		return c, werr
	}
	if werr := settingsIntValue(v, "compaction.factor", &c.Factor); werr != nil {
		return c, werr
	}
	if werr := settingsIntValue(v, "compaction.trigger_packs", &c.TriggerPacks); werr != nil {
		return c, werr
	}
	if werr := settingsSizeValue(v, "compaction.trigger_bytes", &c.TriggerBytes); werr != nil {
		return c, werr
	}
	if werr := settingsDurationValue(v, "compaction.lease_ttl", &c.LeaseTTL); werr != nil {
		return c, werr
	}
	if werr := settingsDurationValue(v, "compaction.retention_superseded", &c.RetentionSuperseded); werr != nil {
		return c, werr
	}
	c.Engine = strings.TrimSpace(v["compaction.engine"])
	return c, nil
}

func settingsUpstreamFromForm(v map[string]string) (config.Upstream, *SettingsWriteError) {
	return config.Upstream{
		Git:    strings.TrimSpace(v["upstream.git"]),
		Lfs:    strings.TrimSpace(v["upstream.lfs"]),
		Follow: settingsListValue(v["upstream.follow"]),
	}, nil
}

// settingsStrategyFromForm rebuilds the strategy slice in posted row order.
// A row whose non-boolean cells are all empty is the add-row placeholder and
// is ignored; removed rows are dropped.
func settingsStrategyFromForm(v map[string]string) ([]config.BundleStrategy, *SettingsWriteError) {
	rows := settingsStrategyRowsFromForm(v)
	out := make([]config.BundleStrategy, 0, len(rows))
	for _, row := range rows {
		values := map[string]string{}
		for _, cell := range row.Cells {
			values[cellKeyName(cell.Name)] = v[cell.Name]
		}
		if scheduledRowBlank(values) {
			continue
		}
		var s config.BundleStrategy
		s.Name = strings.TrimSpace(values["name"])
		s.Kind = strings.TrimSpace(values["kind"])
		s.Base = strings.TrimSpace(values["base"])
		s.Schedule = strings.TrimSpace(values["schedule"])
		keep, werr := settingsIntCell(values, row.Index, "keep")
		if werr != nil {
			return nil, werr
		}
		s.Keep = keep
		backfill, werr := settingsIntCell(values, row.Index, "backfill_max")
		if werr != nil {
			return nil, werr
		}
		s.BackfillMax = backfill
		minCommits, werr := settingsIntCell(values, row.Index, "min_commits")
		if werr != nil {
			return nil, werr
		}
		s.MinCommits = minCommits
		s.Chain = strings.EqualFold(strings.TrimSpace(values["chain"]), "true")
		s.Filter = strings.TrimSpace(values["filter"])
		s.Refs = settingsListValue(values["refs"])
		out = append(out, s)
	}
	return out, nil
}

// settingsIntCell parses one integer cell; blank is zero, a bad value names
// the exact cell the operator must fix.
func settingsIntCell(values map[string]string, row int, name string) (int, *SettingsWriteError) {
	raw := strings.TrimSpace(values[name])
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, settingsWriteErrField(http.StatusUnprocessableEntity, strategyRowCellKey(row, name), name+" must be an integer")
	}
	return n, nil
}

func cellKeyName(formName string) string {
	if i := strings.LastIndexByte(formName, '.'); i >= 0 {
		return formName[i+1:]
	}
	return formName
}

func scheduledRowBlank(values map[string]string) bool {
	for _, cell := range strategyCells {
		raw := strings.TrimSpace(values[cell.key])
		if raw == "" {
			continue
		}
		if cell.typ == "bool" && !strings.EqualFold(raw, "true") {
			continue
		}
		return false
	}
	return true
}

// --- coercion (the config types' own parsers) -------------------------------

func settingsIntValue(v map[string]string, name string, dst *int) *SettingsWriteError {
	raw := strings.TrimSpace(v[name])
	if raw == "" {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, "a value is required when this section is overridden")
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, "must be an integer")
	}
	*dst = n
	return nil
}

func settingsBoolValue(v map[string]string, name string, dst *bool) *SettingsWriteError {
	raw := strings.TrimSpace(v[name])
	switch strings.ToLower(raw) {
	case "true", "1", "on":
		*dst = true
	case "false", "0", "off", "":
		*dst = false
	default:
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, "must be true or false")
	}
	return nil
}

func settingsSizeValue(v map[string]string, name string, dst *config.ByteSize) *SettingsWriteError {
	raw := strings.TrimSpace(v[name])
	if raw == "" {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, "a size is required when this section is overridden")
	}
	if err := dst.UnmarshalText([]byte(raw)); err != nil {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, err.Error())
	}
	return nil
}

func settingsDurationValue(v map[string]string, name string, dst *config.Duration) *SettingsWriteError {
	raw := strings.TrimSpace(v[name])
	if raw == "" {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, "a duration is required when this section is overridden")
	}
	if err := dst.UnmarshalText([]byte(raw)); err != nil {
		return settingsWriteErrField(http.StatusUnprocessableEntity, name, err.Error())
	}
	return nil
}

func settingsListValue(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func settingsWriteErrField(status int, field, msg string) *SettingsWriteError {
	return &SettingsWriteError{Status: status, Field: field, Message: msg}
}

// settingsValidationField maps a config validation message onto the form cell
// it names: "bundles.strategy[0].name" → "bundles.strategy#0.name", else the
// section id (the card-level error). Unknown shapes stay keyless.
func settingsValidationField(msg string) string {
	if i := strings.Index(msg, "bundles.strategy["); i >= 0 {
		rest := msg[i+len("bundles.strategy["):]
		j := strings.IndexByte(rest, ']')
		if j > 0 {
			if n, err := strconv.Atoi(rest[:j]); err == nil && strings.HasPrefix(rest[j:], "].") {
				tail := rest[j+2:]
				end := strings.IndexAny(tail, " :,;")
				if end < 0 {
					end = len(tail)
				}
				if end > 0 {
					return strategyRowCellKey(n, tail[:end])
				}
			}
		}
	}
	for _, id := range []string{"bundles", "maintenance", "compaction", "upstream"} {
		if i := strings.Index(msg, id+"."); i >= 0 {
			rest := msg[i:]
			end := strings.IndexAny(rest, " :,;)\"")
			if end < 0 {
				end = len(rest)
			}
			return rest[:end]
		}
	}
	return ""
}

// settingsOverlayScheduledForm re-renders a rejected Scheduled write with the
// operator's posted values (nothing written, the offending row marked).
func settingsOverlayScheduledForm(page *SettingsRepoPage, w SettingsRepoWrite, werr *SettingsWriteError) {
	if page.Scheduled == nil {
		return
	}
	page.Scheduled.Sections = settingsScheduledSectionsFromPage(page.Scheduled, w.Values)
	if werr != nil {
		settingsMarkScheduledError(page.Scheduled.Sections, werr.Field, werr.Message)
	}
}

// settingsMarkScheduledError attaches the rejected message to the named cell
// (or the card when the name is a section id).
func settingsMarkScheduledError(sections []SettingsSection, field, msg string) {
	if field == "" {
		return
	}
	for i := range sections {
		sec := &sections[i]
		if field == sec.ID {
			sec.Error = msg
			return
		}
		if sec.FieldErrors == nil {
			sec.FieldErrors = map[string]string{}
		}
		for j := range sec.Fields {
			if sec.Fields[j].Name == field {
				sec.FieldErrors[field] = msg
				return
			}
		}
		for j := range sec.Rows {
			for k := range sec.Rows[j].Cells {
				if sec.Rows[j].Cells[k].Name == field {
					sec.FieldErrors[field] = msg
					return
				}
			}
		}
	}
}

// settingsScheduledSectionsFromPage rebuilds section editors from the page's
// loaded config plus the posted overlay. The stored/effective split is already
// encoded in the page model, so this only overlays values.
func settingsScheduledSectionsFromPage(page *SettingsScheduled, form map[string]string) []SettingsSection {
	sections := page.Sections
	for i := range sections {
		sec := &sections[i]
		sec.Override = form[sec.ID+".override"] == "true"
		sec.Inherited = !sec.Override
		for j := range sec.Fields {
			if raw, ok := form[sec.Fields[j].Name]; ok {
				sec.Fields[j].Value = raw
			}
		}
		if sec.RowPrefix != "" {
			sec.Rows = settingsStrategyRowsFromForm(form)
		}
	}
	return sections
}
